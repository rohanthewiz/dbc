package designer

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Writing the statement. What the generator decides for the user, so that
// they cannot get it wrong:
//
//   - GROUP BY is exactly the non-aggregated output columns, whenever any
//     aggregate is asked for; a filter on an aggregate goes to HAVING, one
//     on a row to WHERE.
//   - An OR group is one parenthesized condition.
//   - Values are written as the column's type wants them, and checked: a
//     number column gets a number, a date column a date (issues).
//   - Date filters are ranges (`d >= '2024-01-01' AND d < '2025-01-01'`),
//     which an index on d serves, never a function of d, which it cannot.
//   - A LIKE value's own % and _ are escaped, so "50%" means fifty percent.
//   - Counting a table's rows counts what the user means through the joins:
//     count(*) when the table is the result's grain, count(DISTINCT key)
//     when a join repeats its rows, count(key) when an outer join may leave
//     it empty (count(*) would count the empty match as one).
//   - A summarized child is aggregated in a derived table before the join;
//     an EXISTS child is tested in WHERE, never joined.

// now is the clock, for the date filters an engine has no expression for;
// tests set it.
var now = time.Now

// issue is a problem the generator met while writing: a value that does not
// fit its column, a reference it could not resolve. The rules report them
// (ruleIssues) as errors — the SQL was written as well as it could be, but
// is not to be run.
type issue struct {
	what  string // "filter", "column", "sort"
	index int
	title string
	why   string
}

// gen writes one statement.
type gen struct {
	d       *Design
	tr      *tree
	dia     dialect
	grouped bool
	sums    map[*node]*summary
	issues  []issue
	// names are the output columns' names, as the SELECT list gives them
	// (for the story, and the page's grid header hints)
	names []string
}

// summary is the derived table of one summarized child: the aggregates the
// design asks of it, each once, in the order first asked.
type summary struct {
	items []sumItem
}

type sumItem struct {
	agg  Agg
	col  string // "" for count(*)
	name string // its column in the derived table
}

// need returns the derived table's column for agg(col), adding it.
func (s *summary) need(agg Agg, col string) string {
	for _, it := range s.items {
		if it.agg == agg && it.col == col {
			return it.name
		}
	}
	name := string(agg)
	if col != "" {
		name += "_" + safeName(col)
	}
	s.items = append(s.items, sumItem{agg: agg, col: col, name: name})
	return name
}

// safeName makes a column name usable inside a generated alias: lower case,
// anything but letters and digits as _.
func safeName(s string) string {
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			b[i] = '_'
		}
	}
	return string(b)
}

// report records an issue. what "" is a ref written again for another
// purpose (the GROUP BY, a comparison with the outputs), whose issues were
// reported when it was first written.
func (g *gen) report(what string, i int, title, why string) {
	if what == "" {
		return
	}
	g.issues = append(g.issues, issue{what: what, index: i, title: title, why: why})
}

// node of a ref, or nil.
func (g *gen) node(r Ref) *node {
	if r.Table == "" {
		return nil
	}
	return g.tr.nodes[r.Table]
}

// isGrouped decides whether the statement aggregates: some output, filter or
// sort asks for an aggregate of a table that is not summarized (or for
// count(*) of the whole result). A summarized child's aggregates alone do
// not group the outer statement — its summary is already one row per
// parent, and grouping again would merge parents that share the grouped
// columns' values.
func isGrouped(d *Design, tr *tree) bool {
	agg := func(r Ref) bool {
		if r.Agg == AggNone {
			return false
		}
		n := tr.nodes[r.Table]
		return n == nil || n.role() != "summary"
	}
	for _, o := range d.Columns {
		if agg(o.Ref) {
			return true
		}
	}
	for _, f := range d.Filters {
		if agg(f.Ref) {
			return true
		}
	}
	for _, s := range d.Sorts {
		if agg(s.Ref) {
			return true
		}
	}
	return false
}

// col is alias.column.
func (g *gen) col(n *node, c string) string { return n.alias + "." + g.dia.ident(c) }

// expr writes a ref as an expression of the outer statement. what and i say
// whose ref it is, for an issue.
func (g *gen) expr(r Ref, what string, i int) string {
	if r.Table == "" {
		if r.Agg == AggCount && r.Column == "" {
			return "count(*)"
		}
		g.report(what, i, "no table", "it names no table of the canvas")
		return "NULL"
	}
	n := g.node(r)
	if n == nil {
		g.report(what, i, "a table no longer on the canvas", "it refers to a table that was removed")
		return "NULL"
	}
	if r.Column != "" && n.et != nil && n.et.Col(r.Column) == nil {
		g.report(what, i, "no column "+n.alias+"."+r.Column,
			"`"+n.et.Label+"` has no column `"+r.Column+"` — renamed or dropped since the design was made?")
	}
	switch n.role() {
	case "summary":
		return g.summaryExpr(n, r, what, i)
	case "exists", "notexists":
		g.report(what, i, "a tested table's column", "`"+n.alias+"` is joined as \""+
			kindWords[n.via.Kind]+"\": it is only tested, so its columns are not in the result. "+
			"Change the join to inner or left to show them.")
		return "NULL"
	}
	if r.Agg == AggNone {
		if r.Column == "" {
			g.report(what, i, "no column", "pick a column, or an aggregate such as count")
			return "NULL"
		}
		return g.col(n, r.Column)
	}
	if r.Column == "" {
		if r.Agg != AggCount {
			g.report(what, i, string(r.Agg)+" of no column", "pick the column to "+string(r.Agg))
			return "NULL"
		}
		return g.countRows(n)
	}
	return aggCall(r.Agg, g.col(n, r.Column))
}

// aggCall is agg(x).
func aggCall(a Agg, x string) string {
	if a == AggCountDistinct {
		return "count(DISTINCT " + x + ")"
	}
	return string(a) + "(" + x + ")"
}

// countRows writes "the number of n's rows" so that it means that through
// the joins (see the top of this file).
func (g *gen) countRows(n *node) string {
	key := ""
	if n.et != nil && len(n.et.PK) == 1 {
		key = g.col(n, n.et.PK[0])
	}
	if key != "" && len(g.tr.multipliers(n)) > 0 {
		return "count(DISTINCT " + key + ")"
	}
	if key != "" && n.optional {
		return "count(" + key + ")"
	}
	return "count(*)"
}

// summaryExpr writes a ref to a summarized child: its derived table's
// column, re-aggregated when the outer statement is grouped too.
//
//	                 outer not grouped        outer grouped
//	sum(x)           s.sum_x                  sum(s.sum_x)
//	count(x)/(*)     COALESCE(s.count_x, 0)   COALESCE(sum(s.count_x), 0)
//	min/max(x)       s.min_x                  min(s.min_x)
//	avg(x)           s.avg_x                  sum(s.sum_x)·1.0 / sum(s.count_x)
//	count(DISTINCT)  s.count_distinct_x       — (cannot be combined: issue)
//
// A count is COALESCEd because the derived table is LEFT JOINed: a parent
// with no children has no summary row, and its count is 0, not NULL — as
// a correlated count(*) would say. A sum stays NULL there, which is also
// what SQL's sum of no rows is. avg is re-aggregated from its parts: an
// average of averages weighs a parent with one child like one with a
// thousand.
func (g *gen) summaryExpr(n *node, r Ref, what string, i int) string {
	if r.Agg == AggNone {
		g.report(what, i, "a summarized table's plain column", "`"+n.alias+"` is summarized "+
			"(one row per "+g.tr.nodes[n.parent.t.ID].alias+"): only aggregates of it can be shown — "+
			"pick sum, count, min, max or avg, or change the join to inner or left.")
		return "NULL"
	}
	s := g.sums[n]
	if r.Agg == AggAvg && g.grouped {
		sum, cnt := s.need(AggSum, r.Column), s.need(AggCount, r.Column)
		return "sum(" + n.alias + "." + sum + ") * 1.0 / NULLIF(sum(" + n.alias + "." + cnt + "), 0)"
	}
	if r.Agg == AggCountDistinct && g.grouped {
		g.report(what, i, "count distinct across summaries", "distinct values counted per "+
			n.parent.alias+" cannot be added up across a group: the same value may be counted under "+
			"two of them. Count it with an inner join and count distinct instead.")
		return "NULL"
	}
	name := n.alias + "." + s.need(r.Agg, r.Column)
	if g.grouped {
		outer := r.Agg
		if outer == AggCount {
			outer = AggSum
		}
		name = string(outer) + "(" + name + ")"
	}
	if r.Agg == AggCount {
		return "COALESCE(" + name + ", 0)"
	}
	return name
}

// ── values ─────────────────────────────────────────────────────────────

var (
	numRE  = regexp.MustCompile(`^-?(\d+\.?\d*|\.\d+)([eE][-+]?\d+)?$`)
	dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}([ T]\d{2}:\d{2}(:\d{2}(\.\d+)?)?([+-]\d{2}(:?\d{2})?|Z)?)?$`)
	uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)
)

// value writes v as a literal for a column of family fam, reporting one
// that does not fit (a word for a number, a date the engine would refuse).
func (g *gen) value(fam family, v string, i int) string {
	v = strings.TrimSpace(v)
	bad := func(want string) string {
		g.report("filter", i, "\""+v+"\" is not "+want, "the column holds "+want+"s; "+
			"the engine would refuse the comparison, or quietly compare it as text")
		return g.dia.lit(v)
	}
	switch fam {
	case famInt, famNum:
		if !numRE.MatchString(v) {
			return bad("a number")
		}
		return v
	case famBool:
		switch strings.ToLower(v) {
		case "true", "t", "yes", "y", "1", "on":
			return "TRUE"
		case "false", "f", "no", "n", "0", "off":
			return "FALSE"
		}
		return bad("true or false")
	case famDate, famTime:
		if !dateRE.MatchString(v) {
			return bad("a date (YYYY-MM-DD)")
		}
	case famUUID:
		if !uuidRE.MatchString(v) {
			return bad("a UUID")
		}
	}
	return g.dia.lit(v)
}

// famOf is the family of what a ref yields: a count is an integer, a sum or
// avg a number, anything else its column's.
func (g *gen) famOf(r Ref) family {
	switch r.Agg {
	case AggCount, AggCountDistinct:
		return famInt
	case AggSum, AggAvg:
		return famNum
	}
	if n := g.node(r); n != nil && n.et != nil {
		if c := n.et.Col(r.Column); c != nil {
			return familyOf(c.Type)
		}
	}
	return famUnknown
}

// cond writes filter i as a condition.
func (g *gen) cond(f *Filter, i int, x string) string {
	fam := g.famOf(f.Ref)
	want, ok := opArity[f.Op]
	if !ok {
		g.report("filter", i, "unknown operator "+f.Op, "pick a comparison from the list")
		return "1 = 1"
	}
	vals := f.Values
	if want >= 0 && len(vals) < want || want < 0 && len(vals) == 0 {
		g.report("filter", i, "a value is missing", "this comparison needs "+arityWords(want))
		return "1 = 1"
	}
	v := func(k int) string { return g.value(fam, vals[k], i) }
	var c string
	switch f.Op {
	case OpEq:
		c = x + " = " + v(0)
	case OpNe:
		c = x + " <> " + v(0)
	case OpLt:
		c = x + " < " + v(0)
	case OpLe:
		c = x + " <= " + v(0)
	case OpGt:
		c = x + " > " + v(0)
	case OpGe:
		c = x + " >= " + v(0)
	case OpBetween:
		c = x + " BETWEEN " + v(0) + " AND " + v(1)
	case OpRange:
		c = x + " >= " + v(0) + " AND " + x + " < " + v(1)
	case OpIn, OpNotIn:
		parts := make([]string, len(vals))
		for k := range vals {
			parts[k] = v(k)
		}
		kw := " IN ("
		if f.Op == OpNotIn {
			kw = " NOT IN ("
		}
		c = x + kw + strings.Join(parts, ", ") + ")"
	case OpNull:
		c = x + " IS NULL"
	case OpNotNull:
		c = x + " IS NOT NULL"
	case OpContains, OpStarts, OpEnds:
		c = g.like(x, f.Op, vals[0], f.IgnoreCase)
	case OpYear:
		y, err := strconv.Atoi(strings.TrimSpace(vals[0]))
		if err != nil || y < 1 || y > 9998 {
			g.report("filter", i, "\""+vals[0]+"\" is not a year", "a year is four digits, such as 2024")
			return "1 = 1"
		}
		c = x + " >= " + g.dia.lit(fmt.Sprintf("%04d-01-01", y)) + " AND " + x + " < " + g.dia.lit(fmt.Sprintf("%04d-01-01", y+1))
	case OpOn:
		day, err := time.Parse("2006-01-02", strings.TrimSpace(vals[0]))
		if err != nil {
			g.report("filter", i, "\""+vals[0]+"\" is not a day", "a day is written YYYY-MM-DD")
			return "1 = 1"
		}
		if fam == famDate {
			c = x + " = " + g.dia.lit(day.Format("2006-01-02"))
		} else {
			c = x + " >= " + g.dia.lit(day.Format("2006-01-02")) + " AND " + x + " < " + g.dia.lit(day.AddDate(0, 0, 1).Format("2006-01-02"))
		}
	case OpLastDays:
		n, err := strconv.Atoi(strings.TrimSpace(vals[0]))
		if err != nil || n < 1 || n > 100_000 {
			g.report("filter", i, "\""+vals[0]+"\" is not a number of days", "give a whole number of days, such as 7")
			return "1 = 1"
		}
		c = x + " >= " + g.daysAgo(n, fam == famDate)
	}
	if f.OrNull && f.Op != OpNull && f.Op != OpNotNull {
		c = "(" + c + " OR " + x + " IS NULL)"
	}
	return c
}

func arityWords(n int) string {
	switch n {
	case 1:
		return "a value"
	case 2:
		return "two values"
	}
	return "at least one value"
}

// like writes a LIKE for contains, starts or ends. The value's own % and _
// are escaped (and the escape character itself), with ESCAPE '\' written
// out: SQLite has no default escape, and spelling it makes the statement
// mean the same on every engine.
//
// icase asks for letters to match whatever their case:
//
//	postgres, bytdb   x ILIKE 'pat'                 the engine's own operator,
//	                                                which a trigram index serves
//	mysql, sqlite     lower(x) LIKE lower('pat')    both sides folded by the
//	                                                engine, so its lower() and
//	                                                its LIKE agree on what a
//	                                                letter is
//
// The pattern is lowered by the engine rather than here: Go's ToLower
// folds all of Unicode, SQLite's lower() only A–Z, and a pattern folded
// further than the column could never match it. On MySQL lower() is
// redundant for a column whose collation already ignores case (the
// default …_ci), but it is right for a _bin or _cs one, which the
// designer cannot tell apart; ruleContains points the trade-off out.
func (g *gen) like(x, op, v string, icase bool) string {
	esc := strings.ContainsAny(v, `%_\`)
	if esc {
		v = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v)
	}
	switch op {
	case OpContains:
		v = "%" + v + "%"
	case OpStarts:
		v += "%"
	case OpEnds:
		v = "%" + v
	}
	var c string
	switch {
	case !icase:
		c = x + " LIKE " + g.dia.lit(v)
	case g.dia.ilike:
		c = x + " ILIKE " + g.dia.lit(v)
	default:
		c = "lower(" + x + ") LIKE lower(" + g.dia.lit(v) + ")"
	}
	if esc {
		c += " ESCAPE " + g.dia.lit(`\`)
	}
	return c
}

// daysAgo is the instant n days before now, in the engine's own terms so the
// statement stays current when run again tomorrow; on bytdb, which has no
// date arithmetic to rely on, the instant itself.
func (g *gen) daysAgo(n int, date bool) string {
	switch g.dia.name {
	case "postgres":
		if date {
			return "current_date - " + strconv.Itoa(n)
		}
		return "now() - interval '" + strconv.Itoa(n) + " days'"
	case "mysql":
		if date {
			return "CURDATE() - INTERVAL " + strconv.Itoa(n) + " DAY"
		}
		return "NOW() - INTERVAL " + strconv.Itoa(n) + " DAY"
	case "sqlite":
		if date {
			return "date('now', '-" + strconv.Itoa(n) + " days')"
		}
		return "datetime('now', '-" + strconv.Itoa(n) + " days')"
	}
	t := now().AddDate(0, 0, -n)
	if date {
		return g.dia.lit(t.Format("2006-01-02"))
	}
	return g.dia.lit(t.Format("2006-01-02 15:04:05"))
}

// ── placing filters ────────────────────────────────────────────────────

// where a filter is written: the outer WHERE, HAVING, a subquery's WHERE
// (sub: its node), a join's ON (on: its node).
type place struct {
	kind string // "where", "having", "sub", "on"
	n    *node
}

func (g *gen) placeOf(f *Filter) place {
	n := g.node(f.Ref)
	if f.Agg != AggNone {
		if n != nil && n.role() == "summary" && !g.grouped {
			return place{kind: "where"}
		}
		return place{kind: "having"}
	}
	if n != nil && n.inSubquery() {
		return place{kind: "sub", n: n}
	}
	if n != nil && f.InJoin && n.via != nil {
		return place{kind: "on", n: n}
	}
	return place{kind: "where"}
}

// conds writes the filters placed at p, each OR group as one parenthesized
// condition, in the order the filters were added. Inside a subquery the
// filters are written against the subquery's own table, as in the outer
// statement — the alias is the same, and the subquery's scope has it.
func (g *gen) conds(p place) []string {
	var out []string
	groups := map[string]int{} // group → index in out
	var members = map[string][]string{}
	for i := range g.d.Filters {
		f := &g.d.Filters[i]
		if g.placeOf(f) != p {
			continue
		}
		var x string
		if p.kind == "sub" {
			x = g.col(p.n, f.Column)
		} else {
			x = g.expr(f.Ref, "filter", i)
		}
		c := g.cond(f, i, x)
		if f.Group == "" {
			out = append(out, c)
			continue
		}
		if _, ok := groups[f.Group]; !ok {
			groups[f.Group] = len(out)
			out = append(out, "")
		}
		members[f.Group] = append(members[f.Group], c)
	}
	for grp, at := range groups {
		ms := members[grp]
		if len(ms) == 1 {
			out[at] = ms[0]
			continue
		}
		for k, m := range ms {
			if strings.Contains(m, " AND ") {
				ms[k] = "(" + m + ")"
			}
		}
		out[at] = "(" + strings.Join(ms, " OR ") + ")"
	}
	return out
}

// ── the statement ──────────────────────────────────────────────────────

// tableRef is how the FROM clause names a canvas table: schema-qualified
// when the connection shows several schemas (the sidebar's label does),
// bare otherwise.
func (g *gen) tableRef(n *node) string {
	schema, name := n.t.Schema, n.t.Name
	qualify := schema != ""
	if n.et != nil {
		qualify = n.et.Label != n.et.Name
	}
	if qualify {
		return g.dia.ident(schema) + "." + g.dia.ident(name)
	}
	return g.dia.ident(name)
}

// on is a join's conditions for child c: its tree edge's pairs, the cycle
// joins that close on it, and the filters asked to go into its join.
func (g *gen) on(c *node) []string {
	var out []string
	pairs := func(j *Join, a, b *node) {
		// a is the side the condition is written from (the newer table)
		aLeft := j.Left == a.t.ID
		for _, p := range j.On {
			ac, bc := p.R, p.L
			if aLeft {
				ac, bc = p.L, p.R
			}
			out = append(out, g.col(a, ac)+" = "+g.col(b, bc))
		}
	}
	pairs(c.via, c, c.parent)
	for _, j := range c.extra {
		other := g.tr.nodes[j.Left]
		if other == c {
			other = g.tr.nodes[j.Right]
		}
		pairs(j, c, other)
	}
	if len(out) == 0 {
		out = append(out, "1 = 1") // ruleNoCondition reports it
	}
	return append(out, g.conds(place{kind: "on", n: c})...)
}

// write produces the statement.
func (g *gen) write() string {
	var b strings.Builder

	// summaries first: an output may ask one for a column (need), and the
	// derived tables are written into FROM afterwards
	g.sums = map[*node]*summary{}
	for _, n := range g.tr.order {
		if n.role() == "summary" {
			g.sums[n] = &summary{}
		}
	}

	// SELECT
	sel, groupBy := g.selectList()
	b.WriteString("SELECT")
	if g.d.Distinct {
		b.WriteString(" DISTINCT")
	}
	if len(sel) <= 3 && len(strings.Join(sel, ", ")) <= 72 {
		b.WriteString(" " + strings.Join(sel, ", "))
	} else {
		b.WriteString("\n    " + strings.Join(sel, ",\n    "))
	}

	// ORDER BY is written now (into a string) for the same reason as the
	// select list: it may ask a summary for a column
	orderBy := g.orderBy(groupBy)
	having := g.conds(place{kind: "having"})
	where := g.conds(place{kind: "where"})

	// FROM: the tree in placement order. Tested tables go to WHERE.
	var exists []string
	for _, n := range g.tr.order {
		switch n.role() {
		case "main":
			b.WriteString("\nFROM " + g.tableRef(n) + " " + n.alias)
		case "cross":
			b.WriteString("\nCROSS JOIN " + g.tableRef(n) + " " + n.alias)
		case "exists", "notexists":
			exists = append(exists, g.existsCond(n))
		case "summary":
			b.WriteString("\nLEFT JOIN (\n" + indent(g.summarySelect(n), "    ") + "\n) " + n.alias +
				" ON " + strings.Join(g.on(n), " AND "))
		default:
			b.WriteString("\n" + joinKeyword(n) + " " + g.tableRef(n) + " " + n.alias + " ON " + strings.Join(g.on(n), " AND "))
		}
	}
	where = append(where, exists...)
	if len(where) > 0 {
		b.WriteString("\nWHERE " + strings.Join(where, "\n  AND "))
	}
	if g.grouped && len(groupBy) > 0 {
		b.WriteString("\nGROUP BY " + strings.Join(groupBy, ", "))
	}
	if len(having) > 0 {
		b.WriteString("\nHAVING " + strings.Join(having, "\n   AND "))
	}
	if orderBy != "" {
		b.WriteString("\nORDER BY " + orderBy)
	}
	switch {
	case g.d.Limit > 0:
		b.WriteString("\nLIMIT " + strconv.Itoa(g.d.Limit))
	case g.d.Offset > 0 && g.dia.name == "sqlite":
		b.WriteString("\nLIMIT -1") // SQLite's OFFSET needs a LIMIT; -1 is none
	case g.d.Offset > 0 && g.dia.name == "mysql":
		b.WriteString("\nLIMIT 18446744073709551615") // MySQL's own "no limit" for an OFFSET
	}
	if g.d.Offset > 0 {
		b.WriteString("\nOFFSET " + strconv.Itoa(g.d.Offset))
	}
	return b.String()
}

// joinKeyword is the JOIN a tree edge is written with.
func joinKeyword(c *node) string {
	switch c.via.Kind {
	case JoinLeft:
		if c.fromLeft {
			return "LEFT JOIN"
		}
		return "RIGHT JOIN"
	case JoinFull:
		return "FULL JOIN"
	}
	return "JOIN"
}

// selectList writes the output columns, and the GROUP BY they imply. Each
// output gets a name unique in the result: its alias, else its column's
// name, else (an aggregate) agg_column; a column name already taken is
// prefixed with its table's alias (c_name, p_name) so the grid can tell
// them apart.
func (g *gen) selectList() (sel, groupBy []string) {
	g.names = nil
	taken := map[string]bool{}
	for i, o := range g.d.Columns {
		x := g.expr(o.Ref, "column", i)
		name, as := o.Alias, o.Alias != ""
		if name == "" {
			switch {
			case o.Agg == AggNone:
				name = o.Column
			case o.Column == "":
				name = "count"
				if o.Table != "" {
					if n := g.node(o.Ref); n != nil {
						name = "count_" + safeName(n.t.Name)
					}
				}
				as = true
			default:
				name, as = string(o.Agg)+"_"+safeName(o.Column), true
			}
		}
		if taken[strings.ToLower(name)] {
			base := name
			if n := g.node(o.Ref); n != nil && o.Agg == AggNone {
				base = n.alias + "_" + safeName(name)
			}
			name = base
			for k := 2; taken[strings.ToLower(name)]; k++ {
				name = base + "_" + strconv.Itoa(k)
			}
			as = true
		}
		taken[strings.ToLower(name)] = true
		g.names = append(g.names, name)
		// p.sum_amount already has the name sum_amount: no AS needed
		if as && !strings.HasSuffix(x, "."+g.dia.ident(name)) {
			x += " AS " + g.dia.ident(name)
		}
		sel = append(sel, x)
		if g.grouped && o.Agg == AggNone {
			gx := g.expr(o.Ref, "", -1)
			if !slices.Contains(groupBy, gx) {
				groupBy = append(groupBy, gx)
			}
		}
	}
	if len(sel) == 0 {
		// nothing ticked: every row of every table read into the result
		var stars []string
		for _, n := range g.tr.order {
			if r := n.role(); r == "main" || r == "cross" || r == "joined" {
				stars = append(stars, n.alias+".*")
			}
		}
		switch {
		case g.grouped:
			stars = []string{"count(*) AS count"}
		case len(stars) == 1:
			stars = []string{"*"}
		case len(stars) == 0:
			stars = []string{"1"}
		}
		sel = stars
	}
	return sel, groupBy
}

// orderBy writes the sorts. A sort the statement cannot express is reported:
// in a grouped statement a plain column must be one it groups by, and
// under DISTINCT (on Postgres, and by the standard) a sort must be on an
// output column — otherwise the order would be of rows that no longer
// exist once grouped or deduplicated.
func (g *gen) orderBy(groupBy []string) string {
	var keys []string
	outs := map[string]bool{}
	for _, o := range g.d.Columns {
		outs[g.expr(o.Ref, "", -1)] = true
	}
	for i, s := range g.d.Sorts {
		x := g.expr(s.Ref, "sort", i)
		if g.grouped && s.Agg == AggNone && x != "NULL" && !slices.Contains(groupBy, x) {
			g.report("sort", i, "sorting by "+x+", which is not grouped",
				"the rows are grouped, so "+x+" has many values per group. Show it as a column "+
					"(which groups by it too), or sort by an aggregate of it.")
		}
		if g.d.Distinct && !outs[x] && x != "NULL" {
			g.report("sort", i, "sorting by "+x+", which is not shown",
				"with DISTINCT, rows equal in every shown column are one row, and "+x+
					" may differ between them: there is no single value to sort that row by. Show it as a column.")
		}
		if s.Desc {
			x += " DESC"
		}
		keys = append(keys, x)
	}
	return strings.Join(keys, ", ")
}

// existsCond writes a tested child as [NOT] EXISTS in WHERE.
func (g *gen) existsCond(n *node) string {
	conds := append(g.on(n), g.conds(place{kind: "sub", n: n})...)
	kw := "EXISTS"
	if n.via.Kind == JoinNotExists {
		kw = "NOT EXISTS"
	}
	return kw + " (\n    SELECT 1 FROM " + g.tableRef(n) + " " + n.alias +
		"\n    WHERE " + strings.Join(conds, "\n      AND ") + "\n)"
}

// summarySelect writes a summarized child's derived table: its join
// columns, the aggregates asked of it, its own filters, grouped by the join
// columns.
func (g *gen) summarySelect(n *node) string {
	_, keys := edgeCols(n)
	s := g.sums[n]
	cols := make([]string, 0, len(keys)+len(s.items))
	var group []string
	for _, k := range keys {
		cols = append(cols, g.col(n, k))
		group = append(group, g.col(n, k))
	}
	if len(s.items) == 0 {
		s.need(AggCount, "") // nothing asked yet: the count, so the derived table says something
	}
	for _, it := range s.items {
		x := "*"
		if it.col != "" {
			x = g.col(n, it.col)
		}
		cols = append(cols, aggCall(it.agg, x)+" AS "+it.name)
	}
	q := "SELECT " + strings.Join(cols, ", ") + "\nFROM " + g.tableRef(n) + " " + n.alias
	if w := g.conds(place{kind: "sub", n: n}); len(w) > 0 {
		q += "\nWHERE " + strings.Join(w, "\n  AND ")
	}
	if len(group) > 0 {
		q += "\nGROUP BY " + strings.Join(group, ", ")
	}
	return q
}

// indent prefixes every line of s.
func indent(s, by string) string {
	return by + strings.ReplaceAll(s, "\n", "\n"+by)
}
