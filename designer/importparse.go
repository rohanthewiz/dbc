package designer

import (
	"strconv"
	"strings"
)

// Reading SQL for Import (import.go): tokens, then a small parse of ONE
// SELECT into an AST the binder maps onto a design. The grammar is only as
// wide as a design can be — whatever it does not know is refused here with
// the reason, never skipped:
//
//	stmt    SELECT [DISTINCT] item, … FROM from [WHERE e] [GROUP BY e, …]
//	        [HAVING e] [ORDER BY e [ASC|DESC], …] [LIMIT …] [OFFSET …]
//	        [FETCH FIRST n ROWS ONLY] [;]
//	from    table [[AS] alias] { join table [[AS] alias] [ON e | USING (c, …)] | , table … }
//	join    [INNER] JOIN | LEFT [OUTER] JOIN | RIGHT [OUTER] JOIN | FULL [OUTER] JOIN | CROSS JOIN
//	e       OR < AND < NOT < comparison, IS [NOT] NULL, [NOT] BETWEEN, [NOT] IN,
//	        [NOT] LIKE/ILIKE [ESCAPE s] < || + - < * / % < unary - < ::type < primary
//	primary literal, column, t.*, *, f(args), (e), (SELECT …), EXISTS (SELECT …),
//	        DATE/TIMESTAMP 'lit', INTERVAL …, CAST(e AS type)
//
// The tokens are read here, by the ENGINE's rules, not by sqlsplit's
// scanner (which the editor colors with). That scanner honors a backslash
// before a quote in every dialect, MySQL's rule, so that a split never
// cuts a MySQL string short; on Postgres it misreads a string ending in a
// backslash — and ESCAPE '\', which the generator writes, is one. A design
// read from a misread string would be a different query, so each dialect's
// own rules apply here:
//
//	            'string'           "name"     `name`   $tag$…$tag$   # comment
//	postgres    '' only            name       —        string        —
//	mysql       '' and \ escapes   string     name     —             comment
//	sqlite      '' only            name       name     —             —
//
// bytdb reads as Postgres. Block comments nest on Postgres, not elsewhere.

// tkind is a token's class.
type tkind int

const (
	tWord  tkind = iota // a bare word, keyword or name; val is lower-cased
	tIdent              // a quoted name; val is the name, unquoted
	tStr                // a string literal; val is its value
	tNum                // a number
	tParam              // $1, ?, :name
	tPunct              // ( ) , . ; * and the operators; val is the operator
	tEOF
)

type token struct {
	kind tkind
	val  string
	raw  string // as written
	pos  int    // byte offset
}

// lexSQL tokenizes sql for dialect dia. Comments are dropped (comment says
// whether there were any, for a note).
func lexSQL(sql string, dia dialect) (toks []token, comment bool, err error) {
	pg := dia.name == "postgres" || dia.name == "bytdb"
	add := func(k tkind, val string, i, j int) {
		toks = append(toks, token{kind: k, val: val, raw: sql[i:j], pos: i})
	}
	at := func(i int) byte {
		if i < len(sql) {
			return sql[i]
		}
		return 0
	}
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && at(i+1) == '-', c == '#' && dia.name == "mysql":
			comment = true
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '/' && at(i+1) == '*':
			comment = true
			start, depth := i, 0
			for i < len(sql) {
				switch {
				case sql[i] == '/' && at(i+1) == '*' && (pg || depth == 0):
					depth++
					i += 2
				case sql[i] == '*' && at(i+1) == '/':
					depth--
					i += 2
				default:
					i++
					continue
				}
				if depth == 0 {
					break
				}
			}
			if depth > 0 {
				return nil, false, &ImportError{Reason: "a comment is not closed", At: start}
			}
		case c == '\'' || c == '"' && dia.name == "mysql":
			j, v, ok := scanString(sql, i, c, dia.backslash)
			if !ok {
				return nil, false, &ImportError{Reason: "a string is not closed", At: i}
			}
			// E'…', N'…', X'…', B'…': a prefixed string means something
			// the plain value does not (escapes, bytes, bits)
			if n := len(toks); n > 0 && toks[n-1].kind == tWord && toks[n-1].pos+len(toks[n-1].raw) == i {
				return nil, false, &ImportError{Reason: "a prefixed string (" + toks[n-1].raw + "'…'): the designer compares with plain values", At: toks[n-1].pos}
			}
			add(tStr, v, i, j)
			i = j
		case c == '"' || c == '`':
			j, v, ok := scanString(sql, i, c, false)
			if !ok {
				return nil, false, &ImportError{Reason: "a quoted name is not closed", At: i}
			}
			add(tIdent, v, i, j)
			i = j
		case c == '$' && pg && !isDigitByte(at(i+1)):
			// $tag$…$tag$ (the tag may be empty: $$…$$)
			k := i + 1
			for k < len(sql) && wordByte(sql[k]) && sql[k] != '$' {
				k++
			}
			if at(k) != '$' {
				add(tPunct, "$", i, i+1)
				i++
				continue
			}
			tag := sql[i : k+1]
			end := strings.Index(sql[k+1:], tag)
			if end < 0 {
				return nil, false, &ImportError{Reason: "a dollar-quoted string is not closed", At: i}
			}
			add(tStr, sql[k+1:k+1+end], i, k+1+end+len(tag))
			i = k + 1 + end + len(tag)
		case c == '$' || c == '?' || c == ':' && at(i+1) != ':' && i > 0 && sql[i-1] != ':' && wordByte(at(i+1)) && !isDigitByte(at(i+1)):
			j := i + 1
			for j < len(sql) && wordByte(sql[j]) && sql[j] != '$' {
				j++
			}
			add(tParam, sql[i:j], i, j)
			i = j
		case isDigitByte(c) || c == '.' && isDigitByte(at(i+1)):
			j := i
			for j < len(sql) && (isDigitByte(sql[j]) || sql[j] == '.') {
				j++
			}
			if (at(j) == 'e' || at(j) == 'E') && (isDigitByte(at(j+1)) || (at(j+1) == '-' || at(j+1) == '+') && isDigitByte(at(j+2))) {
				j += 2
				for j < len(sql) && isDigitByte(sql[j]) {
					j++
				}
			}
			add(tNum, sql[i:j], i, j)
			i = j
		case wordByte(c):
			j := i
			for j < len(sql) && wordByte(sql[j]) {
				j++
			}
			add(tWord, strings.ToLower(sql[i:j]), i, j)
			i = j
		default:
			op := sql[i : i+1]
			if i+1 < len(sql) {
				switch two := sql[i : i+2]; two {
				case "<>", "!=", "<=", ">=", "::", "||", "~*", "!~", "@>", "<@", "&&", "@@":
					op = two
				}
			}
			add(tPunct, op, i, i+len(op))
			i += len(op)
		}
	}
	toks = append(toks, token{kind: tEOF, pos: len(sql)})
	return toks, comment, nil
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// wordByte is a byte of a bare word: letters, digits, _ and $ (MySQL and
// Postgres allow it after the first letter), and any non-ASCII byte, as
// names may have them.
func wordByte(c byte) bool {
	return c == '_' || c == '$' || isDigitByte(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// scanString reads the quoted run opening at i with quote q: where it
// ends, and its value — a doubled quote is one, and (backslash, MySQL's
// strings) a backslash escapes. \% and \_ keep their backslash: they mean
// something only to LIKE, which reads them itself. ok is false when it
// does not end.
func scanString(sql string, i int, q byte, backslash bool) (end int, val string, ok bool) {
	var b strings.Builder
	for j := i + 1; j < len(sql); j++ {
		c := sql[j]
		switch {
		case c == q && j+1 < len(sql) && sql[j+1] == q:
			b.WriteByte(q)
			j++
		case c == q:
			return j + 1, b.String(), true
		case c == '\\' && backslash && j+1 < len(sql):
			j++
			switch e := sql[j]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '0':
				b.WriteByte(0)
			case '%', '_':
				b.WriteByte('\\')
				b.WriteByte(e)
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return 0, "", false
}

// ── the AST ────────────────────────────────────────────────────────────

// sel is one SELECT.
type sel struct {
	pos      int
	distinct bool
	items    []selItem
	from     []fromItem
	where    *ex
	groupBy  []*ex
	having   *ex
	orderBy  []ordItem
	limit    int // -1: none
	offset   int
}

type selItem struct {
	e     *ex
	alias string
}

// fromItem is one table of FROM, and how it joins the ones before it.
type fromItem struct {
	pos          int
	schema, name string
	quoted       bool   // the name was quoted: matched exactly
	alias        string // "" none
	join         string // "" (the first), "inner", "left", "right", "full", "cross", "comma"
	on           *ex
	using        []string
	usingPos     int
}

type ordItem struct {
	e    *ex
	desc bool
}

// ex is an expression. op says which; the fields each op uses:
//
//	col       parts (schema, table, column — the last 1 to 3), quoted
//	star      t.* (parts the qualifier) or * (no parts)
//	str num   val
//	bool      val "true" | "false"
//	null param
//	call      val the function, lower; kids the arguments; distinct; star (f(*))
//	cmp       val the operator (= <> < <= > >=); kids[0], kids[1]
//	and or    kids
//	not       kids[0]
//	isnull    not; kids[0]
//	between   not; kids x, a, b
//	in        not; kids[0] x, the rest the list — or sub
//	like      val like | ilike; not; kids x, pattern[, escape]
//	exists    not; sub
//	sub       sub: a scalar subquery
//	arith     val the operator; kids
//	neg       kids[0]
//	interval  val: the interval as written, lower ("7 days", "7 day")
//	typed     val the type (date, timestamp); kids[0] the literal
//	cast      val the type; kids[0]
type ex struct {
	op       string
	pos, end int
	parts    []string
	quoted   []bool
	val      string
	kids     []*ex
	not      bool
	distinct bool
	star     bool
	sub      *sel
}

// ── the parser ─────────────────────────────────────────────────────────

type parser struct {
	toks []token
	i    int
	sql  string
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) peekN(n int) token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

// is reports whether the next token is the word w (or one of ws).
func (p *parser) is(ws ...string) bool {
	t := p.peek()
	if t.kind != tWord {
		return false
	}
	for _, w := range ws {
		if t.val == w {
			return true
		}
	}
	return false
}

// take consumes the word w if it is next.
func (p *parser) take(w string) bool {
	if p.is(w) {
		p.i++
		return true
	}
	return false
}

// punct consumes the operator s if it is next.
func (p *parser) punct(s string) bool {
	if t := p.peek(); t.kind == tPunct && t.val == s {
		p.i++
		return true
	}
	return false
}

func (p *parser) isPunct(s string) bool { t := p.peek(); return t.kind == tPunct && t.val == s }

// last is the offset just past the last token consumed.
func (p *parser) last() int {
	if p.i == 0 {
		return 0
	}
	t := p.toks[p.i-1]
	return t.pos + len(t.raw)
}

func (p *parser) fail(at int, reason string) error { return &ImportError{Reason: reason, At: at} }

// failNext refuses at the next token: what it is, and what was wanted.
func (p *parser) failNext(want string) error {
	t := p.peek()
	if t.kind == tEOF {
		return p.fail(t.pos, "the statement ends where "+want+" was expected")
	}
	return p.fail(t.pos, "`"+t.raw+"` where "+want+" was expected")
}

func (p *parser) expectPunct(s, want string) error {
	if !p.punct(s) {
		return p.failNext(want)
	}
	return nil
}

// clauseWords end a select item, a table or an expression list: a bare
// word among them is never an alias.
var clauseWords = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`from where group having order limit offset fetch join inner left right full
		cross natural on using union intersect except window for into returning straight_join lateral as
		and or not is in like ilike between asc desc nulls collate escape outer`) {
		m[w] = true
	}
	return m
}()

// aliasNext reads an optional [AS] alias.
func (p *parser) aliasNext() (string, error) {
	as := p.take("as")
	t := p.peek()
	switch {
	case t.kind == tIdent:
		p.i++
		return t.val, nil
	case t.kind == tWord && !clauseWords[t.val] && !reserved[t.val]:
		p.i++
		return t.raw, nil
	case as:
		return "", p.failNext("a name after AS")
	}
	return "", nil
}

// parseStatement reads the one SELECT, and refuses whatever else the text
// holds.
func parseStatement(toks []token, sql string) (*sel, error) {
	p := &parser{toks: toks, sql: sql}
	switch {
	case p.is("with"):
		return nil, p.fail(p.peek().pos, "a WITH clause (a CTE): the designer writes one SELECT — open the CTE's own query, "+
			"or the SELECT using it once the CTE is a table or view")
	case p.isPunct("("):
		return nil, p.fail(p.peek().pos, "a SELECT in parentheses: open the SELECT inside")
	case !p.is("select"):
		t := p.peek()
		if t.kind == tEOF {
			return nil, p.fail(0, "there is no statement here")
		}
		return nil, p.fail(t.pos, "not a SELECT (`"+t.raw+"`): the designer reads queries")
	}
	s, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	switch {
	case p.is("union", "intersect", "except"):
		return nil, p.fail(p.peek().pos, "a "+strings.ToUpper(p.peek().val)+" of two SELECTs: the designer writes one — open each on its own")
	case p.is("for"):
		return nil, p.fail(p.peek().pos, "a locking clause (FOR UPDATE / FOR SHARE): the designer only reads")
	case p.is("window"):
		return nil, p.fail(p.peek().pos, "a WINDOW clause: the designer has no window functions")
	}
	p.punct(";")
	if t := p.peek(); t.kind != tEOF {
		return nil, p.fail(t.pos, "more after the statement (`"+t.raw+"`): put the caret on one statement")
	}
	return s, nil
}

// parseSelect reads SELECT … up to the end of its last clause.
func (p *parser) parseSelect() (*sel, error) {
	s := &sel{pos: p.next().pos, limit: -1}
	if p.take("distinct") {
		if p.is("on") {
			return nil, p.fail(p.peek().pos, "DISTINCT ON: the designer's DISTINCT is of whole rows")
		}
		s.distinct = true
	} else {
		p.take("all")
	}
	if p.is("top") {
		return nil, p.fail(p.peek().pos, "TOP: write LIMIT")
	}
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		alias, err := p.aliasNext()
		if err != nil {
			return nil, err
		}
		s.items = append(s.items, selItem{e: e, alias: alias})
		if !p.punct(",") {
			break
		}
	}
	if p.is("into") {
		return nil, p.fail(p.peek().pos, "SELECT … INTO writes a table: the designer only reads")
	}
	if !p.take("from") {
		return nil, p.failNext("FROM")
	}
	if err := p.parseFrom(s); err != nil {
		return nil, err
	}
	if p.take("where") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		s.where = e
	}
	if p.is("group") {
		p.next()
		if !p.take("by") {
			return nil, p.failNext("BY")
		}
		if p.is("rollup", "cube", "grouping") {
			return nil, p.fail(p.peek().pos, "ROLLUP, CUBE and GROUPING SETS: the designer groups once")
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			s.groupBy = append(s.groupBy, e)
			if !p.punct(",") {
				break
			}
		}
		if p.is("with") {
			return nil, p.fail(p.peek().pos, "WITH ROLLUP: the designer groups once")
		}
	}
	if p.take("having") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		s.having = e
	}
	if p.is("window") {
		return nil, p.fail(p.peek().pos, "a WINDOW clause: the designer has no window functions")
	}
	if p.is("order") {
		p.next()
		if !p.take("by") {
			return nil, p.failNext("BY")
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if p.is("collate") {
				return nil, p.fail(p.peek().pos, "COLLATE in ORDER BY: the designer sorts by the column's own order")
			}
			it := ordItem{e: e}
			if p.take("desc") {
				it.desc = true
			} else {
				p.take("asc")
			}
			if p.is("nulls") {
				return nil, p.fail(p.peek().pos, "NULLS FIRST / LAST: the designer sorts empty values where the engine puts them")
			}
			s.orderBy = append(s.orderBy, it)
			if !p.punct(",") {
				break
			}
		}
	}
	return s, p.parseLimit(s)
}

// parseLimit reads LIMIT, OFFSET and FETCH in the spellings the engines
// take: LIMIT n [OFFSET m], LIMIT m, n (MySQL), LIMIT ALL, OFFSET m [ROWS],
// FETCH FIRST|NEXT n ROW[S] ONLY. A "no limit" written as LIMIT -1 (SQLite)
// or MySQL's 18446744073709551615 is no limit.
func (p *parser) parseLimit(s *sel) error {
	num := func(what string) (int, error) {
		t := p.peek()
		neg := false
		if t.kind == tPunct && t.val == "-" {
			p.next()
			neg, t = true, p.peek()
		}
		if t.kind == tParam {
			return 0, p.fail(t.pos, "a parameter as the "+what+": the designer needs the number")
		}
		if t.kind != tNum {
			return 0, p.failNext("a number for the " + what)
		}
		p.next()
		if t.val == "18446744073709551615" && what == "limit" {
			return -1, nil
		}
		n, err := strconv.Atoi(t.val)
		if err != nil || n > 1<<31 {
			return 0, p.fail(t.pos, "`"+t.raw+"` is not a whole number the designer can keep as the "+what)
		}
		if neg {
			if what == "limit" && n == 1 {
				return -1, nil
			}
			return 0, p.fail(t.pos, "a negative "+what)
		}
		return n, nil
	}
	for {
		switch {
		case p.take("limit"):
			if p.take("all") {
				continue
			}
			n, err := num("limit")
			if err != nil {
				return err
			}
			if p.punct(",") { // LIMIT offset, count
				c, err := num("limit")
				if err != nil {
					return err
				}
				if n < 0 {
					return p.fail(p.last(), "a negative offset")
				}
				s.offset, n = n, c
			}
			s.limit = n
		case p.take("offset"):
			n, err := num("offset")
			if err != nil {
				return err
			}
			if n < 0 {
				return p.fail(p.last(), "a negative offset")
			}
			s.offset = n
			if !p.take("rows") {
				p.take("row")
			}
		case p.take("fetch"):
			if !p.take("first") && !p.take("next") {
				return p.failNext("FIRST or NEXT")
			}
			n, err := num("limit")
			if err != nil {
				return err
			}
			if !p.take("rows") && !p.take("row") {
				return p.failNext("ROWS")
			}
			if !p.take("only") {
				return p.fail(p.peek().pos, "FETCH … WITH TIES: the designer's limit cuts at exactly n rows")
			}
			s.limit = n
		default:
			return nil
		}
	}
}

// parseFrom reads the FROM clause's tables and joins.
func (p *parser) parseFrom(s *sel) error {
	join := ""
	for {
		if p.is("lateral") {
			return p.fail(p.peek().pos, "LATERAL: the designer's tables are plain tables")
		}
		if p.isPunct("(") {
			if p.peekN(1).kind == tWord && p.peekN(1).val == "select" {
				return p.fail(p.peek().pos, "a subquery in FROM (a derived table): the designer's tables are tables and views — "+
					"save the subquery as a view, or open it on its own")
			}
			return p.fail(p.peek().pos, "joins in parentheses: write them one after another")
		}
		it := fromItem{pos: p.peek().pos, join: join}
		name, quoted, err := p.nameNext("a table")
		if err != nil {
			return err
		}
		it.name, it.quoted = name, quoted
		if p.punct(".") {
			it.schema = it.name
			if it.name, it.quoted, err = p.nameNext("a table after the schema"); err != nil {
				return err
			}
			if p.isPunct(".") {
				return p.fail(p.peek().pos, "a table named with its database too: name it by schema and table")
			}
		}
		if p.isPunct("(") {
			return p.fail(it.pos, "a function in FROM (`"+it.name+"(…)`): the designer's tables are tables and views")
		}
		if it.alias, err = p.aliasNext(); err != nil {
			return err
		}
		if p.isPunct("(") {
			return p.fail(p.peek().pos, "column aliases on a table (`t(a, b)`): use the table's own column names")
		}
		if p.is("tablesample", "use", "force", "ignore") || (p.is("with") && p.peekN(1).kind == tPunct && p.peekN(1).val == "(") {
			return p.fail(p.peek().pos, "a table hint or sample (`"+p.peek().raw+"`): the designer reads tables plainly")
		}
		if join != "" && join != "cross" && join != "comma" {
			switch {
			case p.take("on"):
				e, err := p.parseExpr()
				if err != nil {
					return err
				}
				it.on = e
			case p.is("using"):
				it.usingPos = p.next().pos
				if err := p.expectPunct("(", "( after USING"); err != nil {
					return err
				}
				for {
					c, _, err := p.nameNext("a column")
					if err != nil {
						return err
					}
					it.using = append(it.using, c)
					if !p.punct(",") {
						break
					}
				}
				if err := p.expectPunct(")", ") after the USING columns"); err != nil {
					return err
				}
			default:
				return p.failNext("ON or USING")
			}
		}
		s.from = append(s.from, it)

		switch {
		case p.punct(","):
			join = "comma"
		case p.is("natural"):
			return p.fail(p.peek().pos, "a NATURAL JOIN: it joins on whatever columns happen to share a name — write the ON")
		case p.is("straight_join"):
			return p.fail(p.peek().pos, "STRAIGHT_JOIN: a planner hint the designer does not keep — write JOIN")
		case p.take("join"):
			join = "inner"
		case p.take("inner"):
			join = "inner"
			if !p.take("join") {
				return p.failNext("JOIN")
			}
		case p.is("left", "right", "full"):
			join = p.next().val
			p.take("outer")
			if !p.take("join") {
				return p.failNext("JOIN")
			}
		case p.take("cross"):
			if p.is("apply") {
				return p.fail(p.peek().pos, "CROSS APPLY: the designer's tables are plain tables")
			}
			if !p.take("join") {
				return p.failNext("JOIN")
			}
			join = "cross"
		default:
			return nil
		}
	}
}

// nameNext reads a name: a bare word or a quoted one.
func (p *parser) nameNext(want string) (string, bool, error) {
	t := p.peek()
	switch {
	case t.kind == tIdent:
		p.i++
		return t.val, true, nil
	case t.kind == tWord && !clauseWords[t.val]:
		p.i++
		return t.raw, false, nil
	}
	return "", false, p.failNext(want)
}

// ── expressions ────────────────────────────────────────────────────────

func (p *parser) parseExpr() (*ex, error) { return p.parseOr() }

func (p *parser) parseOr() (*ex, error) {
	a, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	if !p.is("or") {
		return a, nil
	}
	e := &ex{op: "or", pos: a.pos, kids: []*ex{a}}
	for p.take("or") {
		b, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		e.kids = append(e.kids, b)
	}
	e.end = p.last()
	return e, nil
}

func (p *parser) parseAnd() (*ex, error) {
	a, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	if !p.is("and") {
		return a, nil
	}
	e := &ex{op: "and", pos: a.pos, kids: []*ex{a}}
	for p.take("and") {
		b, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		e.kids = append(e.kids, b)
	}
	e.end = p.last()
	return e, nil
}

func (p *parser) parseNot() (*ex, error) {
	if p.is("not") {
		pos := p.next().pos
		if p.is("exists") {
			e, err := p.parseExists()
			if err != nil {
				return nil, err
			}
			e.not, e.pos = true, pos
			return e, nil
		}
		k, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &ex{op: "not", pos: pos, end: p.last(), kids: []*ex{k}}, nil
	}
	if p.is("exists") {
		return p.parseExists()
	}
	return p.parsePred()
}

func (p *parser) parseExists() (*ex, error) {
	pos := p.next().pos
	if err := p.expectPunct("(", "( after EXISTS"); err != nil {
		return nil, err
	}
	if !p.is("select") {
		return nil, p.failNext("SELECT")
	}
	s, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(")", ") closing the EXISTS"); err != nil {
		return nil, err
	}
	return &ex{op: "exists", pos: pos, end: p.last(), sub: s}, nil
}

// parsePred reads a comparison or a test of one value.
func (p *parser) parsePred() (*ex, error) {
	x, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind == tPunct {
		switch t.val {
		case "=", "<>", "!=", "<", "<=", ">", ">=":
			p.next()
			if p.is("any", "all", "some") {
				return nil, p.fail(p.peek().pos, strings.ToUpper(p.peek().val)+" (…): the designer compares with values")
			}
			y, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			op := t.val
			if op == "!=" {
				op = "<>"
			}
			return &ex{op: "cmp", val: op, pos: x.pos, end: p.last(), kids: []*ex{x, y}}, nil
		case "~", "!~", "~*", "!~*", "@>", "<@", "&&", "@@", "<=>":
			return nil, p.fail(t.pos, "the operator `"+t.val+"`: the designer compares with =, <, >, LIKE and the like")
		}
	}
	if p.is("is") {
		p.next()
		not := p.take("not")
		switch {
		case p.take("null"):
			return &ex{op: "isnull", not: not, pos: x.pos, end: p.last(), kids: []*ex{x}}, nil
		case p.is("true", "false"):
			v := p.next().val
			if not {
				return nil, p.fail(t.pos, "IS NOT "+strings.ToUpper(v)+": write the comparison, with or empty")
			}
			return &ex{op: "cmp", val: "=", pos: x.pos, end: p.last(), kids: []*ex{x, {op: "bool", val: v}}}, nil
		case p.is("distinct"):
			return nil, p.fail(t.pos, "IS [NOT] DISTINCT FROM: write = or <> (with or empty)")
		}
		return nil, p.failNext("NULL after IS")
	}
	not := false
	if p.is("not") && p.peekN(1).kind == tWord {
		switch p.peekN(1).val {
		case "between", "in", "like", "ilike":
			p.next()
			not = true
		}
	}
	switch {
	case p.take("between"):
		if p.is("symmetric") {
			return nil, p.fail(p.peek().pos, "BETWEEN SYMMETRIC: write the smaller value first")
		}
		a, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		if !p.take("and") {
			return nil, p.failNext("AND in BETWEEN")
		}
		b, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		return &ex{op: "between", not: not, pos: x.pos, end: p.last(), kids: []*ex{x, a, b}}, nil
	case p.take("in"):
		if err := p.expectPunct("(", "( after IN"); err != nil {
			return nil, err
		}
		e := &ex{op: "in", not: not, pos: x.pos, kids: []*ex{x}}
		if p.is("select") {
			s, err := p.parseSelect()
			if err != nil {
				return nil, err
			}
			e.sub = s
		} else {
			for {
				v, err := p.parseAdd()
				if err != nil {
					return nil, err
				}
				e.kids = append(e.kids, v)
				if !p.punct(",") {
					break
				}
			}
		}
		if err := p.expectPunct(")", ") closing the IN list"); err != nil {
			return nil, err
		}
		e.end = p.last()
		return e, nil
	case p.is("like", "ilike"):
		kw := p.next().val
		pat, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		e := &ex{op: "like", val: kw, not: not, pos: x.pos, kids: []*ex{x, pat}}
		if p.take("escape") {
			esc, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			e.kids = append(e.kids, esc)
		}
		e.end = p.last()
		return e, nil
	case p.is("similar", "regexp", "rlike", "glob", "match", "sounds"):
		return nil, p.fail(p.peek().pos, strings.ToUpper(p.peek().val)+": the designer's patterns are contains, starts with and ends with")
	}
	return x, nil
}

// parseAdd reads + - || and * / %: kept as arith, which only the date
// arithmetic of "in the last N days" may use (the binder refuses the rest).
func (p *parser) parseAdd() (*ex, error) {
	a, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tPunct || (t.val != "+" && t.val != "-" && t.val != "||" && t.val != "*" && t.val != "/" && t.val != "%") {
			return a, nil
		}
		p.next()
		b, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		a = &ex{op: "arith", val: t.val, pos: a.pos, end: p.last(), kids: []*ex{a, b}}
	}
}

func (p *parser) parseUnary() (*ex, error) {
	if t := p.peek(); t.kind == tPunct && (t.val == "-" || t.val == "+") {
		p.next()
		k, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if t.val == "+" {
			return k, nil
		}
		return &ex{op: "neg", pos: t.pos, end: p.last(), kids: []*ex{k}}, nil
	}
	e, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.punct("::") {
		typ, err := p.typeNext()
		if err != nil {
			return nil, err
		}
		e = &ex{op: "cast", val: typ, pos: e.pos, end: p.last(), kids: []*ex{e}}
	}
	return e, nil
}

// typeNext reads a type name after :: or AS, with its arguments
// (numeric(10,2)), lower-cased. A type of several words takes only the
// words that continue one (double precision, character varying, timestamp
// with time zone), so `x::date d` leaves d to be the alias.
func (p *parser) typeNext() (string, error) {
	t := p.peek()
	if t.kind != tWord && t.kind != tIdent {
		return "", p.failNext("a type")
	}
	p.next()
	words := []string{strings.ToLower(t.val)}
	for {
		n := p.peek()
		if n.kind != tWord {
			break
		}
		prev := words[len(words)-1]
		cont := prev == "double" && n.val == "precision" ||
			(prev == "character" || prev == "char" || prev == "bit") && n.val == "varying" ||
			(prev == "timestamp" || prev == "time") && (n.val == "with" || n.val == "without") ||
			(prev == "with" || prev == "without") && (n.val == "time" || n.val == "local") ||
			prev == "local" && n.val == "time" ||
			prev == "time" && n.val == "zone" && len(words) > 1
		if !cont {
			break
		}
		words = append(words, n.val)
		p.next()
	}
	if p.punct("(") {
		for !p.punct(")") {
			if p.peek().kind == tEOF {
				return "", p.failNext(") closing the type")
			}
			p.next()
		}
	}
	return strings.Join(words, " "), nil
}

// parsePrimary reads one value: a literal, a column, a call, a
// parenthesized expression, or a scalar subquery.
func (p *parser) parsePrimary() (*ex, error) {
	t := p.peek()
	switch t.kind {
	case tStr:
		p.next()
		return &ex{op: "str", val: t.val, pos: t.pos, end: p.last()}, nil
	case tNum:
		p.next()
		return &ex{op: "num", val: t.val, pos: t.pos, end: p.last()}, nil
	case tParam:
		p.next()
		return &ex{op: "param", val: t.val, pos: t.pos, end: p.last()}, nil
	case tPunct:
		switch t.val {
		case "(":
			p.next()
			if p.is("select") {
				s, err := p.parseSelect()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(")", ") closing the subquery"); err != nil {
					return nil, err
				}
				return &ex{op: "sub", pos: t.pos, end: p.last(), sub: s}, nil
			}
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if p.isPunct(",") {
				return nil, p.fail(t.pos, "a row of values (`(a, b)`): compare one column at a time")
			}
			if err := p.expectPunct(")", ")"); err != nil {
				return nil, err
			}
			return e, nil
		case "*":
			p.next()
			return &ex{op: "star", pos: t.pos, end: p.last()}, nil
		}
		return nil, p.failNext("a value")
	case tEOF:
		return nil, p.failNext("a value")
	}
	// a word or a quoted name
	if t.kind == tWord {
		switch t.val {
		case "null":
			p.next()
			return &ex{op: "null", pos: t.pos, end: p.last()}, nil
		case "true", "false":
			p.next()
			return &ex{op: "bool", val: t.val, pos: t.pos, end: p.last()}, nil
		case "case":
			return nil, p.fail(t.pos, "a CASE expression: the designer shows columns and their aggregates")
		case "date", "timestamp", "time", "datetime":
			if n := p.peekN(1); n.kind == tStr {
				p.next()
				p.next()
				return &ex{op: "typed", val: t.val, pos: t.pos, end: p.last(), kids: []*ex{{op: "str", val: n.val, pos: n.pos}}}, nil
			}
		case "interval":
			return p.parseInterval()
		case "cast":
			if p.peekN(1).kind == tPunct && p.peekN(1).val == "(" {
				p.next()
				p.next()
				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				if !p.take("as") {
					return nil, p.failNext("AS in CAST")
				}
				typ, err := p.typeNext()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(")", ") closing CAST"); err != nil {
					return nil, err
				}
				return &ex{op: "cast", val: typ, pos: t.pos, end: p.last(), kids: []*ex{e}}, nil
			}
		case "current_date", "current_timestamp", "localtimestamp", "current_time", "localtime":
			if !(p.peekN(1).kind == tPunct && p.peekN(1).val == "(") {
				p.next()
				return &ex{op: "call", val: t.val, pos: t.pos, end: p.last()}, nil
			}
		}
		if clauseWords[t.val] {
			return nil, p.failNext("a value")
		}
	}
	// a call: name ( … )
	if n := p.peekN(1); n.kind == tPunct && n.val == "(" {
		p.next()
		p.next()
		e := &ex{op: "call", val: strings.ToLower(t.val), pos: t.pos}
		switch {
		case p.punct(")"):
		case p.isPunct("*") && p.peekN(1).kind == tPunct && p.peekN(1).val == ")":
			p.next()
			p.next()
			e.star = true
		default:
			if p.take("distinct") {
				e.distinct = true
			} else {
				p.take("all")
			}
			for {
				a, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				e.kids = append(e.kids, a)
				if !p.punct(",") {
					break
				}
			}
			if p.is("order") {
				return nil, p.fail(p.peek().pos, "ORDER BY inside an aggregate: the designer's aggregates take one column")
			}
			if err := p.expectPunct(")", ") closing "+t.raw+"(…)"); err != nil {
				return nil, err
			}
		}
		if p.is("over") {
			return nil, p.fail(p.peek().pos, "a window function (`"+t.raw+"(…) OVER …`): the designer's aggregates group rows, "+
				"they do not run over a window — compute it in a query tab")
		}
		if p.is("filter") || p.is("within") {
			return nil, p.fail(p.peek().pos, "an aggregate's "+strings.ToUpper(p.peek().val)+" clause: filter the rows instead")
		}
		e.end = p.last()
		return e, nil
	}
	// a column, or t.*
	e := &ex{op: "col", pos: t.pos}
	for {
		name, quoted, err := p.nameNext("a column")
		if err != nil {
			return nil, err
		}
		e.parts = append(e.parts, name)
		e.quoted = append(e.quoted, quoted)
		if !p.punct(".") {
			break
		}
		if p.punct("*") {
			e.op = "star"
			break
		}
	}
	if len(e.parts) > 3 {
		return nil, p.fail(t.pos, "a column named by more than schema, table and column")
	}
	e.end = p.last()
	return e, nil
}

// parseInterval reads INTERVAL 'N days' (Postgres) or INTERVAL N DAY
// (MySQL), as "N days" / "N day".
func (p *parser) parseInterval() (*ex, error) {
	t := p.next()
	n := p.peek()
	switch n.kind {
	case tStr:
		p.next()
		v := strings.ToLower(strings.TrimSpace(n.val))
		if p.is("day", "days") { // INTERVAL '7' DAY
			v += " " + p.next().val
		}
		return &ex{op: "interval", val: v, pos: t.pos, end: p.last()}, nil
	case tNum:
		p.next()
		if !p.is("day", "days", "hour", "minute", "second", "month", "year", "week") {
			return nil, p.failNext("a unit after INTERVAL " + n.raw)
		}
		return &ex{op: "interval", val: n.val + " " + p.next().val, pos: t.pos, end: p.last()}, nil
	}
	return nil, p.failNext("the interval's length")
}
