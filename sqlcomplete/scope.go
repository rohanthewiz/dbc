package sqlcomplete

import (
	"strings"

	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

type tokKind int

const (
	tWord   tokKind = iota // a bare word: a keyword or a name
	tQuoted                // a quoted identifier, text unquoted
	tPunct                 // one punctuation mark, or ::
	tOther                 // a string, a number, a parameter
)

// tok is one token of the statement window, with its byte range in the
// buffer and its parenthesis depth: a "(" and a ")" carry the depth outside
// them, the tokens between one more.
type tok struct {
	text       string // as written; a quoted identifier without its quotes
	low        string // lower-cased text of a word, for keyword tests
	kind       tokKind
	start, end int
	depth      int
}

func (t tok) isPunct(p string) bool { return t.kind == tPunct && t.text == p }

// isName reports whether t can be (part of) a table, alias or column name.
func (t tok) isName() bool { return t.kind == tWord || t.kind == tQuoted }

// tokenize splits buf[start:end] into tokens. Strings, comments, quoted names
// and parameters are found by sqlsplit.Lex — the scanner the splitter and the
// highlighter use, so all three agree on where a string ends — and the text
// between them is cut into words and punctuation here. Comments are dropped.
func tokenize(buf string, start, end int) []tok {
	stmt := buf[start:end]
	var out []tok
	depth := 0
	emit := func(t tok) {
		t.start += start
		t.end += start
		if t.isPunct(")") && depth > 0 {
			depth--
		}
		t.depth = depth
		if t.isPunct("(") {
			depth++
		}
		out = append(out, t)
	}
	text := func(a, b int) {
		i := a
		for i < b {
			ch := stmt[i]
			switch {
			case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f' || ch == '\v':
				i++
			case isWordByte(ch):
				j := i
				for j < b && isWordByte(stmt[j]) {
					j++
				}
				w := stmt[i:j]
				emit(tok{text: w, low: strings.ToLower(w), kind: tWord, start: i, end: j})
				i = j
			case ch == ':' && i+1 < b && stmt[i+1] == ':':
				emit(tok{text: "::", kind: tPunct, start: i, end: i + 2})
				i += 2
			default:
				emit(tok{text: string(ch), kind: tPunct, start: i, end: i + 1})
				i++
			}
		}
	}
	at := 0
	for _, lt := range sqlsplit.Lex(stmt) {
		if lt.Kind == sqlsplit.TokKeyword {
			continue // a word like any other here
		}
		text(at, lt.Start)
		switch lt.Kind {
		case sqlsplit.TokComment:
		case sqlsplit.TokIdent:
			emit(tok{text: unquote(stmt[lt.Start:lt.End]), kind: tQuoted, start: lt.Start, end: lt.End})
		default:
			emit(tok{text: stmt[lt.Start:lt.End], kind: tOther, start: lt.Start, end: lt.End})
		}
		at = lt.End
	}
	text(at, len(stmt))
	return out
}

// unquote strips a quoted identifier's quotes and undoubles the quote inside.
func unquote(s string) string {
	if len(s) < 2 {
		return strings.Trim(s, "\"`")
	}
	q := s[:1]
	body := s[1:]
	if strings.HasSuffix(body, q) {
		body = body[:len(body)-1]
	}
	return strings.ReplaceAll(body, q+q, q)
}

// ---------------------------------------------------------------------------
// Scope: the tables a statement names
// ---------------------------------------------------------------------------

// scope is what the statement names: its table references and its CTEs.
type scope struct {
	refs []*ref
	ctes []*cte
}

// ref is one table reference: a catalog table, a CTE or a derived table
// (a subquery in FROM), with the alias it was given.
type ref struct {
	parts []string   // the name as written, [schema,] name; nil for a subquery
	alias string     // "" when none was given
	table *erd.Table // the catalog table it names, nil when it is not one
	cte   *cte       // the CTE it names, or the subquery's own columns
	at    int        // byte offset where it was written

	// Token indexes, for the resolver (resolve.go), which needs to know
	// which token is the name and which the alias. nameTok is the name's
	// last part (meaningful when parts is set), aliasTok the alias
	// (meaningful when alias is set).
	nameTok, aliasTok int
}

// cte is a WITH query (or a derived table) and the columns its SELECT list
// names, as far as they can be read without a parse.
type cte struct {
	name string
	cols []string
	tok  int // index of the name's token, for the resolver; unset for a derived table
}

// handle is how the statement refers to the reference: its alias, or the
// last part of its name.
func (r *ref) handle() string {
	if r.alias != "" {
		return r.alias
	}
	if len(r.parts) > 0 {
		return r.parts[len(r.parts)-1]
	}
	return ""
}

// qualifier is handle, quoted for the dialect where it needs it.
func (r *ref) qualifier(d *dialect) string { return d.quote(r.handle()) }

// describe is the reference's detail line: what it names.
func (r *ref) describe() string {
	switch {
	case r.table != nil:
		return strings.Join(r.parts, ".")
	case r.cte != nil && r.cte.name != "":
		return "CTE " + r.cte.name
	}
	return "subquery"
}

// known reports whether the reference resolved to a table, a CTE or a
// subquery whose columns could be read.
func (r *ref) known() bool { return r.table != nil || r.cte != nil }

// cols are the reference's columns: the catalog's, or the CTE's.
func (r *ref) cols() []*erd.Column {
	if r.table != nil {
		return r.table.Cols
	}
	if r.cte != nil {
		out := make([]*erd.Column, len(r.cte.cols))
		for i, n := range r.cte.cols {
			out[i] = &erd.Column{Name: n}
		}
		return out
	}
	return nil
}

// find is the reference q names — by alias first, then by table or CTE name —
// or nil. A reference that resolved to nothing is skipped: in FROM sales.▮
// the half-typed "sales" is a schema, not a table to take columns from.
func (s *scope) find(q string) *ref {
	for _, r := range s.refs {
		if r.alias != "" && strings.EqualFold(r.alias, q) && r.known() {
			return r
		}
	}
	for _, r := range s.refs {
		if r.alias == "" && len(r.parts) > 0 && strings.EqualFold(r.parts[len(r.parts)-1], q) && r.known() {
			return r
		}
	}
	for _, c := range s.ctes {
		if strings.EqualFold(c.name, q) {
			return &ref{parts: []string{c.name}, cte: c}
		}
	}
	return nil
}

// newAlias makes an alias for a table about to be joined: the initials of its
// name's words (order_items → oi), with a number added when the statement
// already uses that alias or name.
func (s *scope) newAlias(table string) string {
	var b strings.Builder
	for w := range strings.SplitSeq(strings.ToLower(table), "_") {
		if w != "" {
			b.WriteByte(w[0])
		}
	}
	base := b.String()
	if base == "" || !isWordByte(base[0]) || isDigit(base[0]) {
		base = "t"
	}
	taken := func(a string) bool {
		for _, r := range s.refs {
			if strings.EqualFold(r.handle(), a) {
				return true
			}
		}
		return reserved[a]
	}
	a := base
	for n := 2; taken(a); n++ {
		a = base + itoa(n)
	}
	return a
}

// reserved words cannot be an alias without AS — after a table name they
// start the next clause — and are never generated as one.
var reserved = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`on where join inner left right full outer cross natural using
		group order limit offset having union intersect except window set values select
		returning for fetch lateral as and or when then else end default partition
		tablesample do into from with not is in force ignore straight_join use
		by asc desc null all any some case`) {
		m[w] = true
	}
	return m
}()

// parseScope reads the statement's table references and CTEs.
func parseScope(toks []tok, c *completer) *scope {
	s := &scope{}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tWord {
			continue
		}
		switch t.low {
		case "with":
			i = s.parseCTEs(toks, i+1, c)
		case "from", "join", "update", "into", "using", "truncate":
			// FROM a, b and DELETE … USING a, b are lists; the others
			// name one table
			i = s.parseRefs(toks, i+1, t.low == "from" || t.low == "using", c)
		}
	}
	return s
}

// parseRefs reads table references from toks[i], several when list is set,
// and returns the index of the last token consumed.
func (s *scope) parseRefs(toks []tok, i int, list bool, c *completer) int {
	for i < len(toks) {
		// ONLY t (Postgres: without its inheritors), LATERAL (subquery)
		for i < len(toks) && toks[i].kind == tWord && (toks[i].low == "only" || toks[i].low == "lateral") {
			i++
		}
		if i >= len(toks) {
			return i
		}
		r := &ref{at: toks[i].start}
		switch {
		case toks[i].isPunct("("):
			// a derived table: (SELECT …) alias
			close := matching(toks, i)
			r.cte = &cte{cols: selectList(toks[i+1:close], s, c)}
			i = close + 1
		case toks[i].isName():
			if reserved[toks[i].low] && toks[i].kind == tWord {
				return i - 1
			}
			for {
				r.parts = append(r.parts, toks[i].text)
				if i+2 < len(toks) && toks[i+1].isPunct(".") && toks[i+2].isName() {
					i += 2
					continue
				}
				break
			}
			r.nameTok = i
			i++
			if i < len(toks) && toks[i].isPunct("(") {
				// INSERT INTO t (cols), or a function in FROM
				// (generate_series(…), which resolves to no table):
				// no alias follows
				s.add(r, c)
				return i - 1
			}
		default:
			return i - 1
		}
		// the alias: AS a, or a bare name that is not the next clause
		if i < len(toks) && toks[i].kind == tWord && toks[i].low == "as" {
			i++
		}
		if i < len(toks) && toks[i].isName() && !(toks[i].kind == tWord && reserved[toks[i].low]) {
			r.alias = toks[i].text
			r.aliasTok = i
			i++
		}
		s.add(r, c)
		if list && i < len(toks) && toks[i].isPunct(",") {
			i++
			continue
		}
		return i - 1
	}
	return i
}

// add resolves a reference against the CTEs and the catalog and keeps it.
func (s *scope) add(r *ref, c *completer) {
	if len(r.parts) == 1 {
		for _, ct := range s.ctes {
			if strings.EqualFold(ct.name, r.parts[0]) {
				r.cte = ct
			}
		}
	}
	if r.cte == nil && len(r.parts) > 0 {
		r.table = c.resolve(r.parts)
	}
	s.refs = append(s.refs, r)
}

// parseCTEs reads WITH [RECURSIVE] name [(cols)] AS [NOT] [MATERIALIZED]
// (body) [, …] from toks[i], and returns the index of the last token read.
func (s *scope) parseCTEs(toks []tok, i int, c *completer) int {
	if i < len(toks) && toks[i].low == "recursive" {
		i++
	}
	for i < len(toks) && toks[i].isName() {
		ct := &cte{name: toks[i].text, tok: i}
		i++
		if i < len(toks) && toks[i].isPunct("(") {
			close := matching(toks, i)
			for _, t := range toks[i+1 : close] {
				if t.isName() {
					ct.cols = append(ct.cols, t.text)
				}
			}
			i = close + 1
		}
		for i < len(toks) && toks[i].kind == tWord && (toks[i].low == "as" || toks[i].low == "not" || toks[i].low == "materialized") {
			i++
		}
		if i >= len(toks) || !toks[i].isPunct("(") {
			s.ctes = append(s.ctes, ct)
			return i - 1
		}
		close := matching(toks, i)
		if ct.cols == nil {
			ct.cols = selectList(toks[i+1:close], s, c)
		}
		s.ctes = append(s.ctes, ct)
		i = close + 1
		if i < len(toks) && toks[i].isPunct(",") {
			i++
			continue
		}
		return i - 1
	}
	return i - 1
}

// matching is the index of the ")" closing the "(" at toks[i], or the last
// index when the statement ends first (it is being typed).
func matching(toks []tok, i int) int {
	d := toks[i].depth
	for j := i + 1; j < len(toks); j++ {
		if toks[j].isPunct(")") && toks[j].depth == d {
			return j
		}
	}
	return len(toks)
}

// selectList reads the column names a subquery's outer SELECT list gives its
// result: an alias (count(*) AS n, count(*) n), a column (o.id → id), and
// t.* or * expanded to the columns of what the subquery's FROM names. An
// expression with no name (a + b) gives none; Postgres would call it
// ?column?, and nobody types that.
func selectList(body []tok, outer *scope, c *completer) []string {
	if len(body) == 0 {
		return nil
	}
	d := body[0].depth
	sel := -1
	for i, t := range body {
		if t.depth == d && t.kind == tWord && t.low == "select" {
			sel = i
			break
		}
	}
	if sel < 0 {
		return nil
	}
	// the subquery's own FROM, for its stars: a scope of its own that
	// can see the outer statement's CTEs
	inner := &scope{ctes: outer.ctes}
	var items [][]tok
	var cur []tok
	for i := sel + 1; i < len(body); i++ {
		t := body[i]
		if t.depth == d && t.kind == tWord && (t.low == "from" || t.low == "union" || t.low == "except" || t.low == "intersect") {
			if t.low == "from" {
				inner.parseRefs(body, i+1, true, c)
			}
			break
		}
		if t.depth == d && t.isPunct(",") {
			items = append(items, cur)
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	items = append(items, cur)

	var out []string
	for _, it := range items {
		// DISTINCT / ALL / DISTINCT ON (…) lead the first item
		for len(it) > 0 && it[0].kind == tWord && (it[0].low == "distinct" || it[0].low == "all") {
			it = it[1:]
			if len(it) > 0 && it[0].kind == tWord && it[0].low == "on" && len(it) > 1 && it[1].isPunct("(") {
				it = it[matching(it, 1)+1:]
			}
		}
		n := len(it)
		switch {
		case n == 0:
		case it[n-1].isPunct("*"):
			if n >= 3 && it[n-2].isPunct(".") {
				if r := inner.find(it[n-3].text); r != nil {
					for _, col := range r.cols() {
						out = append(out, col.Name)
					}
				}
			} else if n == 1 {
				for _, r := range inner.refs {
					for _, col := range r.cols() {
						out = append(out, col.Name)
					}
				}
			}
		case !it[n-1].isName():
		case n == 1:
			out = append(out, it[0].text)
		default:
			p := it[n-2]
			if p.isPunct(".") || p.isPunct(")") || p.isName() || p.kind == tOther {
				out = append(out, it[n-1].text)
			}
		}
	}
	return out
}

// trailingName reads a [schema.]name at the end of toks: its parts, and the
// index of the token before it (-1 when none).
func trailingName(toks []tok) (parts []string, before int) {
	i := len(toks) - 1
	for i >= 0 && toks[i].isName() {
		parts = append([]string{toks[i].text}, parts...)
		if i >= 2 && toks[i-1].isPunct(".") {
			i -= 2
			continue
		}
		i--
		break
	}
	return parts, i
}

// ---------------------------------------------------------------------------
// The DDL in a suggestion's documentation
// ---------------------------------------------------------------------------

// ddl renders t as the CREATE statement its catalog entry amounts to, for a
// table suggestion's documentation: each column's type, NOT NULL and key
// roles, the primary key, and the foreign keys out of it.
//
//	CREATE TABLE public.orders (
//	  id          bigint NOT NULL,
//	  customer_id bigint NOT NULL,   -- → customers(id)
//	  PRIMARY KEY (id)
//	)
func ddl(t *erd.Table, s *erd.Schema) string {
	if t == nil {
		return ""
	}
	what := "TABLE"
	if t.View {
		what = "VIEW"
	}
	refs := outgoing(t, s)
	w := 0
	for _, c := range t.Cols {
		w = max(w, len(c.Name))
	}
	var b strings.Builder
	b.WriteString("CREATE " + what + " " + qualified(t) + " (\n")
	lines := make([]string, 0, len(t.Cols)+1)
	for _, c := range t.Cols {
		l := "  " + c.Name + strings.Repeat(" ", w-len(c.Name)+1) + c.Type
		if !c.Nullable {
			l += " NOT NULL"
		}
		if c.Unique {
			l += " UNIQUE"
		}
		lines = append(lines, l)
	}
	if len(t.PK) > 0 {
		lines = append(lines, "  PRIMARY KEY ("+strings.Join(t.PK, ", ")+")")
	}
	for i, l := range lines {
		b.WriteString(l)
		if i < len(lines)-1 {
			b.WriteByte(',')
		}
		if i < len(t.Cols) {
			if r := refs[t.Cols[i].Name]; r != "" {
				b.WriteString("  -- → " + r)
			}
		}
		b.WriteByte('\n')
	}
	b.WriteString(")")
	return b.String()
}

// outgoing maps each of t's foreign-key columns to what it references,
// "customers(id)".
func outgoing(t *erd.Table, s *erd.Schema) map[string]string {
	out := map[string]string{}
	if s == nil {
		return out
	}
	for _, r := range s.Rels {
		if r.Child != t {
			continue
		}
		for i, col := range r.ChildCols {
			if i < len(r.ParentCols) {
				out[col] = labelOf(r.Parent) + "(" + r.ParentCols[i] + ")"
			}
		}
	}
	return out
}

// labelOf is how a diagram names t (bare on a one-schema catalog), or its
// name when the schema was built without labels.
func labelOf(t *erd.Table) string {
	if t.Label != "" {
		return t.Label
	}
	return t.Name
}

func qualified(t *erd.Table) string {
	if t.Schema == "" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// colDetail is a column's one-line detail: its type and key roles.
func colDetail(c *erd.Column) string {
	if c == nil {
		return ""
	}
	d := c.Type
	switch {
	case c.PK:
		d += " PK"
	case c.FK:
		d += " FK"
	case c.Unique:
		d += " UNIQUE"
	}
	if !c.Nullable && !c.PK {
		d += " NOT NULL"
	}
	return d
}

// colDoc is a column's documentation: its declaration and what it references.
func colDoc(c *erd.Column, t *erd.Table, s *erd.Schema) string {
	if c == nil {
		return ""
	}
	doc := qualified(t) + "." + c.Name + " " + c.Type
	if !c.Nullable {
		doc += " NOT NULL"
	}
	if c.PK {
		doc += "\nprimary key"
	}
	if r := outgoing(t, s)[c.Name]; r != "" {
		doc += "\nreferences " + r
	}
	return doc
}

// columnList is a CTE's documentation: its columns.
func columnList(cols []string) string {
	if len(cols) == 0 {
		return ""
	}
	return "columns: " + strings.Join(cols, ", ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
