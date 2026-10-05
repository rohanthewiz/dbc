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
//
// Columns are not resolved: a bare column could belong to any table in
// scope, and telling which needs the catalog and a real grammar.
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

// SymbolKind is what a resolved name is.
type SymbolKind string

const (
	SymAlias SymbolKind = "alias" // a table reference's alias
	SymCTE   SymbolKind = "cte"   // a WITH query's name
	SymTable SymbolKind = "table" // a table referenced without an alias
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
// is a table, or the name is taken in the same query.
func Rename(buf string, caret int, name, driver string) ([]Edit, error) {
	r, k := newResolver(buf, caret)
	var sym Symbol
	if r != nil {
		sym = r.symbolAt(k)
	}
	switch {
	case sym.Kind == "":
		return nil, fmt.Errorf("nothing to rename here: rename works on a table alias or a CTE name")
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
}

// cteDecl is a WITH query's name and the frame it is visible in.
type cteDecl struct {
	name  string
	tok   int
	frame int
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
	r := &resolver{toks: toks, refPart: map[int]bool{}, nameOf: map[int]*handle{}}
	r.blocks()
	r.declarations()
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
					r.ctes = append(r.ctes, &cteDecl{name: ct.name, tok: ct.tok, frame: r.frame[r.block[ct.tok]]})
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
		if len(rf.parts) > 0 {
			for j := range rf.parts {
				r.refPart[rf.nameTok-2*j] = true
			}
			h = &handle{name: rf.parts[len(rf.parts)-1], tok: rf.nameTok, block: r.block[rf.nameTok]}
			if len(rf.parts) == 1 {
				h.cte = r.findCTE(rf.nameTok, h.name)
			}
			r.nameOf[rf.nameTok] = h
		}
		if rf.alias != "" {
			a := &handle{name: rf.alias, tok: rf.aliasTok, block: r.block[rf.aliasTok], alias: true}
			if h != nil {
				a.cte = h.cte
			}
			r.handles = append(r.handles, a)
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
// block, or another CTE of the same WITH. A name in an outer block is
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
	}
	return false
}
