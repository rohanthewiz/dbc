package designer

import (
	"strconv"
	"strings"
)

// The story: the query read back in plain words, step by step, as the user
// builds it. It is how the designer explains rather than only warns —
// "from here on there is a row per order", "tested with EXISTS: each
// customer once", "written as a range so an index can serve it" — and it is
// written from the same tree the SQL is, so it cannot describe a different
// query from the one that runs.
//
// Each line is plain text with `code` spans in backquotes, which the page
// draws as code (as text nodes — never as markup).

var kindWords = map[JoinKind]string{
	JoinInner: "inner join", JoinLeft: "left join", JoinFull: "full join",
	JoinExists: "has any", JoinNotExists: "has none", JoinSummary: "summarize",
}

var opWords = map[string]string{
	OpEq: "is", OpNe: "is not", OpLt: "is less than", OpLe: "is at most", OpGt: "is more than", OpGe: "is at least",
	OpBetween: "is between", OpRange: "is from", OpIn: "is one of", OpNotIn: "is none of",
	OpNull: "is empty", OpNotNull: "is not empty", OpContains: "contains", OpStarts: "starts with", OpEnds: "ends with",
	OpYear: "is in", OpOn: "is on", OpLastDays: "is in the last",
}

var aggWords = map[Agg]string{
	AggCount: "the number of", AggCountDistinct: "the number of different", AggSum: "the total of",
	AggAvg: "the average of", AggMin: "the smallest", AggMax: "the largest",
}

// roughCount is a row count as a person says it: 940, 12K, 1.2M.
func roughCount(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 10_000:
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "K"
	case n < 1_000_000:
		return strconv.FormatInt((n+500)/1000, 10) + "K"
	case n < 10_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n < 1_000_000_000:
		return strconv.FormatInt((n+500_000)/1_000_000, 10) + "M"
	}
	return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + "B"
}

// teller writes the story.
type teller struct {
	d   *Design
	tr  *tree
	g   *gen
	cat *Catalog
}

func code(s string) string { return "`" + s + "`" }

// refWords names a ref: `o.total`, the total of `o.total`, the number of `o` rows.
func (t *teller) refWords(r Ref) string {
	n := t.tr.nodes[r.Table]
	if r.Table == "" || n == nil {
		if r.Agg == AggCount {
			return "the number of rows"
		}
		return "?"
	}
	if r.Column == "" {
		return "the number of " + code(n.alias) + " rows"
	}
	c := code(n.alias + "." + r.Column)
	if r.Agg == AggNone {
		return c
	}
	return aggWords[r.Agg] + " " + c
}

// filterWords describes one filter, with a note on how it is written when
// that is part of the point (a range, an escaped pattern).
func (t *teller) filterWords(f *Filter) string {
	w := t.refWords(f.Ref) + " " + opWords[f.Op]
	v := func(i int) string {
		if i < len(f.Values) {
			return code(f.Values[i])
		}
		return "…"
	}
	switch f.Op {
	case OpNull, OpNotNull:
	case OpBetween:
		w += " " + v(0) + " and " + v(1)
	case OpRange:
		w += " " + v(0) + " up to (not including) " + v(1)
	case OpIn, OpNotIn:
		parts := make([]string, len(f.Values))
		for i := range f.Values {
			parts[i] = v(i)
		}
		w += " " + strings.Join(parts, ", ")
	case OpYear:
		w += " " + v(0) + " (written as a range, so an index on the column can serve it)"
	case OpOn:
		w += " " + v(0) + " (all of that day, as a range)"
	case OpLastDays:
		w += " " + v(0) + " days"
	default:
		w += " " + v(0)
	}
	if f.OrNull {
		w += ", or is empty"
	}
	return w
}

// condsWords lists the filters at a place: "a; and b; and (c, or d)".
func (t *teller) condsWords(p place) string {
	var out []string
	groups := map[string]int{}
	for i := range t.d.Filters {
		f := &t.d.Filters[i]
		if t.g.placeOf(f) != p {
			continue
		}
		w := t.filterWords(f)
		if f.Group == "" {
			out = append(out, w)
			continue
		}
		if at, ok := groups[f.Group]; ok {
			out[at] += ", or " + w
			continue
		}
		groups[f.Group] = len(out)
		out = append(out, "any of: "+w)
	}
	return strings.Join(out, "; and ")
}

// tell writes the story.
func (t *teller) tell() []string {
	if len(t.tr.order) == 0 {
		return []string{"Drop a table here to start: drag one from the list, or double-click it."}
	}
	var out []string
	add := func(s string) { out = append(out, s) }

	for _, n := range t.tr.order {
		switch n.role() {
		case "main":
			s := "Start from " + code(n.t.Name)
			if n.alias != n.t.Name {
				s += " (" + code(n.alias) + ")"
			}
			if r, ok := t.cat.rows(n.et); ok {
				s += ", about " + roughCount(r) + " rows"
			}
			add(s + ".")
		case "cross":
			add("Pair every row so far with every row of " + code(n.t.Name) + " (" + code(n.alias) +
				") — nothing joins it, so the row counts multiply.")
		case "exists":
			s := "Keep only " + code(n.parent.alias) + " rows that have at least one " + code(n.t.Name) + " (" + code(n.alias) + ")"
			if w := t.condsWords(place{kind: "sub", n: n}); w != "" {
				s += " where " + w
			}
			add(s + " — tested with EXISTS, so each " + code(n.parent.alias) + " row stays once, however many match.")
		case "notexists":
			s := "Keep only " + code(n.parent.alias) + " rows with no " + code(n.t.Name) + " (" + code(n.alias) + ")"
			if w := t.condsWords(place{kind: "sub", n: n}); w != "" {
				s += " where " + w
			}
			add(s + " — NOT EXISTS, which (unlike NOT IN) a NULL cannot empty.")
		case "summary":
			var aggs []string
			counted := false
			for _, it := range t.g.sums[n].items {
				r := Ref{Table: n.t.ID, Column: it.col, Agg: it.agg}
				aggs = append(aggs, t.refWords(r))
				counted = counted || it.agg == AggCount
			}
			s := "Summarize " + code(n.t.Name) + " (" + code(n.alias) + ") per " + code(n.parent.alias) + " first"
			if w := t.condsWords(place{kind: "sub", n: n}); w != "" {
				s += ", counting only those where " + w
			}
			s += " — " + strings.Join(aggs, ", ") + " — and attach it as one row per " + code(n.parent.alias) + ", so nothing is repeated"
			if counted {
				s += " (one with none counts 0)"
			}
			add(s + ".")
		default:
			add(t.joinWords(n))
		}
	}

	if w := t.condsWords(place{kind: "where"}); w != "" {
		add("Keep the rows where " + w + ".")
	}
	if t.g.grouped {
		var keys, aggs []string
		for _, o := range t.d.Columns {
			if o.Agg == AggNone {
				keys = append(keys, code(t.g.expr(o.Ref, "", -1)))
			} else {
				aggs = append(aggs, t.refWords(o.Ref))
			}
		}
		comp := ""
		if len(aggs) > 0 {
			comp = ", and compute " + strings.Join(aggs, ", ") + " in each"
		}
		if len(keys) == 0 {
			add("Then collapse the rows into one" + comp + ".")
		} else {
			add("Then group the rows by " + strings.Join(keys, ", ") + " — one result row per different value" + comp + ".")
		}
		for _, o := range t.d.Columns {
			if o.Agg == AggCount && o.Column == "" && o.Table != "" {
				if n := t.tr.nodes[o.Table]; n != nil && strings.HasPrefix(t.g.countRows(n), "count(DISTINCT") {
					add("The number of " + code(n.alias) + " rows counts each once (count DISTINCT on its key), " +
						"though the joins repeat them.")
				}
			}
		}
	} else if g := t.grain(); g != "" {
		add(g)
	}
	if w := t.condsWords(place{kind: "having"}); w != "" {
		add("Keep the groups where " + w + ".")
	}
	if t.d.Distinct {
		add("Remove rows that are equal in every column.")
	}
	if len(t.d.Sorts) > 0 {
		var keys []string
		for _, s := range t.d.Sorts {
			k := t.refWords(s.Ref)
			if s.Desc {
				k += " (highest first)"
			}
			keys = append(keys, k)
		}
		add("Sort by " + strings.Join(keys, ", then ") + ".")
	}
	switch {
	case t.d.Limit > 0 && t.d.Offset > 0:
		add("Skip the first " + strconv.Itoa(t.d.Offset) + " rows and return the next " + strconv.Itoa(t.d.Limit) + ".")
	case t.d.Limit > 0:
		add("Return the first " + strconv.Itoa(t.d.Limit) + ".")
	case t.d.Offset > 0:
		add("Skip the first " + strconv.Itoa(t.d.Offset) + " rows.")
	}
	return out
}

// joinWords tells one ordinary join: what it adds, how many per row, and
// what happens to the rows without a match.
func (t *teller) joinWords(n *node) string {
	p := n.parent
	what := code(n.t.Name)
	if n.alias != n.t.Name {
		what += " (" + code(n.alias) + ")"
	}
	fans, known := t.tr.fansOut(n)
	kind := n.via.Kind
	if kind == JoinLeft && !n.fromLeft {
		return "Join " + what + ", keeping every " + code(n.alias) + " row whether or not it matches the rows so far (a right join)."
	}
	if kind == JoinFull {
		return "Match " + what + " with the rows so far, keeping the unmatched rows of both sides (their other columns empty)."
	}
	var s string
	switch {
	case !fans:
		s = "Look up each " + code(p.alias) + " row's " + what + " — at most one each"
	case known:
		s = "Join each " + code(p.alias) + " row to its " + what + " — possibly many each, so from here on there is a row per " + code(n.alias)
	default:
		s = "Join " + what + " — " + code(n.alias) + " has no key to tell by, so each " + code(p.alias) + " row may match several"
	}
	if kind == JoinLeft {
		return s + "; " + code(p.alias) + " rows with none are kept, with " + code(n.alias) + "'s columns empty."
	}
	if !fans && t.guaranteed(n) {
		return s + " (a foreign key guarantees exactly one)."
	}
	return s + "; " + code(p.alias) + " rows with none are left out."
}

// guaranteed reports whether an inner to-one join always finds its row: the
// join follows a foreign key from the parent, whose columns are NOT NULL.
func (t *teller) guaranteed(n *node) bool {
	if n.via.FK == "" || n.parent.et == nil {
		return false
	}
	pCols, _ := edgeCols(n)
	for _, c := range pCols {
		col := n.parent.et.Col(c)
		if col == nil || col.Nullable || !col.FK {
			return false
		}
	}
	return true
}

// grain says what one result row is, for an ungrouped query: the one table
// whose rows no join repeats (the deepest of a chain of one-to-many joins).
func (t *teller) grain() string {
	var grains []*node
	for _, n := range t.tr.order {
		if r := n.role(); (r == "main" || r == "joined") && n.comp == 0 && len(t.tr.multipliers(n)) == 0 {
			grains = append(grains, n)
		}
	}
	if len(t.tr.order) < 2 {
		return ""
	}
	if len(grains) == 0 {
		return "Each result row is a combination of rows — no one table's rows appear once each."
	}
	// the deepest is the finest grain: the others are looked up from it
	g := grains[0]
	for _, x := range grains {
		if len(x.path()) > len(g.path()) {
			g = x
		}
	}
	return "Each result row is one " + code(g.t.Name) + " row."
}
