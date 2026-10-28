package designer

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The rules: what the designer tells the user about the query they are
// building. Each finding says WHAT (Title), WHY in plain words — what the
// database will do, and what that means for their numbers or their wait —
// and, where it can, offers FIXES. A fix is a whole new design (the page
// swaps it in, and Ctrl+Z takes it back), or a statement to copy (a CREATE
// INDEX — never run for the user).
//
// Two areas:
//
//	correctness  will the result be what the user means? Errors here stop
//	             the SQL from being run; warnings are queries that run and
//	             return a confident, wrong answer — fan-out above all.
//	performance  will the engine read far more than it keeps? These speak
//	             only on evidence (an index list, a row estimate), and a
//	             live plan (plan.go) adds the engine's own view.
//
// Severity: error (the statement is wrong or will not run), warn (it runs,
// but likely not as meant, or slowly on a big table), info (worth knowing).

// Severity of a finding.
type Severity string

const (
	SevError Severity = "error"
	SevWarn  Severity = "warn"
	SevInfo  Severity = "info"
)

// Lint is one finding. Tables, Join, Filter, Column and Sort point at what
// it is about, for the page to highlight; -1 is none.
type Lint struct {
	Rule   string   `json:"rule"`
	Sev    Severity `json:"sev"`
	Area   string   `json:"area"`
	Title  string   `json:"title"`
	Why    string   `json:"why"`
	Tables []string `json:"tables,omitempty"`
	Join   string   `json:"join,omitempty"`
	Filter int      `json:"filter"`
	Column int      `json:"column"`
	Sort   int      `json:"sort"`
	Fixes  []Fix    `json:"fixes,omitempty"`
}

// Fix is one way out of a finding: a design to swap in, or SQL to copy.
type Fix struct {
	Label  string  `json:"label"`
	Design *Design `json:"design,omitempty"`
	SQL    string  `json:"sql,omitempty"`
}

const (
	areaCorrect = "correctness"
	areaPerf    = "performance"
)

func newLint(rule string, sev Severity, area, title, why string) Lint {
	return Lint{Rule: rule, Sev: sev, Area: area, Title: title, Why: why, Filter: -1, Column: -1, Sort: -1}
}

// linter runs the rules over one analysis.
type linter struct {
	d   *Design // normalized
	tr  *tree
	g   *gen
	cat *Catalog
	out []Lint
}

func (l *linter) add(x Lint) { l.out = append(l.out, x) }

// edit returns a copy of the design changed by f, as a fix's design.
func (l *linter) edit(f func(d *Design)) *Design {
	c := l.d.clone()
	f(&c)
	return &c
}

// lint runs every rule, the correctness ones first, and orders the findings
// by severity, keeping each rule's own order within one.
func (l *linter) lint() []Lint {
	l.ruleMissing()
	l.ruleBroken()
	l.ruleNoCondition()
	l.ruleJoinColumns()
	l.ruleIssues()
	l.ruleSubqueryLeaf()
	l.ruleFullJoin()
	l.ruleCartesian()
	l.ruleTypeMismatch()
	l.ruleManyToMany()
	l.ruleFanoutAgg()
	l.ruleChasm()
	l.ruleFanoutUnused()
	l.ruleLeftFilter()
	l.ruleAntiJoin()
	l.ruleLimitNoOrder()
	l.ruleOrderNotUnique()
	l.ruleNullDrop()
	l.ruleBetweenTime()
	l.ruleCountNullable()
	l.ruleRightJoin()
	l.ruleGuessed()
	l.ruleFilterIndex()
	l.ruleJoinIndex()
	l.ruleContains()
	l.ruleSortIndex()
	l.ruleDeepOffset()
	l.ruleUnbounded()
	l.ruleOrColumns()
	l.ruleSelectAll()

	rank := map[Severity]int{SevError: 0, SevWarn: 1, SevInfo: 2}
	slices.SortStableFunc(l.out, func(a, b Lint) int { return rank[a.Sev] - rank[b.Sev] })
	return l.out
}

// ── edits the fixes are made of ────────────────────────────────────────

// dropTable removes a table and everything that refers to it.
func dropTable(d *Design, id string) {
	d.Tables = slices.DeleteFunc(d.Tables, func(t Table) bool { return t.ID == id })
	d.Joins = slices.DeleteFunc(d.Joins, func(j Join) bool { return j.Left == id || j.Right == id })
	d.Columns = slices.DeleteFunc(d.Columns, func(o Output) bool { return o.Table == id })
	d.Filters = slices.DeleteFunc(d.Filters, func(f Filter) bool { return f.Table == id })
	d.Sorts = slices.DeleteFunc(d.Sorts, func(s Sort) bool { return s.Table == id })
	if d.Main == id {
		d.Main = ""
	}
}

// pointAt makes the join's Right the table id, swapping its sides (and
// each pair) when it was drawn the other way: the subquery kinds read
// Right from Left.
func pointAt(d *Design, joinID, id string) {
	j := d.join(joinID)
	if j == nil || j.Right == id {
		return
	}
	j.Left, j.Right = j.Right, j.Left
	for k := range j.On {
		j.On[k].L, j.On[k].R = j.On[k].R, j.On[k].L
	}
}

func setKind(d *Design, joinID string, k JoinKind) {
	if j := d.join(joinID); j != nil {
		j.Kind = k
	}
}

// refsTo reports whether any output, sort or aggregate filter refers to the
// table: whether the design USES its columns, rather than only filtering on
// them.
func refsTo(d *Design, id string) bool {
	for _, o := range d.Columns {
		if o.Table == id {
			return true
		}
	}
	for _, s := range d.Sorts {
		if s.Table == id {
			return true
		}
	}
	for _, f := range d.Filters {
		if f.Table == id && f.Agg != AggNone {
			return true
		}
	}
	return false
}

// onlyAggRefs reports whether every output, sort and filter on the table is
// an aggregate (the condition for summarizing it), and there is one.
func onlyAggRefs(d *Design, id string) bool {
	some := false
	for _, r := range allRefs(d) {
		if r.Table != id {
			continue
		}
		if r.Agg == AggNone {
			return false
		}
		some = true
	}
	return some
}

// allRefs is every ref of the outputs and sorts, and the aggregate filters.
func allRefs(d *Design) []Ref {
	var out []Ref
	for _, o := range d.Columns {
		out = append(out, o.Ref)
	}
	for _, s := range d.Sorts {
		out = append(out, s.Ref)
	}
	for _, f := range d.Filters {
		if f.Agg != AggNone {
			out = append(out, f.Ref)
		}
	}
	return out
}

// leaf reports whether a node is joined to nothing but its parent.
func (l *linter) leaf(n *node) bool {
	return len(n.kids) == 0 && len(n.extra) == 0 && l.tr.degree[n.t.ID] <= 1
}

// name is how a finding names a node: `orders o`, or `o` when they match.
func name(n *node) string {
	if n.alias == n.t.Name {
		return "`" + n.alias + "`"
	}
	return "`" + n.t.Name + "` (" + n.alias + ")"
}

// ── correctness ────────────────────────────────────────────────────────

func (l *linter) ruleMissing() {
	for _, n := range l.tr.order {
		if n.et != nil || l.cat == nil || l.cat.Schema == nil {
			continue
		}
		x := newLint("missing-table", SevError, areaCorrect, "no table "+n.t.Name+" on this connection",
			"the design names `"+qualified(n.t)+"`, which this connection's catalog does not have — "+
				"dropped or renamed since, or a design made on another database.")
		x.Tables = []string{n.t.ID}
		id := n.t.ID
		x.Fixes = []Fix{{Label: "Remove " + n.alias, Design: l.edit(func(d *Design) { dropTable(d, id) })}}
		l.add(x)
	}
}

func qualified(t *Table) string {
	if t.Schema != "" {
		return t.Schema + "." + t.Name
	}
	return t.Name
}

func (l *linter) ruleBroken() {
	for _, j := range l.tr.dangling {
		x := newLint("broken-join", SevError, areaCorrect, "a join to a table not on the canvas",
			"one end of this join is missing, so it was left out of the statement.")
		x.Join = j.ID
		id := j.ID
		x.Fixes = []Fix{{Label: "Remove the join", Design: l.edit(func(d *Design) {
			d.Joins = slices.DeleteFunc(d.Joins, func(j Join) bool { return j.ID == id })
		})}}
		l.add(x)
	}
}

func (l *linter) ruleNoCondition() {
	for i := range l.d.Joins {
		j := &l.d.Joins[i]
		if len(j.On) > 0 || slices.Contains(l.tr.dangling, j) {
			continue
		}
		a, b := l.tr.nodes[j.Left], l.tr.nodes[j.Right]
		x := newLint("join-no-condition", SevError, areaCorrect, "the join "+a.alias+" — "+b.alias+" matches on nothing",
			"with no columns to match, every row of "+name(a)+" would pair with every row of "+name(b)+". "+
				"Drag a column of one onto the other's, or pick a foreign key.")
		x.Join = j.ID
		for _, rel := range relsBetween(l.cat, a, b) {
			on := rel.pairs
			id := j.ID
			x.Fixes = append(x.Fixes, Fix{Label: "Match on " + rel.label, Design: l.edit(func(d *Design) {
				if jj := d.join(id); jj != nil {
					jj.On, jj.FK = on, rel.name
				}
			})})
		}
		l.add(x)
	}
}

func (l *linter) ruleJoinColumns() {
	for i := range l.d.Joins {
		j := &l.d.Joins[i]
		a, b := l.tr.nodes[j.Left], l.tr.nodes[j.Right]
		if a == nil || b == nil {
			continue
		}
		for _, p := range j.On {
			for _, side := range []struct {
				n *node
				c string
			}{{a, p.L}, {b, p.R}} {
				if side.n.et != nil && side.n.et.Col(side.c) == nil {
					x := newLint("join-column", SevError, areaCorrect, "no column "+side.n.alias+"."+side.c,
						"the join matches on `"+side.c+"`, which "+name(side.n)+" does not have (renamed or dropped?).")
					x.Join = j.ID
					l.add(x)
				}
			}
		}
	}
}

// ruleIssues reports what the generator could not write.
func (l *linter) ruleIssues() {
	for _, is := range l.g.issues {
		x := newLint("invalid", SevError, areaCorrect, is.title, is.why)
		i := is.index
		switch is.what {
		case "filter":
			x.Filter = i
			x.Fixes = []Fix{{Label: "Remove the filter", Design: l.edit(func(d *Design) {
				if i < len(d.Filters) {
					d.Filters = slices.Delete(d.Filters, i, i+1)
				}
			})}}
		case "column":
			x.Column = i
			x.Fixes = []Fix{{Label: "Remove the column", Design: l.edit(func(d *Design) {
				if i < len(d.Columns) {
					d.Columns = slices.Delete(d.Columns, i, i+1)
				}
			})}}
		case "sort":
			x.Sort = i
			if i < len(l.d.Sorts) && !slices.ContainsFunc(l.d.Columns, func(o Output) bool { return o.Ref == l.d.Sorts[i].Ref }) {
				r := l.d.Sorts[i].Ref
				x.Fixes = append(x.Fixes, Fix{Label: "Show it as a column", Design: l.edit(func(d *Design) {
					d.Columns = append(d.Columns, Output{Ref: r})
				})})
			}
			x.Fixes = append(x.Fixes, Fix{Label: "Remove the sort", Design: l.edit(func(d *Design) {
				if i < len(d.Sorts) {
					d.Sorts = slices.Delete(d.Sorts, i, i+1)
				}
			})})
		}
		l.add(x)
	}
}

func (l *linter) ruleSubqueryLeaf() {
	for _, n := range l.tr.order {
		if !n.inSubquery() || l.leaf(n) {
			continue
		}
		x := newLint("subquery-leaf", SevError, areaCorrect, name(n)+" is joined further",
			name(n)+" is joined as \""+kindWords[n.via.Kind]+"\", so it is read in a subquery of its own, "+
				"which can see only it and "+n.parent.alias+". It cannot also be joined to other tables. "+
				"Make it an inner join, or remove its other joins.")
		x.Tables, x.Join = []string{n.t.ID}, n.via.ID
		id := n.via.ID
		x.Fixes = []Fix{{Label: "Make it an inner join", Design: l.edit(func(d *Design) { setKind(d, id, JoinInner) })}}
		l.add(x)
	}
	for _, j := range l.tr.misplaced {
		r := l.tr.nodes[j.Right]
		x := newLint("subquery-side", SevError, areaCorrect, r.alias+" is tested, but the query starts from it",
			"\""+kindWords[j.Kind]+"\" reads "+name(r)+" in a subquery, testing it for each row of "+
				l.tr.nodes[j.Left].alias+" — so the query must reach "+r.alias+" from "+l.tr.nodes[j.Left].alias+", "+
				"not start from it.")
		x.Join = j.ID
		left := j.Left
		x.Fixes = []Fix{{Label: "Start from " + l.tr.nodes[left].alias, Design: l.edit(func(d *Design) { d.Main = left })}}
		l.add(x)
	}
}

func (l *linter) ruleFullJoin() {
	if l.g.dia.fullJoin {
		return
	}
	for i := range l.d.Joins {
		j := &l.d.Joins[i]
		if j.Kind != JoinFull {
			continue
		}
		x := newLint("full-join", SevError, areaCorrect, "MySQL has no full join",
			"MySQL cannot keep the unmatched rows of both sides in one join. A left join keeps one side's; "+
				"for both, UNION a left join with a right join by hand.")
		x.Join = j.ID
		id := j.ID
		x.Fixes = []Fix{{Label: "Make it a left join", Design: l.edit(func(d *Design) { setKind(d, id, JoinLeft) })}}
		l.add(x)
	}
}

// ruleCartesian: tables no join connects. Every row of one pairs with every
// row of the other — a few hundred rows each is tens of thousands. The fix
// is the shortest path of foreign keys between the groups (through bridge
// tables if need be), found in the whole schema.
func (l *linter) ruleCartesian() {
	if l.tr.components() < 2 {
		return
	}
	for _, n := range l.tr.order {
		if n.role() != "cross" {
			continue
		}
		x := newLint("cartesian", SevWarn, areaCorrect, name(n)+" is not joined to the rest",
			"nothing connects "+name(n)+" to "+name(l.tr.order[0])+"'s tables, so every row of one is paired "+
				"with every row of the other (a cross join): their row counts multiply. Join it on a column.")
		x.Tables = []string{n.t.ID}
		if fix, label := connectFix(l.d, l.cat, l.tr, n); fix != nil {
			x.Fixes = append(x.Fixes, Fix{Label: label, Design: fix})
		}
		id := n.t.ID
		x.Fixes = append(x.Fixes, Fix{Label: "Remove " + n.alias, Design: l.edit(func(d *Design) { dropTable(d, id) })})
		l.add(x)
	}
}

func (l *linter) ruleTypeMismatch() {
	for i := range l.d.Joins {
		j := &l.d.Joins[i]
		a, b := l.tr.nodes[j.Left], l.tr.nodes[j.Right]
		if a == nil || b == nil || a.et == nil || b.et == nil {
			continue
		}
		for _, p := range j.On {
			ac, bc := a.et.Col(p.L), b.et.Col(p.R)
			if ac == nil || bc == nil || comparable(familyOf(ac.Type), familyOf(bc.Type)) {
				continue
			}
			x := newLint("type-mismatch", SevWarn, areaCorrect,
				fmt.Sprintf("%s.%s (%s) is matched with %s.%s (%s)", a.alias, p.L, ac.Type, b.alias, p.R, bc.Type),
				"the two columns hold different kinds of value, so the engine converts one side on every row. "+
					"That can match unexpectedly ('01' and 1) or fail mid-query, and an index on the converted "+
					"column cannot be used. Are these really the columns that correspond?")
			x.Join = j.ID
			l.add(x)
		}
	}
}

func (l *linter) ruleManyToMany() {
	for i := range l.d.Joins {
		j := &l.d.Joins[i]
		if j.Kind.subquery() || slices.Contains(l.tr.dangling, j) || l.tr.cardinality(j) != "N:N" {
			continue
		}
		a, b := l.tr.nodes[j.Left], l.tr.nodes[j.Right]
		x := newLint("many-to-many", SevWarn, areaCorrect, "the join "+a.alias+" — "+b.alias+" matches on no key",
			"neither side's columns identify a row, so each row of "+name(a)+" may match many of "+name(b)+
				" AND the other way round: rows multiply in both directions. Joins usually follow a foreign key "+
				"to a primary key; check these are the right columns.")
		x.Join = j.ID
		l.add(x)
	}
}

// ruleFanoutAgg: an aggregate that counts duplicates (sum, avg, count) over a
// table whose rows a join repeats. The classic inflated total.
func (l *linter) ruleFanoutAgg() {
	for i, o := range l.d.Columns {
		n := l.tr.nodes[o.Table]
		if n == nil || !o.Agg.duplicates() || o.Column == "" || n.role() == "summary" {
			continue
		}
		ms := l.tr.multipliers(n)
		if len(ms) == 0 {
			continue
		}
		m := ms[0]
		far := m.edge
		if n.under(m.edge) {
			far = m.edge.parent
		}
		verb := "matches"
		if !m.known {
			verb = "may match"
		}
		x := newLint("fanout-aggregate", SevWarn, areaCorrect,
			fmt.Sprintf("%s(%s.%s) counts each %s row more than once", o.Agg, n.alias, o.Column, n.alias),
			fmt.Sprintf("each row of %s %s several rows of %s, and the join repeats it once per match — "+
				"so %s takes in each %s once per %s row, and the result is inflated. "+
				"Aggregate %s before the join (summarize), or test it instead of joining it.",
				name(n), verb, name(far), o.Agg, o.Column, far.alias, far.alias))
		x.Column, x.Tables = i, []string{n.t.ID, far.t.ID}
		x.Fixes = l.fanoutFixes(far)
		if o.Agg == AggCount && n.et != nil && len(n.et.PK) == 1 && n.et.PK[0] == o.Column {
			x.Fixes = append(x.Fixes, Fix{Label: "Count distinct " + n.alias + "." + o.Column, Design: l.edit(func(d *Design) {
				d.Columns[i].Agg = AggCountDistinct
			})})
		}
		l.add(x)
	}
}

// fanoutFixes are the ways to stop the multiplying table m from multiplying:
// summarize it (when only its aggregates are used), test it (when none of
// its columns are). Only for a leaf, since both read it in a subquery.
func (l *linter) fanoutFixes(m *node) []Fix {
	if m.via == nil || !l.leaf(m) || m.inSubquery() || !m.fromLeft && m.via.Kind != JoinInner {
		return nil
	}
	var out []Fix
	id, mid := m.via.ID, m.t.ID
	// subquery kinds read Right from Left: flip the join when it was drawn
	// the other way
	orient := func(d *Design) { pointAt(d, id, mid) }
	if onlyAggRefs(l.d, mid) {
		out = append(out, Fix{Label: "Summarize " + m.alias + " per " + m.parent.alias, Design: l.edit(func(d *Design) {
			orient(d)
			setKind(d, id, JoinSummary)
		})})
	}
	if !refsTo(l.d, mid) {
		out = append(out, Fix{Label: "Only test " + m.alias + " (has any)", Design: l.edit(func(d *Design) {
			orient(d)
			setKind(d, id, JoinExists)
		})})
	}
	return out
}

// ruleChasm: two one-to-many branches from the same table, with no grouping
// — every row of one branch paired with every row of the other.
func (l *linter) ruleChasm() {
	if l.g.grouped || len(l.tr.order) == 0 {
		return
	}
	main := l.tr.order[0]
	ms := l.tr.multipliers(main)
	for a := range ms {
		for b := a + 1; b < len(ms); b++ {
			x, y := ms[a].edge, ms[b].edge
			if x.under(y) || y.under(x) {
				continue // one chain: a row per grandchild, which is a grain, not a trap
			}
			lnt := newLint("chasm", SevWarn, areaCorrect,
				fmt.Sprintf("%s and %s multiply each other", x.alias, y.alias),
				fmt.Sprintf("%s and %s are both \"many\" from where they join, on separate branches. "+
					"Each %s row is paired with each %s row of the same %s: 3 of one and 4 of the other "+
					"make 12 rows, none of which is a real pair. Summarize or test one of them, "+
					"or query them separately.", name(x), name(y), x.alias, y.alias, main.alias))
			lnt.Tables = []string{x.t.ID, y.t.ID}
			lnt.Fixes = append(l.fanoutFixes(y), l.fanoutFixes(x)...)
			l.add(lnt)
			return // one is enough to read; fixing it re-runs the rest
		}
	}
}

// ruleFanoutUnused: a one-to-many join to a table none of whose columns are
// used — it is there only to filter, but it also repeats rows.
func (l *linter) ruleFanoutUnused() {
	for _, c := range l.tr.order {
		if c.via == nil || c.inSubquery() || !c.fromLeft && c.via.Kind != JoinInner || !l.leaf(c) || refsTo(l.d, c.t.ID) {
			continue
		}
		if l.hasCountStar() {
			continue // count(*) counts the joined rows: they are used
		}
		fans, known := l.tr.fansOut(c)
		if !fans || !known {
			continue
		}
		filtered := slices.ContainsFunc(l.d.Filters, func(f Filter) bool { return f.Table == c.t.ID })
		id, cid := c.via.ID, c.t.ID
		var x Lint
		if l.d.Distinct {
			x = newLint("distinct-fanout", SevInfo, areaCorrect, "DISTINCT is undoing the "+c.alias+" join",
				"joining "+name(c)+" repeats each "+c.parent.alias+" row once per match, and DISTINCT then "+
					"removes the copies — work twice over, and DISTINCT also merges rows that were genuinely "+
					"equal. Testing "+c.alias+" (has any) never makes the copies.")
		} else {
			x = newLint("fanout-unused", SevWarn, areaCorrect, c.alias+" repeats "+c.parent.alias+" rows, but none of its columns are used",
				"each "+c.parent.alias+" row appears once per matching "+c.alias+" row. If "+c.alias+
					" is here to keep only the "+c.parent.alias+" rows that have one, test it instead: "+
					"each row once, and the engine stops at the first match.")
		}
		x.Tables, x.Join = []string{cid}, id
		if c.via.Kind == JoinInner || filtered {
			x.Fixes = append(x.Fixes, Fix{Label: "Only test " + c.alias + " (has any)", Design: l.edit(func(d *Design) {
				pointAt(d, id, cid)
				setKind(d, id, JoinExists)
			})})
		}
		if !filtered {
			x.Fixes = append(x.Fixes, Fix{Label: "Remove " + c.alias, Design: l.edit(func(d *Design) { dropTable(d, cid) })})
		}
		l.add(x)
	}
}

func (l *linter) hasCountStar() bool {
	return slices.ContainsFunc(allRefs(l.d), func(r Ref) bool { return r.Table == "" && r.Agg == AggCount })
}

// ruleLeftFilter: a WHERE filter on the optional side of a left join. The
// unmatched rows have NULL there, the filter drops them, and the left join
// is an inner join in disguise.
func (l *linter) ruleLeftFilter() {
	for i, f := range l.d.Filters {
		n := l.tr.nodes[f.Table]
		if n == nil || !n.optional || f.Agg != AggNone || f.Op == OpNull || f.OrNull || l.g.placeOf(&l.d.Filters[i]).kind != "where" {
			continue
		}
		// the outer join that made it optional: the nearest on its path
		var outer *node
		for x := n; x != nil && x.via != nil; x = x.parent {
			if x.via.Kind == JoinFull || x.via.Kind == JoinLeft && x.fromLeft {
				outer = x
				break
			}
		}
		if outer == nil {
			continue
		}
		x := newLint("left-filter", SevWarn, areaCorrect, "this filter turns the left join to "+outer.alias+" into an inner join",
			"the left join keeps "+outer.parent.alias+" rows with no matching "+outer.alias+", filling "+n.alias+
				"'s columns with NULL — and this filter, in WHERE, then drops exactly those rows (NULL passes no comparison). "+
				"Either only matching rows are wanted (make it an inner join, and say so), or every "+outer.parent.alias+
				" row is, with "+n.alias+" narrowed (move the filter into the join).")
		x.Filter, x.Join = i, outer.via.ID
		id := outer.via.ID
		x.Fixes = append(x.Fixes, Fix{Label: "Make it an inner join", Design: l.edit(func(d *Design) { setKind(d, id, JoinInner) })})
		if outer == n {
			x.Fixes = append(x.Fixes, Fix{Label: "Move the filter into the join", Design: l.edit(func(d *Design) { d.Filters[i].InJoin = true })})
		}
		l.add(x)
	}
}

// ruleAntiJoin: LEFT JOIN … WHERE child.key IS NULL, the hand-written
// anti-join, says "has none" — which NOT EXISTS says directly.
func (l *linter) ruleAntiJoin() {
	for i, f := range l.d.Filters {
		n := l.tr.nodes[f.Table]
		if n == nil || f.Op != OpNull || f.Agg != AggNone || n.via == nil || n.via.Kind != JoinLeft || !n.fromLeft ||
			!l.leaf(n) || refsTo(l.d, n.t.ID) || n.et == nil {
			continue
		}
		c := n.et.Col(f.Column)
		if c == nil || c.Nullable {
			continue // a nullable column's NULL may be a real value
		}
		x := newLint("anti-join", SevInfo, areaCorrect, "this asks for "+n.parent.alias+" rows with no "+n.alias,
			"a left join, then "+n.alias+"."+f.Column+" IS NULL (a column that is never NULL in a real row), keeps the "+
				n.parent.alias+" rows that matched nothing. \"Has none\" (NOT EXISTS) says that directly, "+
				"and stops looking at the first match.")
		x.Filter, x.Join = i, n.via.ID
		id := n.via.ID
		x.Fixes = []Fix{{Label: "Use has none (NOT EXISTS)", Design: l.edit(func(d *Design) {
			setKind(d, id, JoinNotExists)
			d.Filters = slices.Delete(d.Filters, i, i+1)
		})}}
		l.add(x)
	}
}

// mainPK is the main table's primary key, when it is the result's grain
// (no join repeats its rows) and has one.
func (l *linter) mainPK() (*node, []string) {
	if len(l.tr.order) == 0 {
		return nil, nil
	}
	m := l.tr.order[0]
	if m.et == nil || len(m.et.PK) == 0 || len(l.tr.multipliers(m)) > 0 {
		return m, nil
	}
	return m, m.et.PK
}

func (l *linter) ruleLimitNoOrder() {
	if l.d.Limit == 0 && l.d.Offset == 0 || len(l.d.Sorts) > 0 || len(l.tr.order) == 0 {
		return
	}
	x := newLint("limit-no-order", SevWarn, areaCorrect, "a limit without a sort picks arbitrary rows",
		"with no ORDER BY the engine returns rows in whatever order is cheapest — which changes with the "+
			"plan, the data, even between runs. The \"first\" rows are not first by anything; "+
			"pages may repeat or skip rows.")
	if m, pk := l.mainPK(); len(pk) > 0 {
		x.Fixes = []Fix{{Label: "Sort by " + m.alias + "." + strings.Join(pk, ", "), Design: l.edit(func(d *Design) {
			for _, c := range pk {
				d.Sorts = append(d.Sorts, Sort{Ref: Ref{Table: m.t.ID, Column: c}})
			}
		})}}
	}
	l.add(x)
}

func (l *linter) ruleOrderNotUnique() {
	if l.d.Limit == 0 && l.d.Offset == 0 || len(l.d.Sorts) == 0 || l.g.grouped {
		return
	}
	m, pk := l.mainPK()
	if len(pk) == 0 {
		return
	}
	var sorted []string
	for _, s := range l.d.Sorts {
		if s.Table == m.t.ID && s.Agg == AggNone {
			sorted = append(sorted, s.Column)
		}
	}
	if u, _ := l.cat.unique(m.et, sorted); u {
		return
	}
	sev := SevInfo
	if l.d.Offset > 0 {
		sev = SevWarn
	}
	x := newLint("order-not-unique", sev, areaCorrect, "rows tied on the sort come back in any order",
		"the sort does not identify a row, so rows with equal values are in no particular order — "+
			"and a limit may cut through them differently each run. With an offset, a row can show on "+
			"two pages or on none. Add a unique tie-breaker as the last sort.")
	x.Fixes = []Fix{{Label: "Break ties by " + m.alias + "." + strings.Join(pk, ", "), Design: l.edit(func(d *Design) {
		for _, c := range pk {
			d.Sorts = append(d.Sorts, Sort{Ref: Ref{Table: m.t.ID, Column: c}})
		}
	})}}
	l.add(x)
}

// ruleNullDrop: <> and NOT IN on a nullable column also drop its NULLs.
func (l *linter) ruleNullDrop() {
	for i, f := range l.d.Filters {
		if f.Op != OpNe && f.Op != OpNotIn || f.OrNull || f.Agg != AggNone {
			continue
		}
		n := l.tr.nodes[f.Table]
		if n == nil || n.et == nil {
			continue
		}
		if c := n.et.Col(f.Column); c == nil || !c.Nullable {
			continue
		}
		x := newLint("null-dropped", SevInfo, areaCorrect, "rows where "+n.alias+"."+f.Column+" is empty are dropped too",
			"a NULL compared with anything is unknown, not true, so \""+f.Column+" is not …\" leaves out the rows "+
				"where it is NULL as well. If those should stay, say so.")
		x.Filter = i
		x.Fixes = []Fix{{Label: "Keep the empty ones", Design: l.edit(func(d *Design) { d.Filters[i].OrNull = true })}}
		l.add(x)
	}
}

// ruleBetweenTime: BETWEEN '…' AND '2024-01-31' on a timestamp stops at
// midnight starting the 31st.
func (l *linter) ruleBetweenTime() {
	for i, f := range l.d.Filters {
		if f.Op != OpBetween || len(f.Values) < 2 || f.Agg != AggNone || l.g.famOf(f.Ref) != famTime {
			continue
		}
		hi, err := time.Parse("2006-01-02", strings.TrimSpace(f.Values[1]))
		if err != nil {
			continue
		}
		x := newLint("between-day", SevInfo, areaCorrect, "the range ends at the start of "+f.Values[1],
			"the column holds times, and \""+f.Values[1]+"\" means midnight that morning — so the rest of that "+
				"day is left out. A half-open range (from, up to but not including the next day) keeps it.")
		x.Filter = i
		x.Fixes = []Fix{{Label: "Include all of " + f.Values[1], Design: l.edit(func(d *Design) {
			d.Filters[i].Op = OpRange
			d.Filters[i].Values = []string{f.Values[0], hi.AddDate(0, 0, 1).Format("2006-01-02")}
		})}}
		l.add(x)
	}
}

func (l *linter) ruleCountNullable() {
	for i, o := range l.d.Columns {
		if o.Agg != AggCount || o.Column == "" {
			continue
		}
		n := l.tr.nodes[o.Table]
		if n == nil || n.et == nil {
			continue
		}
		if c := n.et.Col(o.Column); c == nil || !c.Nullable {
			continue
		}
		x := newLint("count-nullable", SevInfo, areaCorrect, "count("+n.alias+"."+o.Column+") skips its empty values",
			"count of a column counts the rows where it is not NULL. To count the rows themselves, count rows.")
		x.Column = i
		x.Fixes = []Fix{{Label: "Count " + n.alias + " rows", Design: l.edit(func(d *Design) { d.Columns[i].Column = "" })}}
		l.add(x)
	}
}

func (l *linter) ruleRightJoin() {
	for _, j := range l.tr.rightJoins {
		keep := l.tr.nodes[j.Left]
		x := newLint("right-join", SevInfo, areaCorrect, "written as a right join",
			"the query starts from "+name(l.tr.order[0])+" but the join keeps every row of "+name(keep)+
				", which is reached later — so it is a RIGHT JOIN, which keeps "+keep.alias+"'s rows against everything joined "+
				"before it. Starting from "+keep.alias+" reads more naturally and means what a left join usually does.")
		x.Join = j.ID
		id := keep.t.ID
		x.Fixes = []Fix{{Label: "Start from " + keep.alias, Design: l.edit(func(d *Design) { d.Main = id })}}
		l.add(x)
	}
}

func (l *linter) ruleGuessed() {
	for i := range l.d.Joins {
		j := &l.d.Joins[i]
		if !j.Guess {
			continue
		}
		x := newLint("guessed-join", SevInfo, areaCorrect, "this join was guessed from the column names",
			"no foreign key links these tables, so the designer matched them by name. Nothing guarantees every "+
				"value has a match, or that the columns mean the same thing — check a few rows.")
		x.Join = j.ID
		l.add(x)
	}
}

// ── performance ────────────────────────────────────────────────────────

// sargable operators can be answered from an index on the column: an
// equality, a range, a prefix. The others (<>, NOT IN, a leading-wildcard
// LIKE) must read every row.
var sargable = map[string]bool{
	OpEq: true, OpIn: true, OpNull: true, OpLt: true, OpLe: true, OpGt: true, OpGe: true,
	OpBetween: true, OpRange: true, OpStarts: true, OpYear: true, OpOn: true, OpLastDays: true,
}

// equality operators: an index's leading columns must be these for a later
// column (a range, a sort) to be served by it too.
var equality = map[string]bool{OpEq: true, OpIn: true, OpNull: true}

// filterCols are a node's sargable filter columns, equalities first, in the
// order added; filters in OR groups are left out (ruleOrColumns).
func (l *linter) filterCols(n *node) (eq, rng []string) {
	for i, f := range l.d.Filters {
		if f.Table != n.t.ID || f.Agg != AggNone || f.Group != "" || !sargable[f.Op] || f.OrNull {
			continue
		}
		if p := l.g.placeOf(&l.d.Filters[i]); p.kind == "having" {
			continue
		}
		if equality[f.Op] {
			if !slices.Contains(eq, f.Column) {
				eq = append(eq, f.Column)
			}
		} else if !slices.Contains(rng, f.Column) {
			rng = append(rng, f.Column)
		}
	}
	return eq, rng
}

// createIndex writes the CREATE INDEX a finding suggests. The name follows
// Postgres's own convention (table_cols_idx), cut to its 63-byte limit.
func (l *linter) createIndex(n *node, cols []string) string {
	nm := n.t.Name + "_" + strings.Join(cols, "_") + "_idx"
	if len(nm) > 63 {
		nm = nm[:63]
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = l.g.dia.ident(c)
	}
	return "CREATE INDEX " + l.g.dia.ident(safeName(nm)) + " ON " + l.g.tableRef(n) + " (" + strings.Join(quoted, ", ") + ");"
}

func (l *linter) sizeWords(n *node) string {
	r, _ := l.cat.rows(n.et)
	return "about " + roughCount(r) + " rows"
}

func (l *linter) ruleFilterIndex() {
	if l.cat == nil || !l.cat.IndexesKnown {
		return
	}
	for _, n := range l.tr.order {
		if n.et == nil || n.et.View || !l.cat.big(n.et, bigTable) {
			continue
		}
		eq, rng := l.filterCols(n)
		cols := append(slices.Clone(eq), rng...)
		if len(cols) == 0 {
			continue
		}
		if _, ok := l.cat.leads(n.et, cols...); ok {
			continue
		}
		// the index to suggest: the equalities, then one range column —
		// the order in which a b-tree can use them all
		want := slices.Clone(eq)
		if len(rng) > 0 {
			want = append(want, rng[0])
		}
		x := newLint("filter-no-index", SevWarn, areaPerf, n.alias+" is read whole to filter it",
			name(n)+" has "+l.sizeWords(n)+", and no index starts with "+strings.Join(cols, " or ")+
				", so finding the matching rows means reading every one. An index on ("+strings.Join(want, ", ")+
				") — equalities first, then the range — lets the engine go straight to them."+pgConcurrently(l.g.dia))
		x.Tables = []string{n.t.ID}
		x.Fixes = []Fix{{Label: "Copy CREATE INDEX", SQL: l.createIndex(n, want)}}
		l.add(x)
	}
}

func pgConcurrently(d dialect) string {
	if d.name == "postgres" {
		return " On a busy table, CREATE INDEX CONCURRENTLY builds it without blocking writes."
	}
	return ""
}

// ruleJoinIndex: a big table reached by a join on columns no index starts
// with. For each row from the other side, the engine must either hash the
// whole table or scan it; an index lets it look the matches up.
func (l *linter) ruleJoinIndex() {
	if l.cat == nil || !l.cat.IndexesKnown {
		return
	}
	for _, c := range l.tr.order {
		if c.via == nil || c.et == nil || c.et.View || len(c.via.On) == 0 || !l.cat.big(c.et, bigTable) {
			continue
		}
		_, cols := edgeCols(c)
		if _, ok := l.cat.leads(c.et, cols...); ok {
			continue
		}
		sev := SevInfo
		peq, prng := l.filterCols(c.parent)
		narrowed := len(peq)+len(prng) > 0 || l.d.Limit > 0
		if l.cat.big(c.et, hugeTable) && (narrowed || c.via.Kind.tests()) {
			sev = SevWarn
		}
		why := name(c) + " has " + l.sizeWords(c) + " and no index starts with " + strings.Join(cols, ", ") + ". "
		if narrowed || c.via.Kind.tests() {
			why += "Only some " + c.parent.alias + " rows need their matches, but without an index the engine " +
				"must read all of " + c.alias + " to find them."
		} else {
			why += "The engine can still hash-join it (one full read), which is fine for a report over everything — " +
				"but any query that needs only a few " + c.parent.alias + " rows' matches will read it whole too."
		}
		if u, _ := l.cat.unique(c.et, cols); c.via.FK != "" && !u && l.g.dia.name == "postgres" {
			// c holds the foreign key's columns: Postgres indexes the
			// referenced key, never the referencing columns
			why += " Postgres does not index a foreign key's columns by itself."
		}
		x := newLint("join-no-index", sev, areaPerf, "the join to "+c.alias+" has no index to use", why)
		x.Tables, x.Join = []string{c.t.ID}, c.via.ID
		x.Fixes = []Fix{{Label: "Copy CREATE INDEX", SQL: l.createIndex(c, cols)}}
		l.add(x)
	}
}

func (l *linter) ruleContains() {
	for i, f := range l.d.Filters {
		if f.Op != OpContains && f.Op != OpEnds || f.Agg != AggNone {
			continue
		}
		n := l.tr.nodes[f.Table]
		if n == nil || n.et == nil || !l.cat.big(n.et, bigTable) {
			continue
		}
		why := "a pattern that does not start with fixed text cannot use an ordinary index, so each of " +
			name(n) + "'s " + l.sizeWords(n) + " is read and matched."
		x := newLint("leading-wildcard", SevWarn, areaPerf, "\""+opWords[f.Op]+"\" reads every row of "+n.alias, why)
		x.Filter = i
		if l.g.dia.name == "postgres" {
			x.Why += " On Postgres a trigram index (pg_trgm) serves contains and ends-with."
			idx := safeName(n.t.Name + "_" + f.Column + "_trgm_idx")
			x.Fixes = []Fix{{Label: "Copy a trigram index", SQL: "CREATE EXTENSION IF NOT EXISTS pg_trgm;\nCREATE INDEX " +
				idx + " ON " + l.g.tableRef(n) + " USING gin (" + l.g.dia.ident(f.Column) + " gin_trgm_ops);"}}
		}
		if f.Op == OpContains {
			x.Fixes = append(x.Fixes, Fix{Label: "Use starts with instead", Design: l.edit(func(d *Design) { d.Filters[i].Op = OpStarts })})
		}
		l.add(x)
	}
}

// ruleSortIndex: top-N by a column of a big table with no index in that
// order: the engine sorts every matching row to keep N.
func (l *linter) ruleSortIndex() {
	if l.cat == nil || !l.cat.IndexesKnown || l.d.Limit == 0 || l.g.grouped || len(l.d.Sorts) == 0 || len(l.tr.order) == 0 {
		return
	}
	m := l.tr.order[0]
	if m.et == nil || !l.cat.big(m.et, bigTable) {
		return
	}
	var sorted []string
	for _, s := range l.d.Sorts {
		if s.Table != m.t.ID || s.Agg != AggNone {
			return
		}
		sorted = append(sorted, s.Column)
	}
	eq, _ := l.filterCols(m)
	want := append(slices.Clone(eq), sorted...)
	for _, x := range l.cat.indexes(m.et) {
		if !x.Partial && len(x.Cols) >= len(want) && slices.Equal(x.Cols[:len(want)], want) {
			return
		}
		// an index on the equalities followed by the sort serves it; so does
		// one on the sort alone when there are no equalities
		if len(eq) == 0 && len(x.Cols) >= len(sorted) && slices.Equal(x.Cols[:len(sorted)], sorted) {
			return
		}
	}
	x := newLint("sort-no-index", SevInfo, areaPerf, "the first "+strconv.Itoa(l.d.Limit)+" rows need a full sort",
		"to return the top "+strconv.Itoa(l.d.Limit)+" by "+strings.Join(sorted, ", ")+", the engine must sort every matching row of "+
			name(m)+" ("+l.sizeWords(m)+"). An index on ("+strings.Join(want, ", ")+") holds them in that order already, "+
			"so it can read the first "+strconv.Itoa(l.d.Limit)+" and stop.")
	x.Tables = []string{m.t.ID}
	x.Fixes = []Fix{{Label: "Copy CREATE INDEX", SQL: l.createIndex(m, want)}}
	l.add(x)
}

func (l *linter) ruleDeepOffset() {
	if l.d.Offset < 1000 {
		return
	}
	l.add(newLint("deep-offset", SevInfo, areaPerf, "an offset of "+strconv.Itoa(l.d.Offset)+" still reads the rows it skips",
		"OFFSET makes the engine produce and throw away every row before it, so page 500 costs 500 pages. "+
			"Keyset paging remembers the last row's sort values and asks for the rows after them "+
			"(WHERE (created_at, id) > (…) ORDER BY created_at, id LIMIT n): every page costs the same."))
}

func (l *linter) ruleUnbounded() {
	if l.g.grouped || l.d.Limit > 0 || len(l.d.Filters) > 0 || len(l.tr.order) == 0 {
		return
	}
	m := l.tr.order[0]
	if m.et == nil || !l.cat.big(m.et, hugeTable) {
		return
	}
	x := newLint("unbounded", SevInfo, areaPerf, "this returns every row: "+l.sizeWords(m),
		"with no filter and no limit, the whole of "+name(m)+" (and its joins) is read and sent. "+
			"If you are looking rather than exporting, a limit returns at once.")
	x.Fixes = []Fix{{Label: "Limit to 1,000 rows", Design: l.edit(func(d *Design) { d.Limit = 1000 })}}
	l.add(x)
}

func (l *linter) ruleOrColumns() {
	cols := map[string]map[string]bool{}
	tables := map[string]string{}
	for _, f := range l.d.Filters {
		if f.Group == "" || f.Agg != AggNone {
			continue
		}
		if cols[f.Group] == nil {
			cols[f.Group] = map[string]bool{}
		}
		cols[f.Group][f.Table+"."+f.Column] = true
		tables[f.Group] = f.Table
	}
	for grp, cs := range cols {
		n := l.tr.nodes[tables[grp]]
		if len(cs) < 2 || n == nil || !l.cat.big(n.et, bigTable) {
			continue
		}
		l.add(newLint("or-columns", SevInfo, areaPerf, "an \"any of\" group across "+strconv.Itoa(len(cs))+" columns",
			"one index serves one column's condition; OR across columns needs a row to be checked against each. "+
				"Postgres can combine one index per column (a BitmapOr) when each exists; other engines often read "+
				"the whole table. If this is slow, a UNION of one query per column uses each index."))
	}
}

func (l *linter) ruleSelectAll() {
	if len(l.d.Columns) > 0 || len(l.tr.order) == 0 || l.g.grouped {
		return
	}
	x := newLint("select-all", SevInfo, areaPerf, "no columns picked: every column is returned",
		"tick the columns you need. Fewer columns are less to read and send, and when every column a query "+
			"needs is in an index, the engine can answer from the index alone.")
	l.add(x)
}
