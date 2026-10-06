package sqlcomplete

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// The resolver: what a name in a SQL buffer refers to, where that is
// declared and where else it is used — the answers an editor's go to
// definition, find usages and rename need. It reads the statement with the
// same scanner completion does (tokenize, parseRefs, parseCTEs), so the two
// agree on what a table reference and its alias are.
//
// # What it resolves
//
//	alias   FROM orders o … o.id      declared by the alias, used as a qualifier
//	cte     WITH t AS (…) … FROM t    declared by WITH, used as a table and,
//	        … t.n                     when not aliased, as a qualifier
//	table   FROM orders … orders.id   a catalog table brought in without an
//	                                  alias; found but never renamed, since
//	                                  renaming the text would not rename the
//	                                  table, only break the query
//	column  WITH t AS (SELECT         a column the statement itself names:
//	          count(*) AS n …)        a CTE's or a derived table's output
//	        SELECT t.n … ORDER BY n   column, or a select-list alias that
//	                                  ORDER BY uses
//
// Catalog columns are not resolved: a bare column could belong to any
// table in scope, and telling which needs the catalog. See "Columns" below
// for the ones that are.
//
// # Blocks: where a name is visible
//
// Completion treats every table a statement names as in scope everywhere
// in it, which is harmless for a suggestion list. A rename cannot be that
// loose: two subqueries that each alias a table "o" must not be renamed
// together. Names are therefore declared in a block, and a qualifier
// resolves to the innermost block around it that declares the name, as
// SQL resolves a correlated subquery's reference outward.
//
//	SELECT o.id FROM orders o                    ─┐ block 0 (the statement)
//	WHERE EXISTS (SELECT 1 FROM items o          ─┼─┐ block 1: its own "o"
//	              WHERE o.order_id = 7)          ─┼─┘
//	UNION SELECT o.id FROM archive o             ─┘ block 2: block 0's frame,
//	                                                cut at UNION
//
// A block is opened by a parenthesis that starts a query (SELECT, WITH or
// VALUES follows it) — a function's parentheses are not a block — and cut
// at UNION / INTERSECT / EXCEPT. The pieces of one parenthesis are one
// frame: aliases belong to a block, CTEs to the frame, since a WITH's names
// are visible across every arm of the set operation after it.
//
// Like completion this is a scanner, not a parse. It does not see a
// parenthesized join's aliases outside its parentheses, nor a bare alias
// used as a whole-row value (SELECT o FROM orders o) or as MySQL's
// DELETE o FROM orders o target; those are left alone by a rename.
//
// # Columns
//
// A query's output columns are read from its SELECT list (the first arm's,
// for a UNION), one per item that has a name:
//
//	SELECT count(*) AS n,   an alias: declared here, renamable
//	       o.status,        a reference: declared here, but the name is
//	       id,              the referenced column's; it passes that
//	       d.*              column through (via) rather than being one
//	FROM orders o, (…) d    — a star passes through every column of d
//
// A CTE's or derived table's columns are its body's output columns, or the
// names in its column list (WITH t(a, b) AS …, (…) AS d(a, b)), which win.
// A reference to a column is resolved like this:
//
//	t.n     the handle t names → its relation → its column n
//	n       in ORDER BY, alone as an item: the block's own output column
//	        (SQL lets an output name win there); otherwise the handles of
//	        the innermost block that has one with an n — only when every
//	        handle in that block has known columns: one catalog table in
//	        scope and a bare n could be its, so nothing is resolved
//
// The bodies of CTEs and of derived tables in FROM are sealed: a bare name
// in one never resolves outward, since SQL does not let it see the
// statement's FROM (LATERAL aside, which stays unsealed).
//
// Every pass-through ends at an origin: an alias, a column list's name, or
// a select item naming a catalog table's column. A symbol is that origin,
// and its uses are every token whose chain of passes ends there — so
// renaming a.n in
//
//	WITH a AS (SELECT 1 AS n), b AS (SELECT n FROM a) SELECT b.n FROM b
//	                     ^              ^                  ^
//
// renames all three, which keeps b's column following a's. An origin that
// is a catalog column is found but not renamed, as a table is.

// SymbolKind is what a resolved name is.
type SymbolKind string

const (
	SymAlias  SymbolKind = "alias"  // a table reference's alias
	SymCTE    SymbolKind = "cte"    // a WITH query's name
	SymTable  SymbolKind = "table"  // a table referenced without an alias
	SymColumn SymbolKind = "column" // a column the statement itself names
)

// Span is a byte range of the buffer, a token's: a quoted name's quotes
// included.
type Span struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// Symbol is the answer to Resolve. A zero Kind means the caret is on
// nothing the resolver knows: a column, a keyword, blank space.
type Symbol struct {
	Kind SymbolKind `json:"kind"`
	Name string     `json:"name"` // as declared, without quotes
	At   Span       `json:"at"`   // the occurrence under the caret
	Def  Span       `json:"def"`  // where it is declared
	Uses []Span     `json:"uses"` // every occurrence, Def included, in buffer order
	// Fixed says why the name cannot be renamed here; "" when it can.
	Fixed string `json:"fixed,omitempty"`
}

// Edit replaces [From, To) of the buffer with Text.
type Edit struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Text string `json:"text"`
}

// Resolve finds the symbol under caret (a byte offset into buf) in the
// statement around it. It never fails: anything it cannot place is a zero
// Symbol.
func Resolve(buf string, caret int) Symbol {
	r, k := newResolver(buf, caret)
	if r == nil {
		return Symbol{}
	}
	return r.symbolAt(k)
}

// Rename is the edits that rename the symbol under caret to name, quoted as
// driver's dialect needs (a capital on Postgres, a reserved word, a space).
// A name the user quoted already is used as written. The error is a
// sentence for the user: nothing renamable is under the caret, the symbol
// is a table (or a table's column), or the name is taken in the same query.
func Rename(buf string, caret int, name, driver string) ([]Edit, error) {
	r, k := newResolver(buf, caret)
	var sym Symbol
	if r != nil {
		sym = r.symbolAt(k)
	}
	switch {
	case sym.Kind == "":
		return nil, fmt.Errorf("nothing to rename here: rename %s", NotOnSymbol)
	case sym.Fixed != "":
		return nil, fmt.Errorf("%s", sym.Fixed)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("the new name is empty")
	}
	text, bare := name, name
	if quotedName(name) {
		bare = unquote(name)
	} else {
		text = dialectFor(driver).quote(name)
	}
	if r.taken(sym, bare) {
		return nil, fmt.Errorf("%s is already a name in this query", bare)
	}
	edits := make([]Edit, len(sym.Uses))
	for i, u := range sym.Uses {
		edits[i] = Edit{From: u.From, To: u.To, Text: text}
	}
	return edits, nil
}

// NotOnSymbol finishes the sentence for a caret on nothing the resolver
// knows ("rename works on …", "F12 works on …"), so every editor says it in
// the same words.
const NotOnSymbol = "works on a table alias, a CTE name, or a column the query names itself"

// quotedName reports whether s is a single quoted identifier, "a b" or
// `a b`, which a rename uses as written.
func quotedName(s string) bool {
	toks := sqlsplit.Lex(s)
	return len(toks) == 1 && toks[0].Kind == sqlsplit.TokIdent && toks[0].Start == 0 &&
		toks[0].End == len(s) && closed(s)
}

// ---------------------------------------------------------------------------
// Reading the statement
// ---------------------------------------------------------------------------

// handle is a name a table reference can be qualified by: its alias, or
// the last part of its name when it has none.
type handle struct {
	name  string
	tok   int // the token declaring it: the alias, or the name's last part
	block int
	alias bool
	cte   *cteDecl // the CTE the reference names, if it names one
	// rel is the columns of what the reference names when the statement
	// defines it (a CTE, a derived table, an alias's column list); nil for
	// a catalog table, whose columns the resolver does not know.
	rel *colSet
}

// cteDecl is a WITH query's name and the frame it is visible in.
type cteDecl struct {
	name  string
	tok   int
	frame int
	rel   *colSet // its columns
}

// column is one output column of a query (see "Columns" above).
type column struct {
	name string
	tok  int     // the token naming it; -1 when a star brought it in
	via  *column // the column it passes through; nil for an origin
	// named: declared by the query itself (an alias, a column list) — an
	// origin that can be renamed. An origin that is not named is a select
	// item naming a catalog table's column.
	named bool
}

// fixed reports whether c is an origin the query does not own: a table's
// column, found but not renamed.
func (c *column) fixed() bool { return c.via == nil && !c.named }

// colSet is the columns of a relation or a query: read on demand from a
// frame's SELECT list (frame >= 0), or given by a column list.
type colSet struct {
	frame int
	cols  []*column
	// opaque: it has columns that cannot be known — a star over a catalog
	// table, a VALUES list — so a name it lacks may still be one of its.
	opaque bool
	state  int // 0 unread, 1 being read (a cycle reads as opaque), 2 read
}

type resolver struct {
	toks []tok

	block  []int  // per token: the block it is in
	direct []bool // per token: not inside a function's (non-query) parentheses
	parent []int  // per block: the block around it, -1 for the statement's
	frame  []int  // per block: its frame

	handles []*handle
	ctes    []*cteDecl
	// refPart marks the tokens of a table reference's name (sales.orders:
	// both), which are not qualifiers even though a "." follows one.
	refPart map[int]bool
	// nameOf is the reference whose name's last part is the token, for
	// every reference with a name; its handle when it has no alias.
	nameOf map[int]*handle
	// declTok marks the tokens that declare a handle or a CTE, which are
	// never column references.
	declTok map[int]bool

	// Columns (see "Columns" above).
	base    []int           // per block: the paren depth of its own tokens
	clause  []string        // per token: the clause keyword it is under, in its block
	selTok  map[int]int     // per frame: its SELECT keyword, when it has one
	sealed  map[int]bool    // frames a bare name does not resolve out of
	sets    map[int]*colSet // per frame: its output columns
	allSets []*colSet       // every set, the column lists' too
	// declared is the column a select item or column list declares, by
	// the token naming it.
	declared map[int]*column
	// renTarget and renTo, while a rename is tried out (taken), make every
	// column whose origin is renTarget answer to renTo.
	renTarget *column
	renTo     string
}

// newResolver reads the statement around caret, and the index of the name
// token under it; nil when the caret is not on a name.
func newResolver(buf string, caret int) (*resolver, int) {
	caret = min(max(caret, 0), len(buf))
	start, end := stmtWindow(buf, caret)
	toks := tokenize(buf, start, end)
	k := -1
	for i, t := range toks {
		if t.isName() && t.start <= caret && caret <= t.end {
			k = i
			break
		}
	}
	if k < 0 {
		return nil, -1
	}
	r := &resolver{
		toks: toks, refPart: map[int]bool{}, nameOf: map[int]*handle{}, declTok: map[int]bool{},
		selTok: map[int]int{}, sealed: map[int]bool{}, sets: map[int]*colSet{}, declared: map[int]*column{},
	}
	r.blocks()
	r.clauses()
	r.declarations()
	r.columns()
	return r, k
}

// newBlock opens a block inside parent (-1: none) in frame, or in a frame
// of its own when frame is -1. A new frame takes the block's own id, which
// no other frame has.
func (r *resolver) newBlock(parent, frame int) int {
	if frame < 0 {
		frame = len(r.parent)
	}
	r.parent = append(r.parent, parent)
	r.frame = append(r.frame, frame)
	return len(r.parent) - 1
}

// blocks assigns every token its block (see the package comment above).
// A parenthesis belongs to the block outside it.
func (r *resolver) blocks() {
	r.block = make([]int, len(r.toks))
	r.direct = make([]bool, len(r.toks))
	type open struct {
		query bool
		block int // the block the parenthesis opened in
	}
	var stack []open
	cur := r.newBlock(-1, -1)
	for i, t := range r.toks {
		inQuery := len(stack) == 0 || stack[len(stack)-1].query
		switch {
		case t.isPunct("("):
			q := i+1 < len(r.toks) && r.toks[i+1].kind == tWord &&
				(r.toks[i+1].low == "select" || r.toks[i+1].low == "with" || r.toks[i+1].low == "values")
			r.block[i], r.direct[i] = cur, inQuery
			stack = append(stack, open{query: q, block: cur})
			if q {
				cur = r.newBlock(cur, -1)
			}
			continue
		case t.isPunct(")"):
			if len(stack) > 0 {
				cur = stack[len(stack)-1].block
				stack = stack[:len(stack)-1]
			}
			r.block[i] = cur
			r.direct[i] = len(stack) == 0 || stack[len(stack)-1].query
			continue
		case inQuery && t.kind == tWord && (t.low == "union" || t.low == "intersect" || t.low == "except"):
			// the next arm: a block of its own in the same frame, so
			// its aliases are its own but the WITH's names still reach it
			cur = r.newBlock(r.parent[cur], r.frame[cur])
		}
		r.block[i], r.direct[i] = cur, inQuery
	}
}

// declarations reads every WITH's names and every FROM list's references,
// in subqueries too. completion's parseScope skips over the bodies of CTEs
// and derived tables, so the lists are found here by visiting every
// keyword, and parseRefs / parseCTEs are asked to read each one.
func (r *resolver) declarations() {
	// parseRefs resolves names against a catalog; with none it finds no
	// tables, which the resolver does not need
	c := &completer{d: dialectFor("")}
	c.index()
	var refs []*ref
	for i, t := range r.toks {
		if t.kind != tWord || !r.direct[i] {
			continue // FROM in extract(year FROM d) is not a table list
		}
		switch t.low {
		case "with":
			sc := &scope{}
			sc.parseCTEs(r.toks, i+1, c)
			for _, ct := range sc.ctes {
				if r.isCTE(ct.tok) {
					r.ctes = append(r.ctes, &cteDecl{name: ct.name, tok: ct.tok,
						frame: r.frame[r.block[ct.tok]], rel: r.cteRel(ct.tok)})
					r.declTok[ct.tok] = true
				}
			}
		case "from", "join", "update", "into", "using", "truncate":
			if t.low == "from" && i > 0 && r.toks[i-1].kind == tWord && r.toks[i-1].low == "distinct" {
				continue // a IS DISTINCT FROM b: an operator
			}
			sc := &scope{}
			sc.parseRefs(r.toks, i+1, t.low == "from" || t.low == "using", c)
			refs = append(refs, sc.refs...)
		}
	}
	for _, rf := range refs {
		var h *handle
		var rel *colSet
		if len(rf.parts) > 0 {
			for j := range rf.parts {
				r.refPart[rf.nameTok-2*j] = true
			}
			h = &handle{name: rf.parts[len(rf.parts)-1], tok: rf.nameTok, block: r.block[rf.nameTok]}
			if len(rf.parts) == 1 {
				h.cte = r.findCTE(rf.nameTok, h.name)
			}
			if h.cte != nil {
				rel = h.cte.rel
			}
			h.rel = rel
			r.nameOf[rf.nameTok] = h
		} else {
			// a derived table: its body's columns. Its body cannot see
			// the FROM it sits in, unless it is LATERAL.
			rel = r.bodyRel(rf.subTok)
			if !(rf.subTok > 0 && r.toks[rf.subTok-1].kind == tWord && r.toks[rf.subTok-1].low == "lateral") {
				r.seal(rf.subTok)
			}
		}
		if rf.alias != "" {
			a := &handle{name: rf.alias, tok: rf.aliasTok, block: r.block[rf.aliasTok], alias: true, rel: rel}
			if h != nil {
				a.cte = h.cte
			}
			// AS d(a, b): the list names the columns, whatever the
			// body calls them
			if j := rf.aliasTok + 1; j < len(r.toks) && r.toks[j].isPunct("(") {
				a.rel = r.listRel(j)
			}
			r.handles = append(r.handles, a)
			r.declTok[rf.aliasTok] = true
		} else if h != nil {
			r.handles = append(r.handles, h)
		}
	}
}

// isCTE reports whether the name at toks[i] is followed by what a CTE's is,
// [(cols)] AS: parseCTEs takes the word after any WITH for a name, and
// "timestamp WITH time zone" or "WITH ORDINALITY" declare nothing.
func (r *resolver) isCTE(i int) bool {
	j := i + 1
	if j < len(r.toks) && r.toks[j].isPunct("(") {
		j = matching(r.toks, j) + 1
	}
	return j < len(r.toks) && r.toks[j].kind == tWord && r.toks[j].low == "as"
}

// findCTE is the CTE a table name at toks[i] refers to, searching the
// frames around it from the inside out; nil when it names none.
func (r *resolver) findCTE(i int, name string) *cteDecl {
	for b := r.block[i]; b >= 0; b = r.parent[b] {
		for _, c := range r.ctes {
			if c.frame == r.frame[b] && c.tok != i && strings.EqualFold(c.name, name) {
				return c
			}
		}
	}
	return nil
}

// qualifier reports whether toks[i] qualifies what follows it (o in o.id,
// o.*): a name before a "." that is not itself after one and not part of a
// table reference's name.
func (r *resolver) qualifier(i int) bool {
	t := r.toks
	return t[i].isName() && i+1 < len(t) && t[i+1].isPunct(".") &&
		!(i > 0 && t[i-1].isPunct(".")) && !r.refPart[i]
}

// lookup is the handle a qualifier at toks[i] names: the innermost block
// around it that declares the name wins.
func (r *resolver) lookup(i int) *handle {
	name := r.toks[i].text
	for b := r.block[i]; b >= 0; b = r.parent[b] {
		for _, h := range r.handles {
			if h.block == b && strings.EqualFold(h.name, name) {
				return h
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Columns
// ---------------------------------------------------------------------------

// clauseWords start a clause of a query block; clauses records, per token,
// the last one seen at its block's own depth, which is how ORDER BY's
// items are told from the select list's (orderItem).
var clauseWords = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`select from where group having order limit offset fetch window
		union intersect except returning set values on using join into for`) {
		m[w] = true
	}
	return m
}()

// clauses fills base, clause and selTok. A block's base depth is its
// first token's: a parenthesized block's tokens sit one deeper than its
// "(", and anything deeper still is inside a function call or a list.
func (r *resolver) clauses() {
	r.base = make([]int, len(r.parent))
	for b := range r.base {
		r.base[b] = -1
	}
	r.clause = make([]string, len(r.toks))
	cur := map[int]string{}
	for i, t := range r.toks {
		b := r.block[i]
		if r.base[b] < 0 {
			r.base[b] = t.depth
		}
		if t.kind == tWord && t.depth == r.base[b] && clauseWords[t.low] {
			cur[b] = t.low
			// a frame's columns are its first arm's select list
			if _, seen := r.selTok[b]; t.low == "select" && !seen && r.frame[b] == b {
				r.selTok[b] = i
			}
		}
		r.clause[i] = cur[b]
	}
}

// columns reads every frame's output columns up front: reading declares
// the select items' tokens (declared), which finding a symbol's uses walks.
// The reads are memoized, so a frame read early, through a reference to
// it, is not read twice.
func (r *resolver) columns() {
	for b := range r.parent {
		if r.frame[b] == b {
			r.read(r.frameSet(b))
		}
	}
}

// frameSet is frame f's output columns, unread until read is asked.
func (r *resolver) frameSet(f int) *colSet {
	s := r.sets[f]
	if s == nil {
		s = &colSet{frame: f}
		r.sets[f] = s
		r.allSets = append(r.allSets, s)
	}
	return s
}

// opened is the block a "(" at toks[i] opens, or -1 when it opens none
// (a function's or a list's parentheses).
func (r *resolver) opened(i int) int {
	if i+1 < len(r.toks) && r.block[i+1] != r.block[i] {
		return r.block[i+1]
	}
	return -1
}

// bodyRel is the columns of a CTE or derived-table body whose "(" is at
// toks[i]: its frame's select list, or opaque when the parenthesis holds
// no query the scanner sees as one.
func (r *resolver) bodyRel(i int) *colSet {
	if i < len(r.toks) && r.toks[i].isPunct("(") {
		if b := r.opened(i); b >= 0 {
			return r.frameSet(r.frame[b])
		}
	}
	s := &colSet{frame: -1, opaque: true, state: 2}
	r.allSets = append(r.allSets, s)
	return s
}

// seal marks the body opened at toks[i] as one a bare name does not
// resolve out of.
func (r *resolver) seal(i int) {
	if i < len(r.toks) && r.toks[i].isPunct("(") {
		if b := r.opened(i); b >= 0 {
			r.sealed[r.frame[b]] = true
		}
	}
}

// listRel is a column list, (a, b) at toks[i]: each name an origin the
// query declares.
func (r *resolver) listRel(i int) *colSet {
	s := &colSet{frame: -1, state: 2}
	end := matching(r.toks, i)
	for j := i + 1; j < end && j < len(r.toks); j++ {
		if r.toks[j].isName() {
			c := &column{name: r.toks[j].text, tok: j, named: true}
			s.cols = append(s.cols, c)
			r.declared[j] = c
		}
	}
	r.allSets = append(r.allSets, s)
	return s
}

// cteRel is the columns of the CTE named at toks[i]: its column list when
// it has one, else its body's. The body is sealed either way.
func (r *resolver) cteRel(i int) *colSet {
	var list *colSet
	j := i + 1
	if j < len(r.toks) && r.toks[j].isPunct("(") {
		list = r.listRel(j)
		j = matching(r.toks, j) + 1
	}
	for j < len(r.toks) && r.toks[j].kind == tWord &&
		(r.toks[j].low == "as" || r.toks[j].low == "not" || r.toks[j].low == "materialized") {
		j++
	}
	r.seal(j)
	if list != nil {
		return list
	}
	return r.bodyRel(j)
}

// read reads s's columns if they are not yet read. A set being read when
// it is asked for again — WITH t AS (SELECT n FROM t) — reads as opaque
// and empty, which resolves nothing through it.
func (r *resolver) read(s *colSet) *colSet {
	switch s.state {
	case 1:
		return &colSet{frame: -1, opaque: true, state: 2}
	case 0:
		s.state = 1
		r.readSelect(s)
		s.state = 2
	}
	return s
}

// selectEnds end a select list.
var selectEnds = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`from into where group having window order limit offset fetch
		union intersect except for returning`) {
		m[w] = true
	}
	return m
}()

// readSelect reads s's frame's select list into columns, one per item that
// has a name (see "Columns" above). A frame without one (VALUES, UPDATE) is
// opaque.
func (r *resolver) readSelect(s *colSet) {
	sel, ok := r.selTok[s.frame]
	if !ok {
		s.opaque = true
		return
	}
	t := r.toks
	d := t[sel].depth
	var items [][]int
	var cur []int
	for j := sel + 1; j < len(t) && t[j].depth >= d; j++ {
		if t[j].depth == d {
			if t[j].kind == tWord && selectEnds[t[j].low] || t[j].isPunct(";") {
				break
			}
			if t[j].isPunct(",") {
				items, cur = append(items, cur), nil
				continue
			}
		}
		cur = append(cur, j)
	}
	for _, it := range append(items, cur) {
		r.readItem(s, it)
	}
}

// readItem reads one select item, its tokens' indexes.
func (r *resolver) readItem(s *colSet, it []int) {
	t := r.toks
	// DISTINCT / ALL / DISTINCT ON (…) lead the first item
	for len(it) > 0 && t[it[0]].kind == tWord && (t[it[0]].low == "distinct" || t[it[0]].low == "all") {
		it = it[1:]
		if len(it) > 1 && t[it[0]].kind == tWord && t[it[0]].low == "on" && t[it[1]].isPunct("(") {
			end := matching(t, it[1])
			for len(it) > 0 && it[0] <= end {
				it = it[1:]
			}
		}
	}
	n := len(it)
	if n == 0 {
		return
	}
	last := it[n-1]
	if t[last].isPunct("*") {
		// * is every handle's columns in FROM order, t.* the one's; a
		// catalog table's are unknown, which makes the set opaque
		var from []*handle
		switch {
		case n == 1:
			for _, h := range r.handles {
				if h.block == s.frame {
					from = append(from, h)
				}
			}
			slices.SortFunc(from, func(a, b *handle) int { return a.tok - b.tok })
		case n == 3 && t[it[1]].isPunct("."):
			if h := r.lookup(it[0]); h != nil {
				from = []*handle{h}
			} else {
				s.opaque = true
			}
		default:
			s.opaque = true
		}
		for _, h := range from {
			if h.rel == nil {
				s.opaque = true
				continue
			}
			src := r.read(h.rel)
			s.opaque = s.opaque || src.opaque
			for _, c := range src.cols {
				s.cols = append(s.cols, &column{name: c.name, tok: -1, via: c})
			}
		}
		return
	}
	if !t[last].isName() || t[last].kind == tWord && reserved[t[last].low] {
		return // an expression with no name of its own: a + b, CASE … END
	}
	c := &column{name: t[last].text, tok: last}
	switch p := t[it[max(n-2, 0)]]; {
	case n == 1:
		if r.bare(last) {
			c.via = r.bareCol(last, c.name)
		}
	case p.isPunct("."):
		if n == 3 && r.qualifier(it[0]) {
			c.via = r.qualCol(it[0], c.name)
		}
	case p.kind == tWord && (p.low == "as" || p.low == "end" || !reserved[p.low]),
		p.kind == tQuoted, p.kind == tOther, p.isPunct(")"):
		c.named = true // expr AS n, expr n
	default:
		return // x::int, -x, NOT x: no name of its own
	}
	s.cols = append(s.cols, c)
	r.declared[last] = c
}

// notColumn are words that are not reserved but are keywords where they
// stand often enough (NULLS FIRST, ROWS BETWEEN … PRECEDING, AT TIME ZONE)
// that taking one for a column of the same name would rename a keyword.
var notColumn = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`first last next rows row only ties recursive materialized
		preceding following current unbounded over filter within interval distinct exists
		between like ilike similar escape collate at zone true false unknown`) {
		m[w] = true
	}
	return m
}()

// bare reports whether toks[i] could be a bare column reference: a name
// that is not a keyword, a table's name or alias, a CTE's name, a type
// (x::n), an alias being given (AS n), or extract's field.
func (r *resolver) bare(i int) bool {
	t := r.toks[i]
	if !t.isName() || t.kind == tWord && (reserved[t.low] || notColumn[t.low]) {
		return false
	}
	if r.refPart[i] || r.declTok[i] || r.nameOf[i] != nil {
		return false
	}
	if i > 0 {
		p := r.toks[i-1]
		if p.isPunct("::") || p.kind == tWord && (p.low == "as" || p.low == "nulls") {
			return false
		}
		if p.isPunct("(") && i > 1 && r.toks[i-2].kind == tWord && r.toks[i-2].low == "extract" {
			return false
		}
	}
	// date '2024-01-01': a type naming a literal
	if i+1 < len(r.toks) && r.toks[i+1].kind == tOther && strings.HasPrefix(r.toks[i+1].text, "'") {
		return false
	}
	return true
}

// orderItem reports whether toks[i] is an ORDER BY item on its own (ORDER
// BY n, ORDER BY a, n DESC), where SQL lets an output column's name win
// over the input's. Inside an expression (ORDER BY n + 1) it does not.
var orderNext = map[string]bool{"asc": true, "desc": true, "nulls": true, "limit": true, "offset": true,
	"fetch": true, "for": true, "union": true, "intersect": true, "except": true, "using": true}

func (r *resolver) orderItem(i int) bool {
	t := r.toks
	if r.clause[i] != "order" || t[i].depth != r.base[r.block[i]] || i == 0 ||
		!(t[i-1].isPunct(",") || t[i-1].kind == tWord && t[i-1].low == "by") {
		return false
	}
	if i+1 == len(t) {
		return true
	}
	nx := t[i+1]
	return nx.isPunct(",") || nx.isPunct(")") || nx.isPunct(";") || nx.kind == tWord && orderNext[nx.low]
}

// colName is c's name, or the name being tried for it in a rename.
func (r *resolver) colName(c *column) string {
	if r.renTarget != nil && r.root(c) == r.renTarget {
		return r.renTo
	}
	return c.name
}

// root is the origin c passes through to.
func (r *resolver) root(c *column) *column {
	for c != nil && c.via != nil {
		c = c.via
	}
	return c
}

// pick is the column of cols named name, and how many there are.
func (r *resolver) pick(cols []*column, name string) (*column, int) {
	var found *column
	n := 0
	for _, c := range cols {
		if strings.EqualFold(r.colName(c), name) {
			found, n = c, n+1
		}
	}
	return found, n
}

// qualCol is column name of what the qualifier at toks[q] names; nil when
// that is a catalog table or has no such column, or two.
func (r *resolver) qualCol(q int, name string) *column {
	h := r.lookup(q)
	if h == nil || h.rel == nil {
		return nil
	}
	if c, n := r.pick(r.read(h.rel).cols, name); n == 1 {
		return c
	}
	return nil
}

// bareCol is the column a bare name at toks[i] refers to: the block's own
// output column in an ORDER BY item; else the one column of that name
// among the handles of the innermost block that has one. Nothing when a
// block on the way has a handle whose columns are unknown, or two
// columns of the name, or when the walk would leave a sealed body.
func (r *resolver) bareCol(i int, name string) *column {
	b := r.block[i]
	if r.orderItem(i) {
		switch c, n := r.pick(r.read(r.frameSet(r.frame[b])).cols, name); n {
		case 1:
			return c
		case 0:
		default:
			return nil // ORDER BY n with two output columns named n
		}
	}
	for ; b >= 0; b = r.parent[b] {
		var found *column
		n, unknown := 0, false
		for _, h := range r.handles {
			if h.block != b {
				continue
			}
			if h.rel == nil {
				unknown = true
				continue
			}
			s := r.read(h.rel)
			if c, m := r.pick(s.cols, name); m > 0 {
				found, n = c, n+m
			} else if s.opaque {
				unknown = true
			}
		}
		switch {
		case unknown || n > 1:
			return nil
		case n == 1:
			return found
		case r.sealed[r.frame[b]]:
			return nil
		}
	}
	return nil
}

// refCol is the column toks[i] refers to as a reference, reading its text
// as name: t.n through t, a bare n through bareCol.
func (r *resolver) refCol(i int, name string) *column {
	t := r.toks
	if !t[i].isName() || i+1 < len(t) && (t[i+1].isPunct(".") || t[i+1].isPunct("(")) {
		return nil // a qualifier, a function
	}
	if i >= 2 && t[i-1].isPunct(".") {
		if r.qualifier(i - 2) {
			return r.qualCol(i-2, name)
		}
		return nil // schema.table.column: a catalog column
	}
	if !r.bare(i) {
		return nil
	}
	return r.bareCol(i, name)
}

// colAt is the column toks[i] declares or refers to; nil for none.
func (r *resolver) colAt(i int) *column {
	if c := r.declared[i]; c != nil {
		return c
	}
	return r.refCol(i, r.toks[i].text)
}

// colUses is the tokens whose column passes through to origin, the
// origin's own included.
func (r *resolver) colUses(origin *column) []int {
	uses := []int{origin.tok}
	for i := range r.toks {
		if c := r.colAt(i); c != nil && r.root(c) == origin {
			uses = append(uses, i)
		}
	}
	return uses
}

// columnSymbol is a column: declared at its origin, used wherever a
// reference passes through to it. A table's column that only a select item
// names, and nothing refers to, is not the statement's: nothing is found
// there, as before columns were resolved.
func (r *resolver) columnSymbol(k int, c *column) Symbol {
	origin := r.root(c)
	uses := r.colUses(origin)
	if origin.fixed() && len(slices.Compact(slices.Sorted(slices.Values(uses)))) < 2 {
		return Symbol{}
	}
	sym := r.symbol(SymColumn, k, origin.tok)
	if origin.fixed() {
		sym.Fixed = fmt.Sprintf("%s is a table's column: renaming it here would not rename it in the database, "+
			"only stop the query finding it — give it an alias (… AS new_name) and rename that", origin.name)
	}
	sym.Uses = r.spans(uses)
	return sym
}

// columnTaken tries renaming origin to name: it is taken when a column set
// holding origin would hold another column of the name, or when any
// reference would then resolve differently — a use of origin to something
// else or to nothing, or another name to origin.
func (r *resolver) columnTaken(origin *column, uses []Span, name string) bool {
	use := map[int]bool{}
	for i, t := range r.toks {
		for _, u := range uses {
			if t.start == u.From {
				use[i] = true
			}
		}
	}
	before := make([]*column, len(r.toks))
	for i := range r.toks {
		before[i] = r.root(r.colAt(i))
	}
	r.renTarget, r.renTo = origin, name
	defer func() { r.renTarget, r.renTo = nil, "" }()
	for _, s := range r.allSets {
		mine, same := false, 0
		for _, c := range s.cols {
			mine = mine || r.root(c) == origin
			if strings.EqualFold(r.colName(c), name) {
				same++
			}
		}
		if mine && same > 1 {
			return true
		}
	}
	for i := range r.toks {
		text := r.toks[i].text
		if use[i] {
			text = name
		}
		// an origin is itself whatever it is called; anything else is a
		// reference, read again under the new name
		after := r.declared[i]
		if after == nil || after.via != nil {
			after = r.refCol(i, text)
		}
		if r.root(after) != before[i] {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The symbol and its uses
// ---------------------------------------------------------------------------

// symbolAt classifies the name token toks[k] and gathers its occurrences.
func (r *resolver) symbolAt(k int) Symbol {
	// a CTE's own name
	for _, c := range r.ctes {
		if c.tok == k {
			return r.cteSymbol(k, c)
		}
	}
	// an alias where it is given
	for _, h := range r.handles {
		if h.alias && h.tok == k {
			return r.handleSymbol(k, h)
		}
	}
	// a table reference's name
	if h := r.nameOf[k]; h != nil {
		if h.cte != nil {
			return r.cteSymbol(k, h.cte)
		}
		return r.handleSymbol(k, h)
	}
	// a qualifier
	if r.qualifier(k) {
		if h := r.lookup(k); h != nil {
			if !h.alias && h.cte != nil {
				return r.cteSymbol(k, h.cte)
			}
			return r.handleSymbol(k, h)
		}
	}
	// a column the statement names
	if c := r.colAt(k); c != nil {
		return r.columnSymbol(k, c)
	}
	return Symbol{}
}

// handleSymbol is an alias, or a table referenced without one: declared
// where it is written, used by the qualifiers that resolve to it.
func (r *resolver) handleSymbol(k int, h *handle) Symbol {
	sym := r.symbol(SymAlias, k, h.tok)
	if !h.alias {
		sym.Kind = SymTable
		sym.Fixed = fmt.Sprintf("%s is a table: renaming it here would not rename it in the database, "+
			"only stop the query finding it — give it an alias and rename that", h.name)
	}
	uses := []int{h.tok}
	for i := range r.toks {
		if i != h.tok && r.qualifier(i) && r.lookup(i) == h {
			uses = append(uses, i)
		}
	}
	sym.Uses = r.spans(uses)
	return sym
}

// cteSymbol is a CTE: declared by its WITH, used as a table name, and as
// the qualifier of a reference to it that has no alias.
func (r *resolver) cteSymbol(k int, c *cteDecl) Symbol {
	sym := r.symbol(SymCTE, k, c.tok)
	uses := []int{c.tok}
	for i, h := range r.nameOf {
		if h.cte == c {
			uses = append(uses, i)
		}
	}
	for i := range r.toks {
		if r.qualifier(i) {
			if h := r.lookup(i); h != nil && !h.alias && h.cte == c {
				uses = append(uses, i)
			}
		}
	}
	sym.Uses = r.spans(uses)
	return sym
}

func (r *resolver) symbol(kind SymbolKind, at, def int) Symbol {
	return Symbol{
		Kind: kind,
		Name: r.toks[def].text,
		At:   Span{r.toks[at].start, r.toks[at].end},
		Def:  Span{r.toks[def].start, r.toks[def].end},
	}
}

// spans is the token indexes' byte ranges, in buffer order, once each.
func (r *resolver) spans(idx []int) []Span {
	slices.Sort(idx)
	idx = slices.Compact(idx)
	out := make([]Span, len(idx))
	for i, j := range idx {
		out[i] = Span{r.toks[j].start, r.toks[j].end}
	}
	return out
}

// taken reports whether renaming sym to name would collide with a name the
// query already declares beside it: another alias or table in the same
// block, another CTE of the same WITH, or for a column whatever
// columnTaken finds. A name in an outer block is
// shadowed, legally, so it does not count.
func (r *resolver) taken(sym Symbol, name string) bool {
	switch sym.Kind {
	case SymAlias:
		var self *handle
		for _, h := range r.handles {
			if h.alias && r.toks[h.tok].start == sym.Def.From {
				self = h
			}
		}
		for _, h := range r.handles {
			if h != self && self != nil && h.block == self.block && strings.EqualFold(h.name, name) {
				return true
			}
		}
	case SymCTE:
		var self *cteDecl
		for _, c := range r.ctes {
			if r.toks[c.tok].start == sym.Def.From {
				self = c
			}
		}
		for _, c := range r.ctes {
			if c != self && self != nil && c.frame == self.frame && strings.EqualFold(c.name, name) {
				return true
			}
		}
	case SymColumn:
		for i, t := range r.toks {
			if c := r.declared[i]; c != nil && t.start == sym.Def.From && c.via == nil {
				return r.columnTaken(c, sym.Uses, name)
			}
		}
	}
	return false
}
