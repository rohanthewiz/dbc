package designer

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Import reads a SELECT back into a design — the generator's other way
// round — so a query written by hand, or by ⇪ Query and then edited, can be
// put on the canvas and judged: the guide's findings and the story are then
// about the query the user already has.
//
// FAITHFUL OR REFUSED. The design imported must write a statement that
// returns what the one read in returns; otherwise the guide would judge a
// different query from the user's. So the reader takes only what maps onto
// a design exactly, says in a note what it reads in another spelling, and
// refuses everything else with the reason (ImportError) — never by
// quietly dropping a part:
//
//	read as written            columns, aggregates, * and t.*, DISTINCT;
//	                           JOIN / LEFT / RIGHT / FULL / CROSS / comma,
//	                           ON col = col, USING; filters of a column
//	                           against values (= <> < <= > >= BETWEEN IN
//	                           LIKE ILIKE IS NULL), OR groups, HAVING,
//	                           ORDER BY, LIMIT / OFFSET / FETCH; a summary,
//	                           LEFT JOIN (SELECT key, aggregates … GROUP BY
//	                           key) s ON s.key = …, as summaryItem says
//	read in the designer's     [NOT] EXISTS (one table)  → has any / has none
//	terms (a note says so)     x IN (SELECT y FROM t …)  → has any
//	                           a.x = b.y in WHERE        → an inner join
//	                           x >= a AND x < b (dates)  → a range, a year, a day
//	                           x >= now() - interval …   → in the last N days
//	                           (x op v OR x IS NULL)     → "or empty"
//	                           lower(x) LIKE lower(v)    → ignore case
//	                           GROUP BY, no aggregate    → DISTINCT
//	refused, with why          CTEs, set operations, other derived tables,
//	                           window functions, CASE, functions of a
//	                           column, a column compared with a column or
//	                           an expression, NOT IN (SELECT …), parameters…
//
// The pipeline:
//
//	SQL ─► sqlsplit.Split (one statement) ─► lexSQL ─► parseStatement (AST)
//	    ─► binder: names against the catalog, the AST onto a Design
//	    ─► planned as the generator plans it: refused if a RIGHT or FULL
//	       JOIN would be moved (the one place its breadth-first FROM order
//	       could change the meaning)
//
// What the generator then writes differs in spelling — aliases filled in,
// GROUP BY completed, values typed — but not in the rows.

// ImportError is a statement the designer cannot open, and why.
type ImportError struct {
	Reason string `json:"reason"`
	At     int    `json:"at"`   // byte offset in the SQL given; -1 for none
	Line   int    `json:"line"` // the 1-based line of At; 0 for none
}

func (e *ImportError) Error() string {
	if e.Line > 1 {
		return "line " + strconv.Itoa(e.Line) + ": " + e.Reason
	}
	return e.Reason
}

// Import reads sql — one statement, a SELECT — as a design on cat's schema.
// The notes say what was read in the designer's own terms. A statement it
// cannot express is an *ImportError.
func Import(sql string, cat *Catalog) (Design, []string, error) {
	if cat == nil || cat.Schema == nil {
		return Design{}, nil, &ImportError{Reason: "the connection's tables are not known yet", At: -1}
	}
	// split as the engine reads strings, as lexSQL then reads them: on
	// Postgres `x = 'a\'` ends at its quote, and what follows a ; after
	// it is a second statement, not more of the string
	stmts := sqlsplit.DialectOf(cat.Driver).Split(sql)
	switch {
	case len(stmts) == 0:
		return Design{}, nil, &ImportError{Reason: "there is no statement here", At: -1}
	case len(stmts) > 1:
		return Design{}, nil, &ImportError{Reason: "more than one statement: put the caret on the one to open, or select it",
			At: stmts[1].Start, Line: 1 + strings.Count(sql[:stmts[1].Start], "\n")}
	}
	st := stmts[0]
	d, notes, err := importStmt(st.Text, cat)
	if ie, ok := err.(*ImportError); ok && ie.At >= 0 {
		ie.At += st.Start
		ie.Line = 1 + strings.Count(sql[:min(ie.At, len(sql))], "\n")
	}
	return d, notes, err
}

func importStmt(text string, cat *Catalog) (Design, []string, error) {
	dia := dialectOf(cat.Driver)
	toks, comment, err := lexSQL(text, dia)
	if err != nil {
		return Design{}, nil, err
	}
	s, err := parseStatement(toks, text)
	if err != nil {
		return Design{}, nil, err
	}
	b := &binder{cat: cat, dia: dia, sql: text, opt: map[string]bool{}, using: map[string]string{},
		ids: map[string]bool{}, jids: map[string]bool{}, sums: map[string]bool{}}
	if comment {
		b.note("comments are not kept: the designer writes its own SQL")
	}
	if err := b.bind(s); err != nil {
		return Design{}, nil, err
	}
	return b.d, b.notes, nil
}

// ── binding ────────────────────────────────────────────────────────────

// bound is a table the statement names, as bound to the design.
type bound struct {
	id     string
	et     *erd.Table
	name   string // what the statement calls it: its alias, else its name
	quoted bool   // that name was quoted: matched exactly
	alias  bool   // it has an alias (so schema.table.column cannot name it)
	pos    int
	// sum is set for a derived table read as a summary (summaryItem): its
	// columns are the derived table's, not et's, and et is nil. id is
	// then the summarized table's, as the canvas has it.
	sum *summed
}

// colName is x's column called name, as the statement may name it: one of
// its table's, or for a summary one of the derived table's. "" when none.
func (x *bound) colName(name string, quoted bool) string {
	if x.sum != nil {
		if c := x.sum.col(name, quoted); c != nil {
			return c.name
		}
		return ""
	}
	return colOf(x.et, name, quoted)
}

// binder maps the AST onto a design.
type binder struct {
	cat   *Catalog
	dia   dialect
	sql   string
	d     Design
	notes []string

	scope []*bound          // the outer query's FROM tables, in order
	opt   map[string]bool   // table IDs an outer join may leave empty (conservatively)
	using map[string]string // a USING column (lower) → the earlier table's ID it is read from
	// rightAt is where the first RIGHT or FULL JOIN is, -1 none: there
	// the generator's FROM order is checked against the statement's
	rightAt int
	comma   bool
	groups  int // OR groups made, for their letters
	ids     map[string]bool
	jids    map[string]bool
	outs    []outRef // the select list as read, for GROUP BY, HAVING and ORDER BY
	// sums are the summarized tables' IDs; sumUses each read of a
	// summary's column, checked once the whole statement is read
	// (summaryForms)
	sums    map[string]bool
	sumUses []sumUse
}

// outRef is one output as read: its ref and the name it goes by.
type outRef struct {
	ref  Ref
	name string // its alias, else its column's name, else ""
}

func (b *binder) fail(at int, reason string) error { return &ImportError{Reason: reason, At: at} }

func (b *binder) note(s string) {
	if !slices.Contains(b.notes, s) {
		b.notes = append(b.notes, s)
	}
}

// text is an expression as written, cut short for a message.
func (b *binder) text(e *ex) string {
	if e == nil || e.end <= e.pos || e.end > len(b.sql) {
		return "…"
	}
	s := strings.Join(strings.Fields(b.sql[e.pos:e.end]), " ")
	if len(s) > 60 {
		s = s[:57] + "…"
	}
	return "`" + s + "`"
}

func (b *binder) bind(s *sel) error {
	b.rightAt = -1
	for _, it := range s.from {
		if err := b.fromItem(it); err != nil {
			return err
		}
	}
	if b.rightAt >= 0 && b.comma {
		return b.fail(b.rightAt, "a comma join beside a RIGHT or FULL JOIN: a comma binds looser than JOIN, which the designer "+
			"does not keep — write CROSS JOIN, or the ON")
	}
	if err := b.selectList(s); err != nil {
		return err
	}
	levels := [][]*bound{b.scope}
	if s.where != nil {
		var fs []Filter
		for _, c := range conjuncts(s.where) {
			got, err := b.whereCond(c, levels)
			if err != nil {
				return err
			}
			fs = append(fs, got...)
		}
		b.d.Filters = append(b.d.Filters, b.mergeRanges(fs)...)
	}
	if err := b.groupBy(s); err != nil {
		return err
	}
	if s.having != nil {
		var fs []Filter
		for _, c := range conjuncts(s.having) {
			got, err := b.cond(c, levels, condCtx{agg: true, where: "HAVING"})
			if err != nil {
				return err
			}
			for _, f := range got {
				// a plain column in HAVING is a grouped one, which the
				// designer filters in WHERE: the same rows, before grouping
				if f.Agg == AggNone && !b.grouped(f.Ref) {
					return b.fail(c.pos, "HAVING on "+b.text(c)+", a column not grouped by")
				}
			}
			fs = append(fs, got...)
		}
		b.d.Filters = append(b.d.Filters, fs...)
	}
	if err := b.orderBy(s); err != nil {
		return err
	}
	if err := b.summaryForms(); err != nil {
		return err
	}
	// LIMIT ALL, -1 and MySQL's 2^64-1 were read as none (-1): the
	// designer writes its own "no limit" where an OFFSET needs one
	if s.limit == 0 {
		return b.fail(s.pos, "LIMIT 0: a query of no rows")
	}
	if s.limit > 0 {
		b.d.Limit = s.limit
	}
	b.d.Offset = s.offset
	b.d.Distinct = b.d.Distinct || s.distinct
	if len(b.d.Tables) > 0 {
		b.d.Main = b.d.Tables[0].ID
	}
	normalize(&b.d)
	b.markFKs()
	return b.place()
}

// ── FROM ───────────────────────────────────────────────────────────────

// resolveTable finds the catalog's table for a FROM item: by exact name
// first, then — the name not quoted — case-blind, as an engine folds an
// unquoted name. A name in two schemas, unqualified, is the one in the
// default schema (its sidebar label has no schema), or refused.
func (b *binder) resolveTable(it fromItem) (*erd.Table, error) {
	var hits []*erd.Table
	for pass := 0; pass < 2 && len(hits) == 0; pass++ {
		exact := pass == 0
		if !exact && it.quoted {
			break
		}
		for _, et := range b.cat.Schema.Tables {
			nameOK := et.Name == it.name || !exact && strings.EqualFold(et.Name, it.name)
			schemaOK := it.schema == "" || et.Schema == it.schema || !exact && strings.EqualFold(et.Schema, it.schema)
			if nameOK && schemaOK {
				hits = append(hits, et)
			}
		}
	}
	shown := it.name
	if it.schema != "" {
		shown = it.schema + "." + it.name
	}
	switch len(hits) {
	case 0:
		return nil, b.fail(it.pos, "no table or view `"+shown+"` on this connection")
	case 1:
		return hits[0], nil
	}
	var bare []*erd.Table
	var schemas []string
	for _, et := range hits {
		if et.Label == et.Name {
			bare = append(bare, et)
		}
		schemas = append(schemas, et.Schema)
	}
	if len(bare) == 1 {
		return bare[0], nil
	}
	return nil, b.fail(it.pos, "`"+shown+"` is a table in "+strings.Join(schemas, ", ")+": name its schema")
}

// lookup finds a table in scope by the name the statement calls it.
func lookup(sc []*bound, name string, quoted bool) *bound {
	for _, x := range sc {
		if x.name == name || !quoted && !x.quoted && strings.EqualFold(x.name, name) {
			return x
		}
	}
	return nil
}

// addTable puts a FROM (or EXISTS) table on the canvas.
func (b *binder) addTable(it fromItem) (*bound, error) {
	et, err := b.resolveTable(it)
	if err != nil {
		return nil, err
	}
	t := Table{ID: freshID("t", b.ids), Schema: et.Schema, Name: et.Name}
	b.ids[t.ID] = true
	x := &bound{id: t.ID, et: et, name: it.name, quoted: it.quoted, pos: it.pos}
	if it.alias != "" {
		x.name, x.alias = it.alias, true
		x.quoted = false
		if validAlias(it.alias) {
			t.Alias = it.alias
		} else {
			b.note("the alias `" + it.alias + "` is not a plain name, so the designer gives " + et.Name + " one of its own")
		}
	}
	b.d.Tables = append(b.d.Tables, t)
	return x, nil
}

// fromItem binds one FROM table and its join.
func (b *binder) fromItem(it fromItem) error {
	if it.sub != nil {
		return b.summaryItem(it)
	}
	x, err := b.addTable(it)
	if err != nil {
		return err
	}
	if lookup(b.scope, x.name, x.quoted) != nil {
		return b.fail(it.pos, "`"+x.name+"` names two tables: give one an alias")
	}
	switch it.join {
	case "comma":
		b.comma = true
		b.note("a comma between tables is read as CROSS JOIN, and a.x = b.y in WHERE as a join")
	case "right", "full":
		if b.rightAt < 0 {
			b.rightAt = it.pos
		}
		for _, o := range b.scope {
			b.opt[o.id] = true
		}
		if it.join == "full" {
			b.opt[x.id] = true
		}
	case "left":
		b.opt[x.id] = true
	}
	earlier := b.scope
	b.scope = append(b.scope, x)
	switch it.join {
	case "", "comma", "cross":
		return nil
	}
	kind := JoinInner
	switch it.join {
	case "left", "right":
		kind = JoinLeft
	case "full":
		kind = JoinFull
	}
	if it.using != nil {
		return b.usingJoin(it, x, earlier, kind)
	}

	// the ON: col = col pairs with earlier tables are the join; the rest
	// are filters on the joined table, kept in the join (InJoin)
	pairs := map[string][]Pair{}
	var order []string
	levels := [][]*bound{b.scope}
	var filters []Filter
	for _, c := range conjuncts(it.on) {
		l, r, isEq, err := b.colEq(c, levels)
		if err != nil {
			return err
		}
		if isEq {
			if l.Table == x.id {
				l, r = r, l
			}
			switch {
			case l.Table == x.id: // both its own
				return b.fail(c.pos, "a join condition comparing two of `"+x.name+"`'s own columns ("+b.text(c)+")")
			case r.Table != x.id:
				return b.fail(c.pos, "a condition in `"+x.name+"`'s ON about other tables ("+b.text(c)+
					"): the designer's join conditions are between the joined table and one before it")
			}
			if !slices.Contains(order, l.Table) {
				order = append(order, l.Table)
			}
			pairs[l.Table] = append(pairs[l.Table], Pair{L: l.Column, R: r.Column})
			continue
		}
		if isConstTrue(c) {
			continue
		}
		fs, err := b.cond(c, levels, condCtx{only: x.id, where: "`" + x.name + "`'s ON"})
		if err != nil {
			return err
		}
		for i := range fs {
			fs[i].InJoin = true
		}
		filters = append(filters, fs...)
	}
	if len(order) == 0 {
		return b.fail(it.pos, "the join to `"+x.name+"` has no column = column condition: the designer joins on columns")
	}
	for _, o := range order {
		j := Join{ID: freshID("j", b.jids), Left: o, Right: x.id, Kind: kind, On: pairs[o]}
		if it.join == "right" {
			// RIGHT JOIN keeps the joined table's rows: in the designer's
			// terms a left join from it, read from the other end (the tree
			// writes it back as RIGHT JOIN)
			j.Left, j.Right = x.id, o
			for k, p := range j.On {
				j.On[k] = Pair{L: p.R, R: p.L}
			}
		}
		b.jids[j.ID] = true
		b.d.Joins = append(b.d.Joins, j)
	}
	b.d.Filters = append(b.d.Filters, b.mergeRanges(filters)...)
	return nil
}

// usingJoin binds JOIN … USING (c, …): each column paired with the one
// earlier table that has it. A bare reference to it later is read from
// that earlier table — for an inner or left join the value USING shows.
func (b *binder) usingJoin(it fromItem, x *bound, earlier []*bound, kind JoinKind) error {
	if kind != JoinInner && kind != JoinLeft || it.join == "right" {
		return b.fail(it.usingPos, "USING on a RIGHT or FULL JOIN: its column is either side's, which the designer cannot "+
			"say — write the ON")
	}
	pairs := map[string][]Pair{}
	var order []string
	for _, c := range it.using {
		xc := colOf(x.et, c, false)
		if xc == "" {
			return b.fail(it.usingPos, "`"+x.name+"` has no column `"+c+"` for USING")
		}
		var hits []*bound
		for _, o := range earlier {
			if o.colName(c, false) != "" {
				if o.sum != nil {
					return b.fail(it.usingPos, "USING ("+c+") with the summary `"+o.name+"`'s column: write the ON")
				}
				hits = append(hits, o)
			}
		}
		if len(hits) != 1 {
			return b.fail(it.usingPos, "USING ("+c+"): "+strconv.Itoa(len(hits))+" of the tables before `"+x.name+
				"` have that column — write the ON")
		}
		o := hits[0]
		if !slices.Contains(order, o.id) {
			order = append(order, o.id)
		}
		pairs[o.id] = append(pairs[o.id], Pair{L: colOf(o.et, c, false), R: xc})
		b.using[strings.ToLower(c)] = o.id
	}
	for _, o := range order {
		j := Join{ID: freshID("j", b.jids), Left: o, Right: x.id, Kind: kind, On: pairs[o]}
		b.jids[j.ID] = true
		b.d.Joins = append(b.d.Joins, j)
	}
	return nil
}

// ── names ──────────────────────────────────────────────────────────────

// colOf is et's column called name: exactly, or case-blind when the name
// was not quoted. "" when it has none.
func colOf(et *erd.Table, name string, quoted bool) string {
	if et == nil {
		return ""
	}
	if c := et.Col(name); c != nil {
		return c.Name
	}
	if quoted {
		return ""
	}
	for _, c := range et.Cols {
		if strings.EqualFold(c.Name, name) {
			return c.Name
		}
	}
	return ""
}

// column binds a column reference to a table's column. A summary's column
// is not one: it is read only where the generator writes one (summaryRef).
func (b *binder) column(e *ex, levels [][]*bound) (Ref, error) {
	x, c, err := b.colRef(e, levels)
	if err != nil {
		return Ref{}, err
	}
	if x.sum != nil {
		return Ref{}, b.fail(e.pos, b.text(e)+" is a column of the summary `"+x.name+"`: a summary is joined to its parent "+
			"alone, and its aggregates are read only in the select list, a filter or a sort")
	}
	return Ref{Table: x.id, Column: c}, nil
}

// colRef binds a column reference, innermost level first: a bare name to
// the one table of the nearest level that has it, a qualified one to the
// table so named. levels[0] is the innermost scope. It returns the table
// and the column's own name there (for a summary, the derived table's).
func (b *binder) colRef(e *ex, levels [][]*bound) (*bound, string, error) {
	n := len(e.parts)
	name, quoted := e.parts[n-1], e.quoted[n-1]
	if n == 1 {
		for li, sc := range levels {
			var hits []*bound
			for _, x := range sc {
				if x.colName(name, quoted) != "" {
					hits = append(hits, x)
				}
			}
			if len(hits) > 1 && li == len(levels)-1 {
				// a USING column is one column: read from its earlier table
				if id, ok := b.using[strings.ToLower(name)]; ok {
					for _, x := range hits {
						if x.id == id {
							hits = []*bound{x}
							break
						}
					}
				}
			}
			switch len(hits) {
			case 1:
				return hits[0], hits[0].colName(name, quoted), nil
			case 0:
				continue
			}
			var names []string
			for _, x := range hits {
				names = append(names, x.name)
			}
			return nil, "", b.fail(e.pos, "`"+name+"` is a column of "+strings.Join(names, " and ")+": say which")
		}
		return nil, "", b.fail(e.pos, "no column `"+name+"` in the query's tables")
	}
	var x *bound
	for _, sc := range levels {
		if n == 2 {
			x = lookup(sc, e.parts[0], e.quoted[0])
		} else {
			// schema.table.column names a table by its own name, never
			// by an alias it was given
			for _, o := range sc {
				if !o.alias && strings.EqualFold(o.et.Name, e.parts[1]) && strings.EqualFold(o.et.Schema, e.parts[0]) {
					x = o
				}
			}
		}
		if x != nil {
			break
		}
	}
	if x == nil {
		return nil, "", b.fail(e.pos, "no table `"+strings.Join(e.parts[:n-1], ".")+"` in the query for "+b.text(e))
	}
	c := x.colName(name, quoted)
	switch {
	case c == "" && x.sum != nil:
		return nil, "", b.fail(e.pos, "the summary `"+x.name+"` has no column `"+name+"`")
	case c == "":
		return nil, "", b.fail(e.pos, "`"+x.et.Name+"` has no column `"+name+"`")
	}
	return x, c, nil
}

// aggOf maps a call to an aggregate.
var aggOf = map[string]Agg{"count": AggCount, "sum": AggSum, "avg": AggAvg, "min": AggMin, "max": AggMax}

// subject binds what a filter, an output or a sort is about: a column, or
// an aggregate of one (agg: where an aggregate may be). An output's name
// (outs) may stand for its expression, as HAVING and ORDER BY allow.
func (b *binder) subject(e *ex, levels [][]*bound, agg, outs bool) (Ref, error) {
	switch e.op {
	case "col":
		// an output's name before a column's, as ORDER BY reads it (and
		// MySQL's HAVING)
		if outs && len(e.parts) == 1 {
			for _, o := range b.outs {
				if o.name != "" && strings.EqualFold(o.name, e.parts[0]) {
					return o.ref, nil
				}
			}
		}
		if r, ok, err := b.summaryRef(e, levels, agg); ok || err != nil {
			return r, err
		}
		return b.column(e, levels)
	case "arith":
		if r, ok, err := b.summaryRef(e, levels, agg); ok || err != nil {
			return r, err
		}
	case "call":
		if r, ok, err := b.summaryRef(e, levels, agg); ok || err != nil {
			return r, err
		}
		a, ok := aggOf[e.val]
		if !ok {
			return Ref{}, b.fail(e.pos, b.text(e)+": the designer shows columns and their aggregates, not other functions of them")
		}
		if !agg {
			return Ref{}, b.fail(e.pos, "an aggregate in WHERE ("+b.text(e)+"): a condition on groups belongs in HAVING")
		}
		if e.star {
			if a != AggCount {
				return Ref{}, b.fail(e.pos, b.text(e)+" of every column")
			}
			return Ref{Agg: AggCount}, nil
		}
		if len(e.kids) != 1 {
			return Ref{}, b.fail(e.pos, b.text(e)+": the designer's aggregates take one column")
		}
		k := e.kids[0]
		if a == AggCount && !e.distinct && (k.op == "num" || k.op == "str") {
			b.note("`count(" + k.val + ")` is read as count(*): it counts the rows either way")
			return Ref{Agg: AggCount}, nil
		}
		if k.op != "col" {
			return Ref{}, b.fail(k.pos, "an aggregate of "+b.text(k)+": the designer aggregates a column")
		}
		r, err := b.column(k, levels)
		if err != nil {
			return Ref{}, err
		}
		r.Agg = a
		if e.distinct {
			if a != AggCount {
				return Ref{}, b.fail(e.pos, b.text(e)+": only count takes DISTINCT in the designer")
			}
			r.Agg = AggCountDistinct
		}
		return r, nil
	}
	return Ref{}, b.fail(e.pos, b.text(e)+": the designer shows columns and their aggregates")
}

// ── the select list ────────────────────────────────────────────────────

func (b *binder) selectList(s *sel) error {
	levels := [][]*bound{b.scope}
	// a summary's * would be the derived table's columns as written, but
	// the generator writes those from the aggregates asked of it
	for _, it := range s.items {
		if it.e.op != "star" {
			continue
		}
		for _, x := range b.scope {
			if x.sum != nil && (len(it.e.parts) == 0 || lookup([]*bound{x}, it.e.parts[len(it.e.parts)-1], it.e.quoted[len(it.e.quoted)-1]) != nil) {
				return b.fail(it.e.pos, b.text(it.e)+" over the summary `"+x.name+"`: list the columns — the designer writes a "+
					"summary's own from the aggregates asked of it")
			}
		}
	}
	// every item a star covering the FROM tables once each, in order: no
	// columns at all, which the generator writes back as * (or a.*, b.*)
	stars := len(s.items) > 0
	var covered []string
	for _, it := range s.items {
		if it.e.op != "star" {
			stars = false
			break
		}
		if len(it.e.parts) == 0 {
			for _, x := range b.scope {
				covered = append(covered, x.id)
			}
			continue
		}
		x := lookup(b.scope, it.e.parts[len(it.e.parts)-1], it.e.quoted[len(it.e.quoted)-1])
		if x == nil {
			return b.fail(it.e.pos, "no table `"+strings.Join(it.e.parts, ".")+"` in the query for "+b.text(it.e))
		}
		covered = append(covered, x.id)
	}
	var all []string
	for _, x := range b.scope {
		all = append(all, x.id)
	}
	if stars && slices.Equal(covered, all) {
		if len(b.using) > 0 {
			return b.fail(s.pos, "* with JOIN … USING: USING shows its column once, the designer's ON shows each table's — list the columns")
		}
		// no outputs, but ORDER BY 1 and the like still name the columns
		// the * shows
		for _, x := range b.scope {
			for _, c := range x.et.Cols {
				b.outs = append(b.outs, outRef{ref: Ref{Table: x.id, Column: c.Name}, name: c.Name})
			}
		}
		return nil
	}
	for _, it := range s.items {
		if it.e.op == "star" {
			if it.alias != "" {
				return b.fail(it.e.pos, "an alias on "+b.text(it.e))
			}
			tbls := b.scope
			if len(it.e.parts) > 0 {
				tbls = []*bound{lookup(b.scope, it.e.parts[len(it.e.parts)-1], it.e.quoted[len(it.e.quoted)-1])}
				if tbls[0] == nil {
					return b.fail(it.e.pos, "no table `"+strings.Join(it.e.parts, ".")+"` in the query")
				}
			} else if len(b.using) > 0 {
				return b.fail(it.e.pos, "* with JOIN … USING: USING shows its column once, the designer's ON shows each table's — list the columns")
			}
			for _, x := range tbls {
				for _, c := range x.et.Cols {
					r := Ref{Table: x.id, Column: c.Name}
					b.d.Columns = append(b.d.Columns, Output{Ref: r})
					b.outs = append(b.outs, outRef{ref: r, name: c.Name})
				}
			}
			b.note("* beside other columns is read as each of its columns")
			continue
		}
		r, err := b.subject(it.e, levels, true, false)
		if err != nil {
			return err
		}
		o := Output{Ref: r}
		name := it.alias
		if name == "" && r.Agg == AggNone {
			name = r.Column
		}
		if name == "" && it.e.op == "col" && b.sums[r.Table] {
			// p.paid shows as paid; the generator would name it sum_amount
			if _, c, err := b.colRef(it.e, levels); err == nil {
				name, o.Alias = c, c
			}
		}
		if it.alias != "" && !(r.Agg == AggNone && strings.EqualFold(it.alias, r.Column)) {
			o.Alias = it.alias
		}
		b.d.Columns = append(b.d.Columns, o)
		b.outs = append(b.outs, outRef{ref: r, name: name})
	}
	return nil
}

// ── conditions ─────────────────────────────────────────────────────────

// conjuncts are an expression's top-level ANDs.
func conjuncts(e *ex) []*ex {
	if e == nil {
		return nil
	}
	if e.op == "and" {
		var out []*ex
		for _, k := range e.kids {
			out = append(out, conjuncts(k)...)
		}
		return out
	}
	return []*ex{e}
}

// disjuncts are an expression's top-level ORs.
func disjuncts(e *ex) []*ex {
	if e.op == "or" {
		var out []*ex
		for _, k := range e.kids {
			out = append(out, disjuncts(k)...)
		}
		return out
	}
	return []*ex{e}
}

// isConstTrue: WHERE 1 = 1, WHERE true — a template's placeholder.
func isConstTrue(e *ex) bool {
	if e.op == "bool" && e.val == "true" {
		return true
	}
	return e.op == "cmp" && e.val == "=" && e.kids[0].op == "num" && e.kids[1].op == "num" && e.kids[0].val == e.kids[1].val
}

// colEq reports whether e is column = column between two tables of the
// levels, and binds both.
func (b *binder) colEq(e *ex, levels [][]*bound) (l, r Ref, ok bool, err error) {
	if e.op != "cmp" || e.kids[0].op != "col" || e.kids[1].op != "col" {
		return Ref{}, Ref{}, false, nil
	}
	if l, err = b.column(e.kids[0], levels); err != nil {
		return
	}
	if r, err = b.column(e.kids[1], levels); err != nil {
		return
	}
	if e.val != "=" {
		return Ref{}, Ref{}, false, b.fail(e.pos, b.text(e)+": the designer joins on equal columns, and compares a column with values")
	}
	if l.Table == r.Table {
		return Ref{}, Ref{}, false, b.fail(e.pos, b.text(e)+" compares two columns of one table: the designer compares a column with values")
	}
	return l, r, true, nil
}

// condCtx says where a condition is and what it may name.
type condCtx struct {
	agg   bool   // aggregates allowed (HAVING)
	only  string // "" any table; else the one table filters must be on
	where string // where it is, for a message ("WHERE", "o's ON", "EXISTS")
}

// whereCond binds one WHERE conjunct: a filter or an OR group, a join
// (a.x = b.y), or a subquery test.
func (b *binder) whereCond(c *ex, levels [][]*bound) ([]Filter, error) {
	switch {
	case isConstTrue(c):
		return nil, nil
	case c.op == "exists":
		return b.exists(c, c.sub, nil, c.not, levels)
	case c.op == "in" && c.sub != nil:
		if c.not {
			return nil, b.fail(c.pos, "NOT IN (SELECT …): a single NULL from the subquery makes it match nothing. The designer "+
				"writes has none (NOT EXISTS), which keeps the rows regardless — rewrite it as NOT EXISTS to open it, if that is meant")
		}
		if c.kids[0].op != "col" {
			return nil, b.fail(c.pos, b.text(c.kids[0])+" IN (SELECT …): the designer tests a column")
		}
		b.note("x IN (SELECT …) is read as has any (EXISTS): the same rows, each once")
		return b.exists(c, c.sub, c.kids[0], false, levels)
	}
	l, r, isEq, err := b.colEq(c, levels)
	if err != nil {
		return nil, err
	}
	if isEq {
		return nil, b.whereJoin(c, l, r)
	}
	return b.cond(c, levels, condCtx{where: "WHERE"})
}

// whereJoin reads a.x = b.y in WHERE as a pair of an inner join between
// them. On a table an outer join may leave empty that would be a different
// query (WHERE drops the empty rows, ON keeps them), so it is refused.
func (b *binder) whereJoin(c *ex, l, r Ref) error {
	if b.opt[l.Table] || b.opt[r.Table] {
		return b.fail(c.pos, b.text(c)+" in WHERE, on an outer-joined table: it drops the rows the outer join keeps — "+
			"the designer cannot keep both. Move it into the join's ON, or make the join inner")
	}
	idx := func(id string) int { return slices.IndexFunc(b.scope, func(x *bound) bool { return x.id == id }) }
	if idx(l.Table) > idx(r.Table) {
		l, r = r, l
	}
	for i := range b.d.Joins {
		j := &b.d.Joins[i]
		if j.Kind != JoinInner {
			continue
		}
		switch {
		case j.Left == l.Table && j.Right == r.Table:
			j.On = append(j.On, Pair{L: l.Column, R: r.Column})
			return nil
		case j.Left == r.Table && j.Right == l.Table:
			j.On = append(j.On, Pair{L: r.Column, R: l.Column})
			return nil
		}
	}
	j := Join{ID: freshID("j", b.jids), Left: l.Table, Right: r.Table, Kind: JoinInner, On: []Pair{{L: l.Column, R: r.Column}}}
	b.jids[j.ID] = true
	b.d.Joins = append(b.d.Joins, j)
	return nil
}

// cond binds one conjunct as filters: one, or an OR group (several sharing
// a new group letter).
func (b *binder) cond(c *ex, levels [][]*bound, cx condCtx) ([]Filter, error) {
	if c.op == "exists" || c.op == "in" && c.sub != nil {
		return nil, b.fail(c.pos, "a subquery test in "+cx.where+": the designer reads EXISTS as a condition of the whole WHERE")
	}
	if c.op != "or" {
		f, err := b.filter(c, levels, cx)
		if err != nil {
			return nil, err
		}
		return []Filter{f}, nil
	}
	ms := disjuncts(c)
	// (x op v OR x IS NULL): one filter that keeps the empty ones too
	if len(ms) == 2 {
		for k, m := range ms {
			o := ms[1-k]
			if m.op != "isnull" || m.not || o.op == "isnull" || m.kids[0].op != "col" {
				continue
			}
			// subject, not column: a summary's column (p.sum_amount) is one too
			r, err := b.subject(m.kids[0], levels, cx.agg, false)
			if err != nil {
				return nil, err
			}
			f, err := b.filter(o, levels, cx)
			if err == nil && f.Ref == r {
				f.OrNull = true
				return []Filter{f}, nil
			}
		}
	}
	b.groups++
	group := groupName(b.groups)
	var out []Filter
	for _, m := range ms {
		if m.op == "and" {
			f, ok, err := b.rangeOf(m, levels, cx)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, b.fail(m.pos, "an AND inside an OR ("+b.text(m)+"): the designer's any-of groups hold single comparisons")
			}
			f.Group = group
			out = append(out, f)
			continue
		}
		f, err := b.filter(m, levels, cx)
		if err != nil {
			return nil, err
		}
		f.Group = group
		out = append(out, f)
	}
	return out, nil
}

// groupName is the n-th OR group's letter: A, B, … Z, then AA, AB, …
func groupName(n int) string {
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+n%26)) + s
		n /= 26
	}
	return s
}

// rangeOf reads (x >= a AND x < b) inside an OR group as one range filter.
func (b *binder) rangeOf(m *ex, levels [][]*bound, cx condCtx) (Filter, bool, error) {
	ks := conjuncts(m)
	if len(ks) != 2 {
		return Filter{}, false, nil
	}
	var fs []Filter
	for _, k := range ks {
		f, err := b.filter(k, levels, cx)
		if err != nil {
			return Filter{}, false, err
		}
		fs = append(fs, f)
	}
	got := b.mergeRanges(fs)
	return got[0], len(got) == 1, nil
}

// cmpOps maps a comparison to the designer's operator; flipped is the
// comparison with its sides swapped (5 < x is x > 5).
var (
	cmpOps  = map[string]string{"=": OpEq, "<>": OpNe, "<": OpLt, "<=": OpLe, ">": OpGt, ">=": OpGe}
	flipped = map[string]string{"=": "=", "<>": "<>", "<": ">", "<=": ">=", ">": "<", ">=": "<="}
)

// valueish reports whether an expression is a value rather than what a
// filter is about.
func valueish(e *ex) bool {
	switch e.op {
	case "str", "num", "bool", "null", "param", "typed", "interval":
		return true
	case "neg", "cast":
		return valueish(e.kids[0])
	case "arith":
		return true
	case "call":
		return len(e.kids) == 0 || e.val == "datetime" || e.val == "date"
	}
	return false
}

// filter binds one simple condition.
func (b *binder) filter(c *ex, levels [][]*bound, cx condCtx) (Filter, error) {
	subj := func(e *ex) (Ref, error) {
		r, err := b.subject(e, levels, cx.agg, cx.agg)
		if err != nil {
			return Ref{}, err
		}
		if cx.only != "" && r.Table != cx.only {
			name := "another table"
			for _, sc := range levels {
				for _, x := range sc {
					if x.id == cx.only {
						name = "`" + x.name + "`"
					}
				}
			}
			return Ref{}, b.fail(e.pos, b.text(e)+" in "+cx.where+": the designer's filters there are on "+name+" alone")
		}
		return r, nil
	}
	switch c.op {
	case "cmp":
		x, y, op := c.kids[0], c.kids[1], c.val
		if valueish(x) && !valueish(y) {
			x, y, op = y, x, flipped[op]
		}
		r, err := subj(x)
		if err != nil {
			return Filter{}, err
		}
		if y.op == "null" {
			return Filter{}, b.fail(c.pos, b.text(c)+" is never true: IS NULL is meant")
		}
		if op == ">=" {
			if n, ok := b.lastDays(y, r); ok {
				return Filter{Ref: r, Op: OpLastDays, Values: []string{strconv.Itoa(n)}}, nil
			}
		}
		v, err := b.value(y)
		if err != nil {
			return Filter{}, err
		}
		return Filter{Ref: r, Op: cmpOps[op], Values: []string{v}}, nil
	case "isnull":
		r, err := subj(c.kids[0])
		if err != nil {
			return Filter{}, err
		}
		op := OpNull
		if c.not {
			op = OpNotNull
		}
		return Filter{Ref: r, Op: op}, nil
	case "between":
		if c.not {
			return Filter{}, b.fail(c.pos, "NOT BETWEEN: write it as < a or > b, in an any-of group")
		}
		r, err := subj(c.kids[0])
		if err != nil {
			return Filter{}, err
		}
		lo, err := b.value(c.kids[1])
		if err != nil {
			return Filter{}, err
		}
		hi, err := b.value(c.kids[2])
		if err != nil {
			return Filter{}, err
		}
		return Filter{Ref: r, Op: OpBetween, Values: []string{lo, hi}}, nil
	case "in":
		r, err := subj(c.kids[0])
		if err != nil {
			return Filter{}, err
		}
		var vals []string
		for _, k := range c.kids[1:] {
			if k.op == "null" {
				if c.not {
					return Filter{}, b.fail(k.pos, "NOT IN with a NULL in the list matches nothing")
				}
				continue // x IN (…, NULL) is x IN (…) wherever WHERE keeps only the true
			}
			v, err := b.value(k)
			if err != nil {
				return Filter{}, err
			}
			vals = append(vals, v)
		}
		if len(vals) == 0 {
			return Filter{}, b.fail(c.pos, b.text(c)+" has no value to match")
		}
		op := OpIn
		if c.not {
			op = OpNotIn
		}
		return Filter{Ref: r, Op: op, Values: vals}, nil
	case "like":
		return b.like(c, subj)
	case "col":
		// a boolean column as the condition: it is true
		r, err := subj(c)
		if err != nil {
			return Filter{}, err
		}
		if fam := b.famOf(r); fam != famBool && fam != famUnknown {
			return Filter{}, b.fail(c.pos, b.text(c)+" alone as a condition: compare it with a value")
		}
		return Filter{Ref: r, Op: OpEq, Values: []string{"true"}}, nil
	case "not":
		if k := c.kids[0]; k.op == "col" {
			r, err := subj(k)
			if err != nil {
				return Filter{}, err
			}
			if fam := b.famOf(r); fam == famBool || fam == famUnknown {
				return Filter{Ref: r, Op: OpEq, Values: []string{"false"}}, nil
			}
		}
		return Filter{}, b.fail(c.pos, "NOT ("+strings.Trim(b.text(c.kids[0]), "`")+"): write the opposite comparison")
	case "exists":
		return Filter{}, b.fail(c.pos, "EXISTS inside "+cx.where+": the designer reads EXISTS as a condition of the whole WHERE")
	}
	return Filter{}, b.fail(c.pos, b.text(c)+" in "+cx.where+": the designer's filters compare a column with values")
}

// famOf is a ref's column family.
func (b *binder) famOf(r Ref) family {
	t := b.d.table(r.Table)
	if t == nil {
		return famUnknown
	}
	if et := b.cat.erdTable(t); et != nil {
		if c := et.Col(r.Column); c != nil {
			return familyOf(c.Type)
		}
	}
	return famUnknown
}

// value is a literal as a filter's value.
func (b *binder) value(e *ex) (string, error) {
	switch e.op {
	case "str", "num", "bool":
		return e.val, nil
	case "neg":
		if e.kids[0].op == "num" {
			return "-" + e.kids[0].val, nil
		}
	case "typed":
		return e.kids[0].val, nil
	case "cast":
		if k := e.kids[0]; k.op == "str" || k.op == "num" {
			return k.val, nil
		}
	case "param":
		return "", b.fail(e.pos, "a parameter ("+e.val+"): the designer needs the value — put it in, then open the query")
	case "null":
		return "", b.fail(e.pos, "NULL as a value: IS NULL is meant")
	}
	return "", b.fail(e.pos, b.text(e)+": the designer compares a column with values, not with an expression")
}

// like binds [I]LIKE as contains, starts with or ends with — the only
// patterns the designer writes: fixed text with % at one end or both. The
// escape is ESCAPE's, else the engine's default (\ on Postgres, MySQL and
// bytdb; SQLite has none). lower(x) LIKE lower(v) and ILIKE ignore case.
func (b *binder) like(c *ex, subj func(*ex) (Ref, error)) (Filter, error) {
	if c.not {
		return Filter{}, b.fail(c.pos, "NOT LIKE: the designer's patterns keep what matches — write it as an any-of of the other values, or open it in a query tab")
	}
	x, pat := c.kids[0], c.kids[1]
	icase := c.val == "ilike"
	lowX := x.op == "call" && x.val == "lower" && len(x.kids) == 1
	if lowX {
		x = x.kids[0]
	}
	lowP := pat.op == "call" && pat.val == "lower" && len(pat.kids) == 1
	if lowP {
		pat = pat.kids[0]
	}
	r, err := subj(x)
	if err != nil {
		return Filter{}, err
	}
	if r.Agg != AggNone {
		return Filter{}, b.fail(c.pos, "LIKE on an aggregate")
	}
	if pat.op != "str" {
		return Filter{}, b.fail(pat.pos, "a LIKE pattern that is not a string ("+b.text(pat)+")")
	}
	esc := byte(0)
	if b.dia.name != "sqlite" {
		esc = '\\'
	}
	if len(c.kids) == 3 {
		e := c.kids[2]
		if e.op != "str" || len(e.val) != 1 {
			return Filter{}, b.fail(e.pos, "an ESCAPE of other than one character")
		}
		esc = e.val[0]
	}
	// the pattern's own text, its escapes undone, and where its % are
	var body strings.Builder
	lead, trail := false, false
	p := pat.val
	for i := 0; i < len(p); i++ {
		ch := p[i]
		switch {
		case esc != 0 && ch == esc && i+1 < len(p):
			i++
			body.WriteByte(p[i])
		case ch == '%' && i == 0:
			lead = true
		case ch == '%' && i == len(p)-1:
			trail = true
		case ch == '%' || ch == '_':
			return Filter{}, b.fail(pat.pos, "the pattern "+b.text(pat)+" has a "+string(ch)+" inside it: the designer's patterns are "+
				"fixed text, with % at the start, the end, or both")
		default:
			body.WriteByte(ch)
		}
	}
	v := body.String()
	var op string
	switch {
	case v == "":
		return Filter{}, b.fail(pat.pos, "the pattern "+b.text(pat)+" matches every value that is not empty")
	case lead && trail:
		op = OpContains
	case trail:
		op = OpStarts
	case lead:
		op = OpEnds
	default:
		return Filter{}, b.fail(pat.pos, "a LIKE with no %: it is =, except in how case and trailing spaces compare — write =")
	}
	switch {
	case lowX && lowP:
		icase = true
	case lowX && strings.ToLower(v) == v:
		icase = true
	case lowX:
		return Filter{}, b.fail(c.pos, "lower(x) LIKE a pattern with capitals: it never matches")
	case lowP:
		return Filter{}, b.fail(c.pos, "x LIKE lower(…): only one side lowered — write lower(x) LIKE lower(…), or ILIKE")
	}
	return Filter{Ref: r, Op: op, Values: []string{v}, IgnoreCase: icase}, nil
}

// lastDays reads the right side of x >= … as "the last N days", in each
// engine's spelling — the ones the generator writes (sql.go daysAgo):
//
//	timestamp  now() - interval 'N days'   NOW() - INTERVAL N DAY   datetime('now', '-N days')
//	date       current_date - N            CURDATE() - INTERVAL N DAY   date('now', '-N days')
//
// The generator writes the date spelling for a date column and the
// timestamp one otherwise. Read the other way round they differ by part of
// the first day, which a note says.
func (b *binder) lastDays(e *ex, r Ref) (int, bool) {
	days := func(s string) (int, bool) {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(s), "-"))
		if len(f) != 2 || (f[1] != "day" && f[1] != "days") {
			return 0, false
		}
		n, err := strconv.Atoi(f[0])
		return n, err == nil && n > 0
	}
	n, date, ok := 0, false, false
	switch {
	case e.op == "arith" && e.val == "-" && e.kids[0].op == "call" && len(e.kids[0].kids) == 0:
		fn, rhs := e.kids[0].val, e.kids[1]
		date = fn == "current_date" || fn == "curdate"
		if !date && fn != "now" && fn != "current_timestamp" && fn != "localtimestamp" && fn != "sysdate" {
			return 0, false
		}
		switch {
		case rhs.op == "interval":
			n, ok = days(rhs.val)
		case rhs.op == "num" && date:
			v, err := strconv.Atoi(rhs.val)
			n, ok = v, err == nil && v > 0
		}
	case e.op == "call" && (e.val == "datetime" || e.val == "date") && len(e.kids) == 2 &&
		e.kids[0].op == "str" && strings.EqualFold(e.kids[0].val, "now") && e.kids[1].op == "str" &&
		strings.HasPrefix(strings.TrimSpace(e.kids[1].val), "-"):
		date = e.val == "date"
		n, ok = days(e.kids[1].val)
	}
	if !ok {
		return 0, false
	}
	if isDate := b.famOf(r) == famDate; isDate != date {
		b.note("`>= " + strings.Trim(b.text(e), "`") + "` is read as in the last " + strconv.Itoa(n) +
			" days, which the designer writes for the column's type: it differs in how much of the first day it counts")
	}
	return n, true
}

// mergeRanges reads a >= and a < on the same date column, neither in a
// group, as one range — and a range that is a whole calendar year or day
// as "in year" or "on day", which is how the generator writes those. Only
// a pair written side by side is one: that is how the generator writes a
// range, and how a person writes one; two bounds far apart may be two
// thoughts (the last 7 days, and before March), kept as they are.
func (b *binder) mergeRanges(fs []Filter) []Filter {
	plain := func(f Filter) bool {
		return f.Group == "" && !f.OrNull && f.Agg == AggNone && len(f.Values) == 1 && dateRE.MatchString(strings.TrimSpace(f.Values[0]))
	}
	used := map[int]bool{}
	var out []Filter
	for i, f := range fs {
		if used[i] {
			continue
		}
		if k := i + 1; k < len(fs) && plain(f) && plain(fs[k]) && fs[k].Ref == f.Ref && fs[k].InJoin == f.InJoin {
			lo, hi := f, fs[k]
			if f.Op == OpLt {
				lo, hi = hi, lo
			}
			if lo.Op == OpGe && hi.Op == OpLt {
				used[k] = true
				f = b.dateRange(lo, hi)
			}
		}
		out = append(out, f)
	}
	return out
}

// dateRange is [lo, hi) as a range, a year or a day.
func (b *binder) dateRange(lo, hi Filter) Filter {
	a, z := strings.TrimSpace(lo.Values[0]), strings.TrimSpace(hi.Values[0])
	f := Filter{Ref: lo.Ref, InJoin: lo.InJoin, Op: OpRange, Values: []string{a, z}}
	da, ea := time.Parse("2006-01-02", a)
	dz, ez := time.Parse("2006-01-02", z)
	if ea != nil || ez != nil {
		return f
	}
	switch {
	case da.Month() == 1 && da.Day() == 1 && dz.Equal(da.AddDate(1, 0, 0)):
		f.Op, f.Values = OpYear, []string{strconv.Itoa(da.Year())}
	case dz.Equal(da.AddDate(0, 0, 1)):
		f.Op, f.Values = OpOn, []string{a}
	}
	return f
}

// ── subquery tests ─────────────────────────────────────────────────────

// exists binds [NOT] EXISTS (SELECT … FROM t WHERE …) — or, with in set,
// in IN (SELECT y FROM t …) — as a join of kind has any / has none from
// the outer table it is correlated with to t, its filters on t.
func (b *binder) exists(c *ex, s *sel, in *ex, not bool, levels [][]*bound) ([]Filter, error) {
	what := "EXISTS"
	if in != nil {
		what = "IN (SELECT …)"
	}
	switch {
	case len(s.from) != 1:
		return nil, b.fail(s.pos, what+" over more than one table: the designer tests one table")
	case len(s.groupBy) > 0 || s.having != nil:
		return nil, b.fail(s.pos, what+" over groups: the designer tests rows")
	case s.limit == 0:
		return nil, b.fail(s.pos, what+" with LIMIT 0: never true")
	case s.offset > 0:
		return nil, b.fail(s.pos, what+" with an OFFSET: the designer tests for any row")
	}
	if in == nil {
		// EXISTS (SELECT count(*) …) is always true: an aggregate with no
		// GROUP BY returns its one row whether rows match or not
		for _, it := range s.items {
			if it.e.op == "call" && aggOf[it.e.val] != "" {
				return nil, b.fail(it.e.pos, "EXISTS (SELECT "+it.e.val+"(…) …) is always true: an aggregate returns a row even of no rows")
			}
		}
	}
	t, err := b.addTable(s.from[0])
	if err != nil {
		return nil, err
	}
	inner := append([][]*bound{{t}}, levels...)
	var outer string
	var pairs []Pair
	pair := func(c *ex, l, r Ref) error {
		if l.Table == t.id {
			l, r = r, l
		}
		if r.Table != t.id || l.Table == t.id {
			return b.fail(c.pos, b.text(c)+" inside "+what+" is not about the tested table: move it out")
		}
		if outer != "" && l.Table != outer {
			return b.fail(c.pos, what+" tied to two of the outer tables: the designer tests a table against one")
		}
		outer = l.Table
		pairs = append(pairs, Pair{L: l.Column, R: r.Column})
		return nil
	}
	if in != nil {
		if len(s.items) != 1 || s.items[0].e.op != "col" {
			return nil, b.fail(s.pos, "IN (SELECT …) of other than one column")
		}
		l, err := b.column(in, levels)
		if err != nil {
			return nil, err
		}
		r, err := b.column(s.items[0].e, [][]*bound{{t}})
		if err != nil {
			return nil, err
		}
		if err := pair(c, l, r); err != nil {
			return nil, err
		}
	}
	var fs []Filter
	for _, k := range conjuncts(s.where) {
		if isConstTrue(k) {
			continue
		}
		l, r, isEq, err := b.colEq(k, inner)
		if err != nil {
			return nil, err
		}
		if isEq {
			if err := pair(k, l, r); err != nil {
				return nil, err
			}
			continue
		}
		got, err := b.cond(k, inner, condCtx{only: t.id, where: what})
		if err != nil {
			return nil, err
		}
		fs = append(fs, got...)
	}
	if outer == "" {
		return nil, b.fail(s.pos, what+" not tied to the outer query's tables: it is true or false for every row alike")
	}
	kind := JoinExists
	if not {
		kind = JoinNotExists
	}
	j := Join{ID: freshID("j", b.jids), Left: outer, Right: t.id, Kind: kind, On: pairs}
	b.jids[j.ID] = true
	b.d.Joins = append(b.d.Joins, j)
	return b.mergeRanges(fs), nil
}

// ── summaries ──────────────────────────────────────────────────────────

// summed is a derived table read as a summarized join: its columns, each a
// key it is grouped (and joined) by, or an aggregate of its one table.
type summed struct {
	cols []sumCol
}

// sumCol is one column of a summary, by the name the derived table gives
// it: key, a column of the table it is grouped by; else agg of col ("" for
// count(*)). An aggregate given no name is kept with name "", which
// nothing outside can name.
type sumCol struct {
	name string
	key  string
	agg  Agg
	col  string
}

// col is the summary's column called name, or nil. A derived table's
// column is named by an alias, which the engines fold as they fold any
// unquoted name, so a name not quoted matches case-blind.
func (s *summed) col(name string, quoted bool) *sumCol {
	for i := range s.cols {
		c := &s.cols[i]
		if c.name != "" && (c.name == name || !quoted && strings.EqualFold(c.name, name)) {
			return c
		}
	}
	return nil
}

// sumUse is one read of a summary's column: where, and whether in its
// grouped (re-aggregated) form.
type sumUse struct {
	e       *ex
	grouped bool
	parent  string // the summary's parent, as the statement calls it
}

// summaryItem binds a derived table in FROM as the designer's summarized
// join (sql.go summarySelect), the one derived table the generator writes:
//
//	LEFT JOIN (
//	    SELECT p.order_id, sum(p.amount) AS sum_amount, count(*) AS count
//	    FROM payments p                    one table
//	    WHERE p.amount > 0                 its filters, kept on it
//	    GROUP BY p.order_id                its key, exactly
//	) p ON p.order_id = o.id               the key, with one earlier table
//
// Anything else a derived table can compute is refused. Two parts carry
// the meaning, so both are held to the generator's shape:
//
//   - LEFT JOIN: a summary keeps the rows of the parent that have none
//     (their sum NULL, their count 0). An inner join drops them.
//   - Grouped by exactly the columns it is joined on: then each parent
//     row meets at most one summary row, so attaching it repeats nothing,
//     which is the point of a summary. Grouped by more, a parent would
//     meet one row per extra value. Joined on fewer, likewise.
//
// The derived table's alias becomes the canvas table's, since it is what
// the outer statement calls it; the generator gives both the same.
func (b *binder) summaryItem(it fromItem) error {
	s := it.sub
	switch {
	case it.join == "":
		return b.fail(it.pos, "a derived table first in FROM: the designer reads one as a summary of a table per row of "+
			"another — start FROM that table, and LEFT JOIN the summary to it")
	case it.join != "left":
		return b.fail(it.pos, "a derived table joined other than by LEFT JOIN: the designer reads one as a summary, which "+
			"keeps the rows that have none — write LEFT JOIN")
	case it.using != nil:
		return b.fail(it.usingPos, "USING on a summary: write the ON")
	case len(s.from) != 1 || s.from[0].sub != nil:
		return b.fail(s.pos, "a derived table over more than one table: the designer summarizes one table — join "+
			"the others outside it")
	case len(s.groupBy) == 0:
		return b.fail(s.pos, "a derived table with no GROUP BY: the designer reads one as a summary per key — group it "+
			"by the columns it is joined on")
	case s.having != nil:
		return b.fail(s.having.pos, "HAVING inside a summary: it drops whole groups, which then show as having none — "+
			"filter the summarized rows inside (WHERE), or the totals outside")
	case s.limit >= 0 || s.offset > 0:
		return b.fail(s.pos, "LIMIT or OFFSET inside a summary: it keeps some groups and drops the others")
	}
	if len(s.orderBy) > 0 {
		b.note("ORDER BY inside a summary orders nothing outside it: it is not kept")
	}
	// DISTINCT is kept by no note: a row per group is distinct already

	t, err := b.addTable(s.from[0])
	if err != nil {
		return err
	}
	inner := [][]*bound{{t}}
	tb := b.d.table(t.id)
	tb.Alias = ""
	if validAlias(it.alias) {
		tb.Alias = it.alias
	} else {
		b.note("the alias `" + it.alias + "` is not a plain name, so the designer gives the summary of " + t.et.Name + " one of its own")
	}
	x := &bound{id: t.id, name: it.alias, alias: true, pos: it.pos, sum: &summed{}}
	if lookup(b.scope, x.name, false) != nil {
		return b.fail(it.pos, "`"+x.name+"` names two tables: give one an alias")
	}

	// its columns: keys and aggregates of its table
	for _, si := range s.items {
		e := si.e
		var c sumCol
		switch e.op {
		case "col":
			r, err := b.column(e, inner)
			if err != nil {
				return err
			}
			c = sumCol{name: r.Column, key: r.Column}
		case "call":
			r, err := b.subject(e, inner, true, false)
			if err != nil {
				return err
			}
			c = sumCol{agg: r.Agg, col: r.Column}
		default:
			return b.fail(e.pos, b.text(e)+" in a summary: the designer's summaries show their key and aggregates of the table")
		}
		if si.alias != "" {
			c.name = si.alias
		}
		if c.name != "" && x.sum.col(c.name, false) != nil {
			return b.fail(e.pos, "the summary `"+x.name+"` has two columns called `"+c.name+"`")
		}
		x.sum.cols = append(x.sum.cols, c)
	}

	// its key: exactly what it is grouped by, and every key shown grouped
	var keys []string
	for _, g := range s.groupBy {
		var key string
		switch g.op {
		case "col":
			r, err := b.column(g, inner)
			if err != nil {
				return err
			}
			key = r.Column
		case "num":
			k, err := strconv.Atoi(g.val)
			if err != nil || k < 1 || k > len(x.sum.cols) || x.sum.cols[k-1].key == "" {
				return b.fail(g.pos, "GROUP BY "+g.val+" in a summary: not one of its key columns")
			}
			key = x.sum.cols[k-1].key
		default:
			return b.fail(g.pos, "a summary grouped by "+b.text(g)+": the designer groups one by the columns it joins on")
		}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	for _, c := range x.sum.cols {
		if c.key != "" && !slices.Contains(keys, c.key) {
			return b.fail(s.pos, "the summary shows `"+c.key+"` but is not grouped by it")
		}
	}

	// the ON: each key paired with a column of one earlier table, nothing else
	b.opt[x.id] = true
	b.scope = append(b.scope, x)
	levels := [][]*bound{b.scope}
	var parent *bound
	var pairs []Pair
	joined := map[string]bool{}
	for _, c := range conjuncts(it.on) {
		if isConstTrue(c) {
			continue
		}
		if c.op != "cmp" || c.val != "=" || c.kids[0].op != "col" || c.kids[1].op != "col" {
			return b.fail(c.pos, b.text(c)+" in the summary's ON: the designer joins a summary on its key alone — filter the "+
				"summarized rows inside it, or its totals in WHERE")
		}
		l, lc, err := b.colRef(c.kids[0], levels)
		if err != nil {
			return err
		}
		r, rc, err := b.colRef(c.kids[1], levels)
		if err != nil {
			return err
		}
		if l == x {
			l, lc, r, rc = r, rc, l, lc
		}
		switch {
		case r != x || l == x:
			return b.fail(c.pos, b.text(c)+" in the summary's ON: the designer joins a summary on its key, with a table before it")
		case l.sum != nil:
			return b.fail(c.pos, b.text(c)+": a summary joined to another summary — the designer summarizes a table per row of a table")
		case parent != nil && l != parent:
			return b.fail(c.pos, "the summary `"+x.name+"` joined to two tables: the designer summarizes per row of one")
		}
		k := x.sum.col(rc, true)
		if k.key == "" {
			return b.fail(c.pos, b.text(c)+" joins on the summary's aggregate: the designer joins a summary on its key")
		}
		parent = l
		if !joined[k.key] {
			joined[k.key] = true
			pairs = append(pairs, Pair{L: lc, R: k.key})
		}
	}
	if parent == nil {
		return b.fail(it.pos, "the summary `"+x.name+"` is joined on no key = column: the designer joins a summary on its key")
	}
	for _, k := range keys {
		if !joined[k] {
			return b.fail(it.pos, "the summary `"+x.name+"` is grouped by `"+k+"` but not joined on it: each `"+parent.name+
				"` would meet one of its rows per "+k+" — join on every column it is grouped by, or group by fewer")
		}
	}
	j := Join{ID: freshID("j", b.jids), Left: parent.id, Right: x.id, Kind: JoinSummary, On: pairs}
	b.jids[j.ID] = true
	b.d.Joins = append(b.d.Joins, j)
	b.sums[x.id] = true

	// its filters, on its table, in its own WHERE
	var fs []Filter
	for _, k := range conjuncts(s.where) {
		if isConstTrue(k) {
			continue
		}
		got, err := b.cond(k, inner, condCtx{only: t.id, where: "the summary"})
		if err != nil {
			return err
		}
		fs = append(fs, got...)
	}
	b.d.Filters = append(b.d.Filters, b.mergeRanges(fs)...)
	return nil
}

// summaryRef reads e as a summary's aggregate, in the forms the generator
// writes one (sql.go summaryExpr) — ok false when e is not about a
// summary's column at all, so the caller reads it as anything else:
//
//	                 not grouped              grouped
//	sum(x)           s.sum_x                  sum(s.sum_x)
//	count(x)/(*)     COALESCE(s.count_x, 0)   COALESCE(sum(s.count_x), 0)
//	min/max(x)       s.min_x                  min(s.min_x)
//	avg(x)           s.avg_x                  sum(s.sum_x) * 1.0 / NULLIF(sum(s.count_x), 0)
//	count(DISTINCT)  s.count_distinct_x       — (no form)
//
// Which form fits depends on the whole statement, not known yet: each read
// is kept (sumUses) for summaryForms. A summary's column written another
// way is refused, with the form that is meant: those are the spellings
// that give a parent with no rows (no summary row) its right value, and
// that re-aggregate per group without weighing parents wrongly.
func (b *binder) summaryRef(e *ex, levels [][]*bound, agg bool) (Ref, bool, error) {
	// asCol: e as a summary's column, or nil
	asCol := func(e *ex) (*bound, *sumCol) {
		if e == nil || e.op != "col" {
			return nil, nil
		}
		x, c, err := b.colRef(e, levels)
		if err != nil || x.sum == nil {
			return nil, nil
		}
		return x, x.sum.col(c, true)
	}
	// sumOf: e as sum(<a summary's column>)
	sumOf := func(e *ex) (*bound, *sumCol) {
		if e == nil || e.op != "call" || e.val != "sum" || e.distinct || e.star || len(e.kids) != 1 {
			return nil, nil
		}
		return asCol(e.kids[0])
	}
	num := func(e *ex, v float64) bool {
		f, err := strconv.ParseFloat(e.val, 64)
		return e.op == "num" && err == nil && f == v
	}
	// mentions: e names a summary's column anywhere
	var mentions func(e *ex) bool
	mentions = func(e *ex) bool {
		if x, _ := asCol(e); x != nil {
			return true
		}
		return e != nil && slices.ContainsFunc(e.kids, mentions)
	}
	use := func(x *bound, c *sumCol, a Agg, grouped bool) (Ref, bool, error) {
		if grouped && !agg {
			return Ref{}, false, b.fail(e.pos, "an aggregate in WHERE ("+b.text(e)+"): a condition on groups belongs in HAVING")
		}
		parent := ""
		for _, j := range b.d.Joins {
			if j.Right == x.id {
				for _, p := range b.scope {
					if p.id == j.Left {
						parent = p.name
					}
				}
			}
		}
		b.sumUses = append(b.sumUses, sumUse{e: e, grouped: grouped, parent: parent})
		return Ref{Table: x.id, Column: c.col, Agg: a}, true, nil
	}
	refuse := func(why string) (Ref, bool, error) { return Ref{}, false, b.fail(e.pos, b.text(e)+": "+why) }

	switch e.op {
	case "col":
		x, c := asCol(e)
		switch {
		case x == nil:
			return Ref{}, false, nil
		case c.key != "":
			return refuse("the summary's key, which is its parent's column where it matches and NULL where it does not — show the parent's own")
		case c.agg == AggCount:
			return refuse("a count with no COALESCE: where there are none there is no summary row, so it is NULL — the designer " +
				"writes COALESCE(" + strings.Trim(b.text(e), "`") + ", 0), which is 0")
		}
		return use(x, c, c.agg, false)
	case "call":
		if e.val == "coalesce" && len(e.kids) == 2 && num(e.kids[1], 0) {
			if x, c := asCol(e.kids[0]); x != nil {
				if c.agg != AggCount {
					return refuse("COALESCE of a summary's " + string(c.agg) + ": the designer's is NULL where there are none, as SQL's " +
						string(c.agg) + " of no rows is")
				}
				return use(x, c, AggCount, false)
			}
			if x, c := sumOf(e.kids[0]); x != nil {
				if c.agg != AggCount {
					return refuse("COALESCE of a summary's re-aggregated " + string(c.agg) + ": the designer's is NULL where there are none")
				}
				return use(x, c, AggCount, true)
			}
		}
		if outer, ok := aggOf[e.val]; ok && len(e.kids) == 1 && !e.star {
			x, c := asCol(e.kids[0])
			if x == nil {
				break
			}
			switch {
			case e.distinct || c.key != "":
			case outer == AggSum && c.agg == AggSum, outer == AggMin && c.agg == AggMin, outer == AggMax && c.agg == AggMax:
				return use(x, c, c.agg, true)
			case outer == AggSum && c.agg == AggCount:
				return refuse("a sum of counts with no COALESCE: it is NULL for a group with none — the designer writes COALESCE(" +
					strings.Trim(b.text(e), "`") + ", 0)")
			case c.agg == AggAvg:
				return refuse("an average re-aggregated: a parent with one row would weigh as much as one with a thousand — the " +
					"designer writes sum(s.sum_x) * 1.0 / NULLIF(sum(s.count_x), 0), from a sum and a count of x in the summary")
			case c.agg == AggCountDistinct:
				return refuse("distinct counts per row cannot be added up across a group: one value may be counted under two of them")
			}
			return refuse("the designer re-aggregates a summary's sum and count with sum, its min with min and its max with max")
		}
	case "arith":
		// sum(s.sum_x) * 1.0 / NULLIF(sum(s.count_x), 0): the average of x per group
		if e.val == "/" && e.kids[0].op == "arith" && e.kids[0].val == "*" {
			m, z := e.kids[0], e.kids[1]
			sx, sc := sumOf(m.kids[0])
			one := m.kids[1]
			if sx == nil {
				sx, sc = sumOf(m.kids[1])
				one = m.kids[0]
			}
			if sx != nil && num(one, 1) && strings.ContainsAny(one.val, ".eE") &&
				z.op == "call" && z.val == "nullif" && len(z.kids) == 2 && num(z.kids[1], 0) {
				if cx, cc := sumOf(z.kids[0]); cx == sx && sc.agg == AggSum && cc.agg == AggCount && cc.col == sc.col && sc.col != "" {
					return use(sx, sc, AggAvg, true)
				}
			}
		}
	}
	if mentions(e) {
		return refuse("the designer reads a summary's columns as it writes them: s.sum_x, s.min_x, s.max_x, s.avg_x or " +
			"COALESCE(s.count, 0) per row; sum(s.sum_x), min(s.min_x), max(s.max_x), COALESCE(sum(s.count), 0) or " +
			"sum(s.sum_x) * 1.0 / NULLIF(sum(s.count_x), 0) per group")
	}
	return Ref{}, false, nil
}

// summaryForms checks each summary column read against the statement as
// the generator will write it (sql.go isGrouped): grouped when it shows,
// filters or sorts by an aggregate of a table not summarized, or count(*);
// then every summary column is re-aggregated, else each is read per row.
// The other mix is a statement the designer cannot write: a summary's
// value per row inside groups, or groups of nothing but re-aggregated
// summaries (the generator would group by nothing, and so not at all).
func (b *binder) summaryForms() error {
	if len(b.sumUses) == 0 {
		return nil
	}
	other := func(r Ref) bool { return r.Agg != AggNone && !b.sums[r.Table] }
	grouped := slices.ContainsFunc(b.d.Columns, func(o Output) bool { return other(o.Ref) }) ||
		slices.ContainsFunc(b.d.Filters, func(f Filter) bool { return other(f.Ref) }) ||
		slices.ContainsFunc(b.d.Sorts, func(s Sort) bool { return other(s.Ref) })
	for _, u := range b.sumUses {
		switch {
		case u.grouped && !grouped:
			return b.fail(u.e.pos, b.text(u.e)+" re-aggregates a summary, but nothing else is aggregated: the designer then "+
				"shows a summary's value per `"+u.parent+"`, and groups only for another table's aggregate or count(*)")
		case !u.grouped && grouped:
			return b.fail(u.e.pos, b.text(u.e)+" is a summary's value per `"+u.parent+"`, but the query is grouped: the "+
				"designer re-aggregates it per group (sum of a sum, min of a min, COALESCE(sum(…), 0) of a count)")
		}
	}
	return nil
}

// ── GROUP BY, ORDER BY ─────────────────────────────────────────────────

// aggregated reports whether the statement aggregates anywhere.
func (b *binder) aggregated(s *sel) bool {
	for _, o := range b.d.Columns {
		if o.Agg != AggNone {
			return true
		}
	}
	var has func(e *ex) bool
	has = func(e *ex) bool {
		if e == nil {
			return false
		}
		if e.op == "call" && aggOf[e.val] != "" {
			return true
		}
		return slices.ContainsFunc(e.kids, has)
	}
	if has(s.having) {
		return true
	}
	return slices.ContainsFunc(s.orderBy, func(o ordItem) bool { return has(o.e) })
}

// grouped reports whether r is a plain output column: what the designer
// groups by.
func (b *binder) grouped(r Ref) bool {
	return slices.ContainsFunc(b.d.Columns, func(o Output) bool { return o.Agg == AggNone && o.Ref == r })
}

// groupBy checks GROUP BY against what the designer writes: exactly the
// plain output columns. One it does not show cannot be kept; with no
// aggregate at all the grouping is DISTINCT.
func (b *binder) groupBy(s *sel) error {
	if len(s.groupBy) == 0 {
		return nil
	}
	levels := [][]*bound{b.scope}
	var refs []Ref
	for _, e := range s.groupBy {
		var r Ref
		switch {
		case e.op == "num":
			k, err := strconv.Atoi(e.val)
			if err != nil || k < 1 || k > len(b.outs) {
				return b.fail(e.pos, "GROUP BY "+e.val+": there is no output column "+e.val)
			}
			r = b.outs[k-1].ref
		case e.op == "col":
			var err error
			if r, err = b.column(e, levels); err != nil {
				// a name that is no column may be an output's alias
				found := false
				for _, o := range b.outs {
					if len(e.parts) == 1 && o.name != "" && strings.EqualFold(o.name, e.parts[0]) {
						r, found = o.ref, true
						break
					}
				}
				if !found {
					return err
				}
			}
		default:
			return b.fail(e.pos, "grouping by "+b.text(e)+": the designer groups by columns")
		}
		if r.Agg != AggNone {
			return b.fail(e.pos, "grouping by an aggregate ("+b.text(e)+")")
		}
		if !b.grouped(r) {
			return b.fail(e.pos, "grouping by "+b.text(e)+", which is not shown: the designer groups by exactly the shown "+
				"columns — show it, or drop it from GROUP BY")
		}
		refs = append(refs, r)
	}
	if !b.aggregated(s) {
		for _, o := range b.d.Columns {
			if !slices.Contains(refs, o.Ref) {
				return b.fail(s.groupBy[0].pos, "GROUP BY without an aggregate, and without "+o.Column+": the designer reads it "+
					"as DISTINCT only when it groups by every shown column")
			}
		}
		b.d.Distinct = true
		b.note("GROUP BY with no aggregate is read as DISTINCT: the same rows")
		return nil
	}
	for _, o := range b.d.Columns {
		if o.Agg == AggNone && !slices.Contains(refs, o.Ref) {
			b.note("`" + o.Column + "` is shown but not in GROUP BY: the designer groups by every shown column")
		}
	}
	return nil
}

func (b *binder) orderBy(s *sel) error {
	levels := [][]*bound{b.scope}
	for _, it := range s.orderBy {
		e := it.e
		var r Ref
		switch e.op {
		case "num":
			k, err := strconv.Atoi(e.val)
			if err != nil || k < 1 || k > len(b.outs) {
				return b.fail(e.pos, "ORDER BY "+e.val+": there is no output column "+e.val)
			}
			r = b.outs[k-1].ref
		case "col", "call", "arith":
			var err error
			if r, err = b.subject(e, levels, true, true); err != nil {
				return err
			}
		default:
			return b.fail(e.pos, "sorting by "+b.text(e)+": the designer sorts by columns and their aggregates")
		}
		b.d.Sorts = append(b.d.Sorts, Sort{Ref: r, Desc: it.desc})
	}
	return nil
}

// ── finishing ──────────────────────────────────────────────────────────

// markFKs names the foreign key each join follows, when its pairs are
// exactly one's columns — what AddTable records, and what the rules read
// ("Postgres does not index a foreign key's columns").
func (b *binder) markFKs() {
	key := func(ps []Pair, swap bool) string {
		var parts []string
		for _, p := range ps {
			if swap {
				p.L, p.R = p.R, p.L
			}
			parts = append(parts, p.L+"="+p.R)
		}
		slices.Sort(parts)
		return strings.Join(parts, ",")
	}
	for i := range b.d.Joins {
		j := &b.d.Joins[i]
		l, r := b.d.table(j.Left), b.d.table(j.Right)
		if l == nil || r == nil {
			continue
		}
		lt, rt := b.cat.erdTable(l), b.cat.erdTable(r)
		for _, rel := range b.cat.Schema.Rels {
			var ps []Pair
			for k := range rel.ChildCols {
				ps = append(ps, Pair{L: rel.ChildCols[k], R: rel.ParentCols[k]})
			}
			if rel.Child == lt && rel.Parent == rt && key(ps, false) == key(j.On, false) ||
				rel.Child == rt && rel.Parent == lt && key(ps, true) == key(j.On, false) {
				j.FK = rel.Name
				break
			}
		}
	}
}

// place lays the tables out as the generator will read them — a column per
// step from the main table, cards stacked by their height — and refuses a
// statement whose RIGHT or FULL JOIN the generator would move: it writes
// the FROM tree breadth first, which keeps the rows of inner and left
// joins whatever the order, but not of a RIGHT or FULL JOIN met earlier or
// later than written.
func (b *binder) place() error {
	tr := plan(&b.d, b.cat)
	if b.rightAt >= 0 {
		var got, want []string
		for _, n := range tr.order {
			if r := n.role(); r == "main" || r == "joined" || r == "cross" {
				got = append(got, n.t.ID)
			}
		}
		for _, x := range b.scope {
			if x.sum == nil { // a summary is a leaf the tree puts back where it was
				want = append(want, x.id)
			}
		}
		if !slices.Equal(got, want) {
			return b.fail(b.rightAt, "the designer writes the joins out from the first table, step by step, which would move "+
				"this RIGHT or FULL JOIN and change the rows it keeps — write it as a LEFT JOIN from the table whose rows are kept, FROM that table")
		}
	}
	// a card is its header and a row per column (designer.js); the
	// columns are 300 apart, which clears a card's 232 and its edge labels
	y := map[int]float64{}
	for _, n := range tr.order {
		depth := len(n.path()) - 1
		n.t.X = 40 + float64(depth)*300
		n.t.Y = 40 + y[depth]
		cols := 6
		if n.et != nil {
			cols = len(n.et.Cols)
		}
		y[depth] += 70 + 22*float64(cols) + 40
	}
	return nil
}
