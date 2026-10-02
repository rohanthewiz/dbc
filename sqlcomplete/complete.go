// Package sqlcomplete suggests what to type next in a SQL buffer: the
// columns, tables and join conditions the connection's schema holds, and the
// dialect's keywords, functions and types. Both UIs ask it — the TUI's
// editor popup and dbc web's Monaco suggest widget — so the rules for what
// is offered where exist once, in Go, rather than as a JavaScript copy that
// would drift from the terminal's.
//
// # How a request is answered
//
//	buffer + caret ─► the statement under the caret (sqlsplit, the run's splitter)
//	                     │
//	                     ├─► tokens (sqlsplit.Lex for strings/comments/quoted names)
//	                     │
//	                     ├─► scope: FROM/JOIN/UPDATE/INTO refs and their aliases,
//	                     │          CTEs with the columns their SELECT lists name
//	                     │
//	                     └─► context: what the token before the caret asks for
//	                               │
//	           ┌──────────┬────────┴──────┬────────────┬──────────┐
//	         start      tables       expression     qualified   clause …
//	       (SELECT…)  (FROM ▮)    (WHERE ▮, =▮)    (o.▮)     (FROM t ▮)
//	                               │
//	          candidates ─► filtered by the typed prefix ─► ranked ─► capped
//
// # Lexical, not a parse
//
// Like the splitter and the highlighter this is a scanner with a few rules,
// not a SQL grammar. It knows which keyword precedes the caret and which
// tables the statement names; it does not know that a subquery's columns are
// out of scope outside it. Every table the statement names is therefore in
// scope everywhere in it. Offering a column one subquery too early costs a
// line in a list; a grammar for four dialects would cost far more and still
// fail on the half-typed statement a completion request always sees.
//
// # Offsets
//
// Every offset in and out is a byte offset into the buffer. dbc web converts
// to and from the UTF-16 units the browser counts (web/api.go byteOffset),
// the TUI from its rune positions.
package sqlcomplete

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Kind classifies a suggestion, for its icon and its place in the order.
type Kind string

const (
	KindColumn   Kind = "column"
	KindTable    Kind = "table"
	KindView     Kind = "view"
	KindSchema   Kind = "schema"
	KindAlias    Kind = "alias" // a table reference's alias, or a CTE
	KindJoin     Kind = "join"  // a whole join clause or ON condition from a foreign key
	KindKeyword  Kind = "keyword"
	KindFunction Kind = "function"
	KindType     Kind = "type"
)

// Item is one suggestion.
type Item struct {
	Label  string `json:"label"`            // what the list shows
	Kind   Kind   `json:"kind"`             // see Kind
	Detail string `json:"detail,omitempty"` // one line beside the label: a type, a signature
	Doc    string `json:"doc,omitempty"`    // longer text: a table's DDL, a function's description
	Insert string `json:"insert"`           // what replaces [From, To) when picked
	// Filter is the text the typed prefix is matched against when it is
	// not the label — a column offered qualified ("o.id") still matches
	// "id". Empty means the label.
	Filter string `json:"filter,omitempty"`
	// Cursor is where the caret lands inside Insert after the pick, as a
	// byte offset into Insert; -1 means after it. A function lands
	// between its parentheses.
	Cursor int    `json:"cursor"`
	Sort   string `json:"sort"` // ascending sort key; the order Items is already in
}

// Result is the answer to a request.
type Result struct {
	From, To int    // the byte range a picked item replaces: the word being typed
	Prefix   string // buffer[From:To], what was matched against
	Items    []Item
	// Incomplete reports that Items was cut at MaxItems: a longer prefix
	// may surface items not listed now, so a client should ask again as
	// the prefix grows rather than filter this list itself.
	Incomplete bool
}

// MaxItems caps a result. A catalog of thousands of columns answered in full
// would be a long list nobody scrolls and a large response per keystroke;
// the first few hundred, best first, are the useful ones, and Incomplete
// tells the client to ask again as the prefix narrows it.
const MaxItems = 400

// allColumnsLimit is how many columns a catalog may have for a statement with
// no tables in scope yet (SELECT ▮ before its FROM) to be offered every
// column in it. Past it, that list would be noise, and the tables are offered
// instead.
const allColumnsLimit = 3000

// Request is one completion request.
type Request struct {
	// Schema is the connection's tables, columns and keys (the ERD's
	// reading of the catalog); nil offers only the dialect's vocabulary.
	Schema *erd.Schema
	// Driver is the connection's driver as configured (postgres, pg,
	// mysql, sqlite, bytdb …), for the dialect's vocabulary and quoting.
	Driver string
	// Focus is the schema the user is browsing (the sidebar's), whose
	// tables are ranked first; "" when there is none.
	Focus string
	// Buffer is the whole editor text and Caret a byte offset into it.
	Buffer string
	Caret  int
}

// Complete answers req. It never fails: a caret inside a string or a
// comment, or a context with nothing to offer, is an empty Result.
func Complete(req Request) Result {
	buf, caret := req.Buffer, min(max(req.Caret, 0), len(req.Buffer))
	d := dialectFor(req.Driver)
	res := Result{From: caret, To: caret}

	start, end := stmtWindow(buf, caret)
	if insideLiteral(buf[start:end], caret-start) {
		return res
	}

	// The word being typed: back from the caret over identifier bytes. A
	// word starting with a digit is a number, not a name.
	from := caret
	for from > start && isWordByte(buf[from-1]) {
		from--
	}
	if from < caret && isDigit(buf[from]) {
		return res
	}
	res.From, res.Prefix = from, buf[from:caret]

	toks := tokenize(buf, start, end)
	// The word under the caret is what is being typed, not a name the
	// statement uses: leaving it out keeps "FROM ord▮" from adding a table
	// "ord" to the scope and "SELECT o▮" from finding an alias "o".
	toks = slices.DeleteFunc(toks, func(t tok) bool { return t.start == from && from < caret })
	var before []tok
	for _, t := range toks {
		if t.end <= from {
			before = append(before, t)
		}
	}

	c := &completer{req: req, d: d, buf: buf, caret: caret, prefix: res.Prefix}
	c.index()
	c.sc = parseScope(toks, c)
	c.suggest(before)
	res.Items, res.Incomplete = c.finish()
	return res
}

// completer holds one request's working state.
type completer struct {
	req    Request
	d      *dialect
	buf    string
	caret  int
	prefix string
	sc     *scope

	// the catalog, indexed: lower(name) → tables of that name, every
	// schema's, and lower(schema) → its tables
	byName   map[string][]*erd.Table
	bySchema map[string][]*erd.Table
	schemas  []string // distinct schema names, in catalog order

	items []Item
	seen  map[string]bool // Kind + Insert, so one thing is offered once
}

func (c *completer) index() {
	c.byName, c.bySchema, c.seen = map[string][]*erd.Table{}, map[string][]*erd.Table{}, map[string]bool{}
	if c.req.Schema == nil {
		return
	}
	for _, t := range c.req.Schema.Tables {
		n, s := strings.ToLower(t.Name), strings.ToLower(t.Schema)
		c.byName[n] = append(c.byName[n], t)
		if _, ok := c.bySchema[s]; !ok {
			c.schemas = append(c.schemas, t.Schema)
		}
		c.bySchema[s] = append(c.bySchema[s], t)
	}
}

// ---------------------------------------------------------------------------
// The statement under the caret
// ---------------------------------------------------------------------------

// stmtWindow is the byte range of the statement the caret is in, as the run's
// splitter sees it, extended to the caret when the caret sits in the blank
// space after the statement's last word (the splitter trims that off). A
// caret past a statement's semicolon, or before the first word of the next,
// starts a statement of its own: the window then begins at the caret's word.
func stmtWindow(buf string, caret int) (start, end int) {
	stmts := sqlsplit.Split(buf)
	i := sqlsplit.IndexAt(stmts, caret)
	if i < 0 {
		return 0, len(buf)
	}
	s := stmts[i]
	if caret < s.Start {
		// in the blank space before a statement: whatever precedes the
		// caret belongs to the statement before
		w := caret
		for w > 0 && isWordByte(buf[w-1]) {
			w--
		}
		return w, max(s.End, caret)
	}
	if caret > s.End && strings.Contains(buf[s.End:caret], ";") {
		return caret, caret // just past the terminator: a new statement
	}
	return s.Start, max(s.End, caret)
}

// insideLiteral reports whether offset (into stmt) is inside a string, a
// comment or a quoted identifier, where nothing is suggested. The end of a
// line comment counts as inside it (the comment runs to the newline), as does
// the end of an unterminated string or quoted name.
func insideLiteral(stmt string, offset int) bool {
	for _, t := range sqlsplit.Lex(stmt) {
		if t.Start >= offset {
			break
		}
		switch t.Kind {
		case sqlsplit.TokString, sqlsplit.TokIdent, sqlsplit.TokComment:
		default:
			continue
		}
		if offset < t.End {
			return true
		}
		if offset == t.End {
			body := stmt[t.Start:t.End]
			switch {
			case strings.HasPrefix(body, "--"):
				return true
			case t.Kind != sqlsplit.TokComment && !closed(body):
				return true
			}
		}
	}
	return false
}

// closed reports whether a quoted token ends with the quote it opened with.
// A dollar-quoted body ends with its tag, which closes it as well.
func closed(body string) bool {
	if len(body) < 2 {
		return false
	}
	q := body[0]
	if q == '$' {
		return body[len(body)-1] == '$'
	}
	return body[len(body)-1] == q
}

// ---------------------------------------------------------------------------
// Context: what the caret's position asks for
// ---------------------------------------------------------------------------

// tableWords precede a table name.
var tableWords = map[string]bool{
	"from": true, "join": true, "update": true, "into": true, "table": true,
	"truncate": true, "only": true, "describe": true,
}

// exprWords precede an expression, where columns are what is wanted.
var exprWords = map[string]bool{
	"select": true, "where": true, "and": true, "or": true, "by": true, "having": true,
	"set": true, "returning": true, "when": true, "then": true, "else": true, "case": true,
	"not": true, "distinct": true, "like": true, "ilike": true, "between": true,
	"in": true, "is": true, "all": true, "any": true, "some": true, "exists": true,
	"filter": true, "partition": true, "coalesce": true, "using": true,
}

// suggest fills c.items for the context the tokens before the caret's word
// make.
func (c *completer) suggest(before []tok) {
	n := len(before)
	prev := func(k int) tok { // k = 1 is the token just before the word
		if n-k < 0 {
			return tok{}
		}
		return before[n-k]
	}
	p := prev(1)

	// qualified: o.▮, orders.▮, public.▮, public.orders.▮ — the dot must
	// touch the word being typed
	if p.isPunct(".") && p.end == c.caret-len(c.prefix) {
		q := prev(2)
		if !q.isName() {
			return
		}
		var q0 string
		if prev(3).isPunct(".") && prev(4).isName() {
			q0 = prev(4).text
		}
		c.qualified(q0, q.text)
		return
	}

	switch {
	case n == 0 || p.isPunct(";"):
		c.statementStart()
	case p.isPunct("::"):
		c.types(0)
	case p.kind == tWord && p.low == "as":
		// AS names an alias, which nothing can suggest — except in
		// CAST(x AS ▮), where a type follows
		if c.inCast(before) {
			c.types(0)
		}
	case p.kind == tWord && p.low == "join":
		c.tables(true)
	case p.kind == tWord && tableWords[p.low]:
		c.tables(false)
	case p.kind == tWord && p.low == "on":
		c.joinConditions()
		c.expression(1)
	case p.kind == tWord && exprWords[p.low]:
		c.expression(0)
	case p.isPunct(","):
		c.afterComma(before)
	case p.isPunct("("):
		c.afterParen(before)
	case p.isPunct("*"):
		// SELECT * ▮ or t.* ▮ is past an expression; a * between two
		// operands is multiplication
		if q := prev(2); q.kind == tWord && q.low == "select" || q.isPunct(",") || q.isPunct(".") {
			c.clause()
		} else {
			c.expression(0)
		}
	case p.kind == tPunct && p.text != ")" && p.text != "]":
		// an operator: =, <, ||, + … is followed by an operand
		c.expression(0)
	default:
		// past a name, a literal, a closing paren or a keyword that ends
		// a phrase (ASC, NULL): the next clause
		c.clause()
	}
}

// statementStart offers the words a statement begins with.
func (c *completer) statementStart() {
	for _, k := range c.d.starts {
		c.add(Item{Label: c.kwCase(k), Kind: KindKeyword, Insert: c.kwCase(k)}, 0)
	}
	// a bare table name is not a statement, but TABLE t and SELECT … FROM t
	// are, and showing the tables here lets someone see what is there
	c.keywords(5)
}

// afterComma decides between a list of tables (FROM a, ▮) and a list of
// expressions (SELECT a, ▮ · GROUP BY a, ▮ · INSERT INTO t (a, ▮)) by the
// keyword that opened the list at the comma's depth.
func (c *completer) afterComma(before []tok) {
	depth := before[len(before)-1].depth
	for i := len(before) - 2; i >= 0; i-- {
		t := before[i]
		if t.depth < depth {
			// the list is inside parentheses: INSERT INTO t (a, ▮)
			if t.isPunct("(") {
				if tbl := c.insertTarget(before[:i]); tbl != nil {
					c.columnsOf(tbl, "", 0)
					return
				}
			}
			break
		}
		if t.depth > depth || t.kind != tWord {
			continue
		}
		switch {
		case t.low == "from":
			c.tables(false)
			return
		case exprWords[t.low] || t.low == "values" || t.low == "returning":
			c.expression(0)
			return
		}
	}
	c.expression(0)
}

// afterParen decides what an open parenthesis wants: the target's columns in
// INSERT INTO t (▮, a subquery's SELECT, or an expression — a function's
// argument, a grouped condition.
func (c *completer) afterParen(before []tok) {
	head := before[:len(before)-1]
	if tbl := c.insertTarget(head); tbl != nil {
		c.columnsOf(tbl, "", 0)
		return
	}
	if len(head) > 0 && head[len(head)-1].kind == tWord {
		switch head[len(head)-1].low {
		case "in", "exists", "from", "join", "as", "lateral":
			// a subquery's place: SELECT first
			c.add(Item{Label: c.kwCase("SELECT"), Kind: KindKeyword, Insert: c.kwCase("SELECT")}, 0)
			if head[len(head)-1].low == "in" {
				c.expression(1)
			}
			return
		}
	}
	c.expression(0)
}

// insertTarget is the table an INSERT INTO t ( names, when toks ends with
// INTO t (any qualifier allowed); nil otherwise.
func (c *completer) insertTarget(toks []tok) *erd.Table {
	parts, i := trailingName(toks)
	if len(parts) == 0 || i < 0 || toks[i].kind != tWord || toks[i].low != "into" {
		return nil
	}
	return c.resolve(parts)
}

// inCast reports whether the AS before the caret is a CAST's: the innermost
// open parenthesis is CAST's.
func (c *completer) inCast(before []tok) bool {
	as := before[len(before)-1]
	for i := len(before) - 2; i >= 1; i-- {
		t := before[i]
		if t.isPunct("(") && t.depth < as.depth {
			return before[i-1].kind == tWord && before[i-1].low == "cast"
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The candidate sets
// ---------------------------------------------------------------------------

// qualified offers what follows "q." — the columns of the table or alias q
// names, or the tables of schema q. q0 is a schema before it (s.t.▮).
func (c *completer) qualified(q0, q string) {
	if q0 != "" {
		if t := c.resolve([]string{q0, q}); t != nil {
			c.columnsOf(t, "", 0)
		}
		return
	}
	// an alias or a CTE in the statement wins over a catalog name: in
	// FROM orders o, "o." is the alias even if a table o exists
	if r := c.sc.find(q); r != nil {
		c.refColumns(r, 0)
		return
	}
	if t := c.resolve([]string{q}); t != nil {
		c.columnsOf(t, "", 0)
	}
	for _, t := range c.bySchema[strings.ToLower(q)] {
		c.table(t, 1, true)
	}
}

// tables offers what may follow FROM or JOIN. The ranks:
//
//	0  the statement's CTEs; after JOIN, whole join clauses from foreign keys
//	1  after JOIN, the tables those foreign keys link to, by bare name
//	2  the browsed schema's tables and views (every one when none is browsed)
//	3  the other schemas' tables
//	4  the schemas themselves, for schema.▮
func (c *completer) tables(join bool) {
	for _, cte := range c.sc.ctes {
		c.add(Item{Label: cte.name, Kind: KindAlias, Detail: "CTE", Insert: c.d.quote(cte.name),
			Doc: columnList(cte.cols)}, 0)
	}
	if join {
		c.joinClauses()
	}
	if c.req.Schema == nil {
		return
	}
	for _, t := range c.req.Schema.Tables {
		rank := 3
		if c.req.Focus == "" || t.Schema == c.req.Focus {
			rank = 2
		}
		c.table(t, rank, false)
	}
	if len(c.schemas) > 1 {
		for _, s := range c.schemas {
			c.add(Item{Label: s, Kind: KindSchema, Detail: "schema", Insert: c.d.quote(s) + ".", Cursor: -1}, 4)
		}
	}
	c.add(Item{Label: c.kwCase("LATERAL"), Kind: KindKeyword, Insert: c.kwCase("LATERAL")}, 6)
}

// expression offers what an expression may use: the columns of the tables in
// scope, the scope's aliases, the dialect's functions and keywords. With
// nothing in scope yet (SELECT ▮ before its FROM), a small catalog's every
// column is offered, qualified by its table, and otherwise the tables.
func (c *completer) expression(rank int) {
	if len(c.sc.refs) > 0 {
		// a column whose name more than one table in scope has must be
		// qualified, or the statement is ambiguous
		count := map[string]int{}
		for _, r := range c.sc.refs {
			for _, col := range r.cols() {
				count[strings.ToLower(col.Name)]++
			}
		}
		for _, r := range c.sc.refs {
			c.refColumns(r, rank, count)
		}
		for _, r := range c.sc.refs {
			name := r.handle()
			if name == "" {
				continue
			}
			c.add(Item{Label: name, Kind: KindAlias, Detail: r.describe(), Insert: c.d.quote(name)}, rank+2)
		}
	} else if c.req.Schema != nil {
		total := 0
		for _, t := range c.req.Schema.Tables {
			total += len(t.Cols)
		}
		if total <= allColumnsLimit {
			for _, t := range c.req.Schema.Tables {
				c.columnsOf(t, t.Name, rank+2)
			}
		}
		for _, t := range c.req.Schema.Tables {
			c.table(t, rank+3, false)
		}
	}
	c.functions(rank + 3)
	c.keywords(rank + 4)
}

// clause offers the keywords that may come next after a finished phrase:
// FROM t ▮ → WHERE, JOIN …; WHERE a ▮ → IS NULL, IN, LIKE ….
func (c *completer) clause() {
	for _, k := range c.d.clauses {
		c.add(Item{Label: c.kwCase(k), Kind: KindKeyword, Insert: c.kwCase(k)}, 0)
	}
	c.keywords(2)
}

// keywords offers the dialect's whole vocabulary at rank.
func (c *completer) keywords(rank int) {
	for _, k := range c.d.keywords {
		c.add(Item{Label: c.kwCase(k), Kind: KindKeyword, Insert: c.kwCase(k)}, rank)
	}
}

// functions offers the dialect's functions at rank. The caret lands between
// the parentheses, unless the buffer already has one after the caret.
func (c *completer) functions(rank int) {
	paren := c.caret < len(c.buf) && c.buf[c.caret] == '('
	for _, f := range c.d.funcs {
		it := Item{Label: f.name, Kind: KindFunction, Detail: f.sig, Doc: f.doc, Insert: f.name + "()", Cursor: len(f.name) + 1}
		if f.bare {
			// CURRENT_DATE, CURRENT_TIMESTAMP: called without parentheses
			it.Insert, it.Cursor = f.name, -1
		} else if paren {
			it.Insert, it.Cursor = f.name, -1
		}
		c.add(it, rank)
	}
}

// types offers the dialect's type names, after :: or in CAST(x AS ▮).
func (c *completer) types(rank int) {
	for _, t := range c.d.types {
		c.add(Item{Label: t, Kind: KindType, Detail: "type", Insert: t}, rank)
	}
}

// joinClauses offers, after JOIN, each table a foreign key links to a table
// already in scope, as the whole clause: "customers c ON c.id = o.customer_id".
func (c *completer) joinClauses() {
	if c.req.Schema == nil {
		return
	}
	for _, r := range c.sc.refs {
		if r.table == nil {
			continue
		}
		for _, rel := range c.req.Schema.Rels {
			var other *erd.Table
			var mine, theirs []string
			switch {
			case rel.Child == r.table:
				other, mine, theirs = rel.Parent, rel.ChildCols, rel.ParentCols
			case rel.Parent == r.table:
				other, mine, theirs = rel.Child, rel.ParentCols, rel.ChildCols
			default:
				continue
			}
			alias := c.sc.newAlias(other.Name)
			name := c.tableRef(other)
			var conds []string
			for i := range mine {
				conds = append(conds, alias+"."+c.d.quote(theirs[i])+" = "+r.qualifier(c.d)+"."+c.d.quote(mine[i]))
			}
			text := name + " " + alias + " ON " + strings.Join(conds, " AND ")
			c.add(Item{Label: text, Kind: KindJoin, Detail: "join on " + rel.Name, Filter: other.Name,
				Doc: ddl(other, c.req.Schema), Insert: text}, 0)
			// the related table alone also comes before the rest
			c.table(other, 1, false)
		}
	}
}

// joinConditions offers, after ON, the condition each foreign key between the
// table being joined (the last reference before the caret) and another in
// scope makes.
func (c *completer) joinConditions() {
	if c.req.Schema == nil || len(c.sc.refs) < 2 {
		return
	}
	var last *ref
	for _, r := range c.sc.refs {
		if r.at < c.caret {
			last = r
		}
	}
	if last == nil || last.table == nil {
		return
	}
	for _, r := range c.sc.refs {
		if r == last || r.table == nil {
			continue
		}
		for _, rel := range c.req.Schema.Rels {
			var a, b []string // last's columns, r's columns
			switch {
			case rel.Child == last.table && rel.Parent == r.table:
				a, b = rel.ChildCols, rel.ParentCols
			case rel.Parent == last.table && rel.Child == r.table:
				a, b = rel.ParentCols, rel.ChildCols
			default:
				continue
			}
			var conds []string
			for i := range a {
				conds = append(conds, last.qualifier(c.d)+"."+c.d.quote(a[i])+" = "+r.qualifier(c.d)+"."+c.d.quote(b[i]))
			}
			text := strings.Join(conds, " AND ")
			c.add(Item{Label: text, Kind: KindJoin, Detail: "foreign key " + rel.Name, Insert: text,
				Filter: strings.Join(a, " ")}, 0)
		}
	}
}

// ---------------------------------------------------------------------------
// Items for schema objects
// ---------------------------------------------------------------------------

// table adds t at rank. bare inserts the name unqualified (it follows a
// schema the user typed).
func (c *completer) table(t *erd.Table, rank int, bare bool) {
	kind, what := KindTable, "table"
	if t.View {
		kind, what = KindView, "view"
	}
	detail := what
	if len(c.schemas) > 1 {
		detail = t.Schema + " · " + what
	}
	ins := c.d.quote(t.Name)
	if !bare {
		ins = c.tableRef(t)
	}
	c.add(Item{Label: t.Name, Kind: kind, Detail: detail, Doc: ddl(t, c.req.Schema), Insert: ins}, rank)
}

// tableRef is how a statement names t: bare where the dialect resolves the
// bare name to it, schema-qualified otherwise.
func (c *completer) tableRef(t *erd.Table) string {
	if c.d.bare(t.Schema, len(c.schemas)) {
		return c.d.quote(t.Name)
	}
	return c.d.quote(t.Schema) + "." + c.d.quote(t.Name)
}

// columnsOf adds t's columns at rank. qual, when set, is shown and inserted
// before each name ("orders.id") — for columns offered with no table in
// scope to say which table they come from.
func (c *completer) columnsOf(t *erd.Table, qual string, rank int) {
	for _, col := range t.Cols {
		it := c.column(col, t)
		if qual != "" {
			it.Label = qual + "." + col.Name
			it.Insert = c.d.quote(qual) + "." + c.d.quote(col.Name)
			it.Filter = col.Name
		}
		c.add(it, rank)
	}
}

// refColumns adds the columns of a reference in scope. When dup is given, a
// column whose name dup counts more than once is offered qualified by the
// reference's alias or name.
func (c *completer) refColumns(r *ref, rank int, dup ...map[string]int) {
	for _, col := range r.cols() {
		var it Item
		if r.table != nil {
			it = c.column(r.table.Col(col.Name), r.table)
		} else {
			it = Item{Label: col.Name, Kind: KindColumn, Detail: r.describe(), Insert: c.d.quote(col.Name), Cursor: -1}
		}
		if len(dup) > 0 && dup[0][strings.ToLower(col.Name)] > 1 {
			q := r.qualifier(c.d)
			it.Label = r.handle() + "." + col.Name
			it.Insert = q + "." + c.d.quote(col.Name)
			it.Filter = col.Name
		}
		c.add(it, rank)
	}
}

// column is col of table t as an item: its type and keys in the detail, its
// whole declaration and any foreign key it is part of in the doc.
func (c *completer) column(col *erd.Column, t *erd.Table) Item {
	return Item{Label: col.Name, Kind: KindColumn, Detail: colDetail(col) + " · " + t.Name,
		Doc: colDoc(col, t, c.req.Schema), Insert: c.d.quote(col.Name), Cursor: -1}
}

// resolve finds the table a name's parts ([schema,] name) refer to:
// case-insensitively, an exact-case match preferred, and for a bare name the
// browsed schema's table, then the dialect's default schema's, then the only
// one of that name.
func (c *completer) resolve(parts []string) *erd.Table {
	if len(parts) == 0 {
		return nil
	}
	name := parts[len(parts)-1]
	cands := c.byName[strings.ToLower(name)]
	if len(parts) > 1 {
		schema := parts[len(parts)-2]
		var in []*erd.Table
		for _, t := range cands {
			if strings.EqualFold(t.Schema, schema) {
				in = append(in, t)
			}
		}
		cands = in
	}
	if len(cands) == 0 {
		return nil
	}
	pick := func(ok func(*erd.Table) bool) *erd.Table {
		for _, t := range cands {
			if ok(t) && t.Name == name {
				return t
			}
		}
		for _, t := range cands {
			if ok(t) {
				return t
			}
		}
		return nil
	}
	if len(parts) == 1 {
		if t := pick(func(t *erd.Table) bool { return c.req.Focus != "" && t.Schema == c.req.Focus }); t != nil {
			return t
		}
		if t := pick(func(t *erd.Table) bool { return c.d.bare(t.Schema, 2) }); t != nil {
			return t
		}
	}
	return pick(func(*erd.Table) bool { return true })
}

// ---------------------------------------------------------------------------
// Filtering, ranking, the cap
// ---------------------------------------------------------------------------

// add keeps it if it matches the typed prefix, ranked. The sort key is
//
//	rank · match quality · label
//
// so the context's own candidates (rank 0: the scope's columns, the clause's
// keywords) come before the general vocabulary, and within a rank a prefix
// match comes before a match inside the word.
func (c *completer) add(it Item, rank int) {
	key := string(it.Kind) + "\x00" + it.Insert
	if c.seen[key] {
		return
	}
	filter := it.Filter
	if filter == "" {
		filter = it.Label
	}
	q, ok := matchQuality(filter, c.prefix)
	if !ok {
		return
	}
	c.seen[key] = true
	// Cursor's zero value means "not set": a caret at the very start of
	// what was inserted is never wanted, so 0 is read as "after it"
	if it.Cursor == 0 {
		it.Cursor = -1
	}
	it.Sort = fmt.Sprintf("%d%d%s", min(rank, 9), q, strings.ToLower(it.Label))
	c.items = append(c.items, it)
}

// matchQuality scores how word matches prefix, case-insensitively: 0 a
// prefix, 1 the prefix at a word boundary inside it (_id in customer_id),
// 2 the prefix's letters in order (cid → customer_id). ok is false for no
// match. An empty prefix matches everything.
func matchQuality(word, prefix string) (q int, ok bool) {
	if prefix == "" {
		return 0, true
	}
	w, p := strings.ToLower(word), strings.ToLower(prefix)
	if strings.HasPrefix(w, p) {
		return 0, true
	}
	for i := 1; i < len(w); i++ {
		if !isWordByte(w[i-1]) || w[i-1] == '_' {
			if strings.HasPrefix(w[i:], p) {
				return 1, true
			}
		}
	}
	// letters in order, the first one matching the word's first: "cid"
	// finds customer_id, but "id" alone does not find every word with an
	// i and a d in it
	if w[0] != p[0] {
		return 0, false
	}
	j := 0
	for i := 0; i < len(w) && j < len(p); i++ {
		if w[i] == p[j] {
			j++
		}
	}
	return 2, j == len(p)
}

// finish sorts the items and applies the cap.
func (c *completer) finish() ([]Item, bool) {
	slices.SortStableFunc(c.items, func(a, b Item) int { return strings.Compare(a.Sort, b.Sort) })
	if len(c.items) > MaxItems {
		return c.items[:MaxItems], true
	}
	return c.items, false
}

// kwCase writes a keyword in the case the user is typing in: lower when the
// prefix has a lower-case letter, upper otherwise (and with nothing typed).
func (c *completer) kwCase(k string) string {
	if strings.ToUpper(c.prefix) != c.prefix {
		return strings.ToLower(k)
	}
	return k
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isWordByte is an identifier byte: ASCII letters, digits, _ and $ (Postgres
// allows $ after the first character), and every byte of a multi-byte UTF-8
// rune, so a name in another script is one word.
func isWordByte(b byte) bool {
	return b == '_' || b == '$' || isDigit(b) || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}
