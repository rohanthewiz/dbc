package ai

import (
	"fmt"
	"strings"
)

// What the assistant is told about the query and its result.
//
// THE DATA RULE. The query text and the last error always go: they are what
// the user is asking about, and they are the user's own words. Result ROWS
// are the database's contents — customer names, salaries, whatever the table
// holds — and sending them to a hosted model is a decision about that data,
// not about the question. So rows go only when the connection says
// ai_rows = true, and never more than ai_context_rows of them. The default is
// off, and every turn tells the user what went and, when rows did not, the
// exact setting that would send them. Nothing is attached silently, in
// either direction.
//
// SCHEMA IS NOT DATA. The names and declared types of the tables a question
// mentions are the database's shape, not its contents — the same thing a
// CREATE TABLE in the repo would show — so they go without ai_rows. They are
// what turns "SELECT owner FROM cats" (a guessed column) into
// "SELECT owner_id FROM cats" (the real one).
//
// HIDDEN COLUMNS STAY HIDDEN. A column hidden in the grid is left out of the
// result the model sees, as it is left out of every copy and export: hiding
// is how a user says "not this one", and a salary or email column is
// exactly what gets hidden before a result is shared. Hiding it and then
// seeing it go to a hosted model would be a surprise, while the opposite
// mistake costs one keypress (+ shows all, ask again). The hidden columns'
// NAMES still go, since names are schema: the model can then say "the
// answer is probably in api_key, which you hid" instead of reasoning as if
// the column did not exist.
//
// ROWS GO IN THE GRID'S ORDER. After a header sort, "the first 10 rows" the
// user sees are the top 10 by that column, and "why is the top one so
// high?" is about the top one on screen. So the rows sent follow the grid's
// sort, as copies do, and the model is told the order is the grid's, not
// the query's: otherwise it would read "sorted by price" into the SQL's
// ORDER BY (or its absence) and explain an ordering the query never asked
// for.

// DefaultContextRows is how many result rows go with a question when the
// connection allows rows and ai_context_rows is not set.
const DefaultContextRows = 10

// maxSchemaColumns caps how many columns of one table are described. A wide
// table (hundreds of columns is not rare in a warehouse) would otherwise be
// the whole prompt; the model is told how many were left out.
const maxSchemaColumns = 80

// maxCellRunes caps one value in the attached rows. A single JSON or TEXT
// column can be megabytes; ten of them would crowd the question out of the
// model's context and cost the user tokens to say nothing.
const maxCellRunes = 200

// Context is the state of the editor and results at the moment of asking.
type Context struct {
	Conn   string // connection name
	Driver string // postgres | mysql | sqlite | bytdb — the model needs the dialect

	Query string // the statement the question is about; "" if none
	Err   string // the error the last run of it failed with, if it failed

	// The last result. Columns nil means there is none (never run, or an
	// exec with nothing to show).
	Columns   []string
	Rows      [][]string
	Truncated bool // the fetch hit max_rows, so len(Rows) is not the table's size

	// Hidden lists the result columns (indices into Columns, ascending)
	// hidden in the grid. Build leaves their values out and names them. The
	// full rows are passed rather than a projected copy because the context
	// chip builds a Context every frame, and Build only projects the handful
	// of rows it actually sends.
	Hidden []int

	// Order is the grid's row order after a header sort: Order[i] is the
	// result row shown i-th. It may hold only a prefix — the rows that
	// could be sent — since the chip builds a Context every frame and a
	// full copy of a 10,000-row permutation per frame buys nothing. nil
	// means the result's own order. SortedBy names the sorted column (""
	// when unsorted) and SortDesc its direction.
	Order    []int
	SortedBy string
	SortDesc bool

	// SendRows is the connection's ai_rows opt-in; MaxRows is ai_context_rows.
	SendRows bool
	MaxRows  int

	// Plan is the query plan the user explained for Query, rendered as text
	// with dbc's findings ("" when there is none). A plan is how the
	// database will run the statement — table and index names, row counts
	// and timings — not what the rows hold, so like the schema it goes on
	// every connection. The literals it quotes in its conditions are the
	// user's own, from the SQL that is sent anyway.
	Plan string

	// Tables are the catalog's tables the query or question mentions. A
	// table with no Columns is still named in the Note — that is how the
	// context chip forecasts "schema of cats" before the columns have been
	// looked up — but is left out of the prompt text, so a caller building
	// the real prompt drops the ones the lookup could not describe.
	Tables []Table
}

// Table is one table's shape: its name as the user would write it, and its
// columns in declared order.
type Table struct {
	Name    string
	View    bool
	Columns []Column
}

// Column is a column's name and declared type ("" when the database does
// not say, as for a SQLite view's computed column).
type Column struct {
	Name string
	Type string
}

// Prompt is a question ready to send, and the line the transcript shows about
// what went with it.
type Prompt struct {
	Text string
	Note string
}

// preamble frames every conversation's first message the same way. The fenced
// ```sql convention is load-bearing: the chat pane offers "insert into the
// editor" on exactly those blocks.
const preamble = "You are the SQL assistant inside dbc, a terminal database client. " +
	"Answer concisely. When you suggest SQL, put each statement in a fenced ```sql block " +
	"written for the connection's dialect, so the user can insert it into their editor."

// Build renders a question plus its context as one prompt. first says
// whether this is the first turn of the conversation, which is the only one
// that carries the preamble — the agent keeps the session's history, so
// repeating it would cost tokens every turn for nothing.
func Build(question string, ctx Context, first bool) Prompt {
	var sb strings.Builder
	var sent []string // what the note will list

	if first {
		sb.WriteString(preamble)
		sb.WriteString("\n\n")
	}
	if ctx.Conn != "" {
		fmt.Fprintf(&sb, "Connection %q uses the %s driver.\n\n", ctx.Conn, dialectName(ctx.Driver))
	}
	if len(ctx.Tables) > 0 {
		if schema := schemaText(ctx.Tables); schema != "" {
			sb.WriteString(schema)
			sb.WriteString("\n")
		}
		sent = append(sent, schemaNote(ctx.Tables))
	}
	if q := strings.TrimSpace(ctx.Query); q != "" {
		sb.WriteString("The SQL in question:\n```sql\n")
		sb.WriteString(q)
		sb.WriteString("\n```\n\n")
		sent = append(sent, "query")
	}
	if pl := strings.TrimSpace(ctx.Plan); pl != "" {
		sb.WriteString("Its query plan, as dbc summarized it (each step's own share of the time or cost, " +
			"with dbc's automatic findings after it):\n```\n")
		sb.WriteString(pl)
		sb.WriteString("\n```\n\n")
		sent = append(sent, "plan")
	}
	if e := strings.TrimSpace(ctx.Err); e != "" {
		sb.WriteString("Running it failed with:\n```\n")
		sb.WriteString(e)
		sb.WriteString("\n```\n\n")
		sent = append(sent, "error")
	}

	withheld := ""
	if ctx.Columns != nil && ctx.Err == "" {
		n := rowsToSend(ctx)
		shown, hidden := splitHidden(ctx)
		switch {
		case n > 0:
			picked := pickRows(ctx, n)
			n = len(picked) // a malformed Order can only shrink it
			if ctx.SortedBy != "" {
				sb.WriteString(sortText(ctx))
			}
			fmt.Fprintf(&sb, "%s:\n", rowsHeading(n, ctx))
			sb.WriteString(markdownRows(names(ctx.Columns, shown), project(picked, shown)))
			sb.WriteString("\n")
			sb.WriteString(hiddenText(ctx.Columns, hidden))

			// the parenthetical says how the rows differ from the query's
			// own result; its wording matches the copy log's —
			// "the result (8 rows, 1 column hidden)"
			var how []string
			if ctx.SortedBy != "" {
				how = append(how, "sorted by "+ctx.SortedBy+" "+pick(ctx.SortDesc, "desc", "asc"))
			}
			if len(hidden) > 0 {
				how = append(how, fmt.Sprintf("%d column%s hidden", len(hidden), pluralS(len(hidden))))
			}
			rows := fmt.Sprintf("%d of %s rows", n, totalRows(ctx))
			if len(how) > 0 {
				rows += " (" + strings.Join(how, ", ") + ")"
			}
			sent = append(sent, rows)
		default:
			// Column names are schema, not contents, so they go even when
			// rows do not: "why is price a string?" needs them. No values
			// go here, so hiding changes nothing the chip need mention;
			// the model is still told which columns the user hid, to read
			// the question the way the user sees the grid. A sort is not
			// mentioned: with no rows sent, there is no order to explain.
			fmt.Fprintf(&sb, "Its result has the columns: %s (%s rows; the values are not shared).\n\n",
				strings.Join(names(ctx.Columns, shown), ", "), totalRows(ctx))
			sb.WriteString(hiddenText(ctx.Columns, hidden))
			sent = append(sent, "column names")
			if !ctx.SendRows && len(ctx.Rows) > 0 {
				withheld = fmt.Sprintf("rows not sent — set ai_rows = true on connection %q to include them",
					ctx.Conn)
			}
		}
	}

	sb.WriteString("Question: ")
	sb.WriteString(strings.TrimSpace(question))

	note := "sent: question only"
	if len(sent) > 0 {
		note = "sent: " + strings.Join(sent, ", ")
	}
	if withheld != "" {
		note += " · " + withheld
	}
	return Prompt{Text: sb.String(), Note: note}
}

// rowsToSend is how many rows the data rule allows for this context.
func rowsToSend(ctx Context) int {
	if !ctx.SendRows || ctx.MaxRows <= 0 {
		return 0
	}
	return min(ctx.MaxRows, len(ctx.Rows))
}

// rowsHeading says what slice of the result the model is looking at, so it
// does not mistake ten rows for the whole table and draw conclusions from
// them ("there are only 10 customers").
func rowsHeading(n int, ctx Context) string {
	if n == len(ctx.Rows) && !ctx.Truncated {
		return fmt.Sprintf("Its complete result (%d rows)", n)
	}
	return fmt.Sprintf("The first %d of its %s result rows", n, totalRows(ctx))
}

// totalRows is the row count as honestly as dbc knows it.
func totalRows(ctx Context) string {
	if ctx.Truncated {
		return fmt.Sprintf("%d+", len(ctx.Rows))
	}
	return fmt.Sprint(len(ctx.Rows))
}

// pickRows returns the first n rows in the grid's order, or the result's
// when there is no Order. Entries of Order that are out of range are
// skipped rather than trusted, and an Order shorter than n yields fewer
// rows, not result-order ones mixed in.
func pickRows(ctx Context, n int) [][]string {
	if ctx.Order == nil {
		return ctx.Rows[:n]
	}
	out := make([][]string, 0, n)
	for _, ri := range ctx.Order {
		if len(out) == n {
			break
		}
		if ri >= 0 && ri < len(ctx.Rows) {
			out = append(out, ctx.Rows[ri])
		}
	}
	return out
}

// sortText tells the model the rows are in the grid's order. NULLs last
// is said because it holds in both directions, which is not what every
// database does with ORDER BY … DESC.
func sortText(ctx Context) string {
	return fmt.Sprintf("The user sorted the result in the grid by %s, %s (NULLs last), "+
		"so the rows below are in that order, not the query's.\n",
		ctx.SortedBy, pick(ctx.SortDesc, "descending", "ascending"))
}

// maxHiddenNames caps how many hidden columns are named. Hiding most of a
// warehouse table's 300 columns is a reasonable thing to do, and naming all
// of them would be the prompt.
const maxHiddenNames = 40

// splitHidden divides the result's columns into the shown and the hidden,
// both as ascending indices into ctx.Columns. Out-of-range or repeated
// entries in ctx.Hidden are ignored rather than trusted. Hiding every column
// is treated as hiding none: the grid refuses to, and a result reduced to
// nothing would read to the model as an empty one.
func splitHidden(ctx Context) (shown, hidden []int) {
	isHidden := make([]bool, len(ctx.Columns))
	for _, c := range ctx.Hidden {
		if c >= 0 && c < len(isHidden) {
			isHidden[c] = true
		}
	}
	for c, h := range isHidden {
		if h {
			hidden = append(hidden, c)
		} else {
			shown = append(shown, c)
		}
	}
	if len(shown) == 0 {
		return hidden, nil
	}
	return shown, hidden
}

// names picks the column names at idx.
func names(cols []string, idx []int) []string {
	out := make([]string, len(idx))
	for i, c := range idx {
		out[i] = cols[c]
	}
	return out
}

// project cuts rows down to the columns at idx. A short row (fewer values
// than columns) gets "" rather than a panic.
func project(rows [][]string, idx []int) [][]string {
	out := make([][]string, len(rows))
	for r, row := range rows {
		vals := make([]string, len(idx))
		for i, c := range idx {
			if c < len(row) {
				vals[i] = row[c]
			}
		}
		out[r] = vals
	}
	return out
}

// hiddenText tells the model which columns the user hid, so it does not
// take the result above for the whole of it; "" when none are.
func hiddenText(cols []string, hidden []int) string {
	if len(hidden) == 0 {
		return ""
	}
	ns := names(cols, hidden[:min(len(hidden), maxHiddenNames)])
	list := strings.Join(ns, ", ")
	if n := len(hidden) - len(ns); n > 0 {
		list += fmt.Sprintf(", … and %d more", n)
	}
	return fmt.Sprintf("The user hid %s in the grid, so %s left out above: %s.\n\n",
		pick(len(hidden) == 1, "this column", "these columns"),
		pick(len(hidden) == 1, "it is", "they are"), list)
}

func pluralS(n int) string { return pick(n == 1, "", "s") }

func pick(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// markdownRows renders rows as a compact Markdown table, which models read
// reliably and which costs fewer tokens than JSON's repeated keys.
func markdownRows(cols []string, rows [][]string) string {
	cell := func(s string) string {
		s = strings.NewReplacer("|", `\|`, "\r\n", " ", "\n", " ").Replace(s)
		if r := []rune(s); len(r) > maxCellRunes {
			s = string(r[:maxCellRunes-1]) + "…"
		}
		return s
	}
	var sb strings.Builder
	sb.WriteString("|")
	for _, c := range cols {
		sb.WriteString(" " + cell(c) + " |")
	}
	sb.WriteString("\n|")
	for range cols {
		sb.WriteString(" --- |")
	}
	sb.WriteString("\n")
	for _, row := range rows {
		sb.WriteString("|")
		for _, v := range row {
			sb.WriteString(" " + cell(v) + " |")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// schemaText renders the tables' columns, one line per table:
//
//	Tables involved, from the database's catalog (columns in declared order):
//	- cats: id integer, name text, owner_id integer
//	- old_cats (view): id integer, name text
//
// One line each keeps a dozen tables to a dozen lines; the heading's "from
// the catalog" is what tells the model these names are real, not a guess of
// dbc's it may improve on.
func schemaText(tables []Table) string {
	var sb strings.Builder
	for _, t := range tables {
		if len(t.Columns) == 0 {
			continue
		}
		if sb.Len() == 0 {
			sb.WriteString("Tables involved, from the database's catalog (columns in declared order):\n")
		}
		sb.WriteString("- " + t.Name)
		if t.View {
			sb.WriteString(" (view)")
		}
		sb.WriteString(": ")
		cols := t.Columns[:min(len(t.Columns), maxSchemaColumns)]
		for i, c := range cols {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(c.Name)
			if c.Type != "" {
				sb.WriteString(" " + c.Type)
			}
		}
		if n := len(t.Columns) - len(cols); n > 0 {
			fmt.Fprintf(&sb, ", … and %d more", n)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// schemaNote names the tables for the transcript: by name while that stays
// short, by count once a list would push the rest of the note off the chip.
func schemaNote(tables []Table) string {
	if len(tables) > 3 {
		return fmt.Sprintf("schema of %d tables", len(tables))
	}
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.Name
	}
	return "schema of " + strings.Join(names, ", ")
}

// dialectName names a driver the way a model knows the database. bytdb is
// new enough that no model has heard of it, and it speaks the Postgres
// dialect, so saying so is what gets usable SQL back.
func dialectName(driver string) string {
	switch driver {
	case "postgres", "pgx", "postgresql":
		return "PostgreSQL"
	case "mysql", "mariadb":
		return "MySQL"
	case "sqlite", "sqlite3":
		return "SQLite"
	case "bytdb":
		return "bytdb (an embedded database speaking the PostgreSQL dialect)"
	}
	return driver
}
