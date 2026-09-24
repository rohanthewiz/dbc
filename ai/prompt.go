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

// DefaultContextRows is how many result rows go with a question when the
// connection allows rows and ai_context_rows is not set.
const DefaultContextRows = 10

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

	// SendRows is the connection's ai_rows opt-in; MaxRows is ai_context_rows.
	SendRows bool
	MaxRows  int
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
	if q := strings.TrimSpace(ctx.Query); q != "" {
		sb.WriteString("The SQL in question:\n```sql\n")
		sb.WriteString(q)
		sb.WriteString("\n```\n\n")
		sent = append(sent, "query")
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
		switch {
		case n > 0:
			fmt.Fprintf(&sb, "%s:\n", rowsHeading(n, ctx))
			sb.WriteString(markdownRows(ctx.Columns, ctx.Rows[:n]))
			sb.WriteString("\n")
			sent = append(sent, fmt.Sprintf("%d of %s rows", n, totalRows(ctx)))
		default:
			// Column names are schema, not contents, so they go even when
			// rows do not: "why is price a string?" needs them.
			fmt.Fprintf(&sb, "Its result has the columns: %s (%s rows; the values are not shared).\n\n",
				strings.Join(ctx.Columns, ", "), totalRows(ctx))
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
