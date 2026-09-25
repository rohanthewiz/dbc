package explain

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// The insight rules. Each looks at one step (or the plan as a whole) for a
// pattern that experienced DBAs recognize on sight — a full scan that keeps a
// handful of rows, a sort that spilled to disk, a planner estimate off by
// ×50 — and says in plain words what it is, why it costs, and what to try.
//
// DESIGN RULES FOR A RULE:
//
//   - EVIDENCE OR SILENCE. A rule fires only on numbers the engine reported.
//     SQLite reports no row counts, so the "keeps 10 of 200,000 rows" rule
//     cannot fire there, and nothing pretends it did; a weaker rule (a full
//     scan of a table known to be big) speaks instead.
//   - SIZE MATTERS. A full scan of a 40-row table is the right plan; the
//     thresholds below (bigScan, bigRows) keep small tables out of the list,
//     so what is listed is worth reading.
//   - SAY IT ONCE. A misestimate cascades up the tree — every join above a
//     bad estimate inherits it — so only the step where it starts is named.
//   - A FIX IS A SUGGESTION. Rules that can name a concrete statement (a
//     CREATE INDEX over the filtered columns) put it in Insight.SQL, for the
//     user to read, copy or insert. Nothing is run for them.

const (
	bigScan    = 10_000 // rows: below this a full scan is not worth a word
	bigRows    = 1_000  // rows: below this a misestimate or a loop is noise
	hugeScan   = 100_000
	misestimOf = 10.0 // ×: estimates are rarely better than 2–3×; 10× changes plans
)

// analyze runs every rule over the plan and orders the findings: the most
// severe first, then by how much of the plan the step accounts for.
func analyze(p *Plan) []Insight {
	var out []Insight
	add := func(in Insight) { out = append(out, in) }
	m := p.Metric
	if len(p.Metrics()) > 0 {
		m = p.Metrics()[0]
	}

	misestimates := 0
	for _, n := range p.nodes {
		ruleFullScan(p, n, add)
		ruleIndexWaste(p, n, add)
		if misestimates < 3 && ruleMisestimate(p, n, add) {
			misestimates++
		}
		ruleSpill(p, n, add)
		ruleCorrelated(p, n, add)
		ruleAutoIndex(p, n, add)
		ruleTempSort(p, n, add)
		ruleMySQLIndexUnused(p, n, add)
		ruleHeapFetches(p, n, add)
		ruleWorkers(n, add)
	}
	ruleNeverExecuted(p, add)
	ruleHotspot(p, out, add)
	rulePlanning(p, add)

	rank := map[Severity]int{SevCrit: 0, SevWarn: 1, SevInfo: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Severity] != rank[out[j].Severity] {
			return rank[out[i].Severity] < rank[out[j].Severity]
		}
		return nodeShare(p, out[i].NodeID, m) > nodeShare(p, out[j].NodeID, m)
	})

	serious := 0
	for _, in := range out {
		if in.Severity != SevInfo {
			serious++
		}
	}
	if serious == 0 {
		// first, since it is the headline: the notes after it are context
		out = append([]Insight{allClear(p, len(out))}, out...)
	}
	return out
}

func nodeShare(p *Plan, id int, m Metric) float64 {
	if n := p.Node(id); n != nil {
		return p.Share(n, m)
	}
	return 0
}

// ---------------------------------------------------------------------------
// The rules
// ---------------------------------------------------------------------------

// ruleFullScan: a step that reads a whole table. Worth a word only when the
// table is big, and worth a warning when most of what it reads is thrown
// away — the classic missing index — or when it runs over and over.
func ruleFullScan(p *Plan, n *Node, add func(Insight)) {
	if n.Kind != KindScan || n.NeverExecuted {
		return
	}
	table := tableOf(n)
	cond, removedPerLoop, keptPerLoop := scanFilter(n)
	loops := math.Max(n.Loops, 1)
	examined := (keptPerLoop + removedPerLoop) * loops
	// Under a Gather, Loops counts the parallel workers that shared ONE
	// pass over the table; only the rest are real repeats.
	runs := loops / math.Max(n.Participants, 1)
	size := math.Max(examined, n.TableRows)
	if !p.Analyzed && n.HasEst {
		// an estimate-only plan knows the rows the step returns, not how many
		// it read; that is a lower bound on the table
		size = math.Max(size, n.EstRows)
	}
	cols := indexColumns(cond, n)
	// the suggestion names the real table or nothing: an unresolved alias
	// ("ON o") would be a statement that fails
	fix, sql := indexFix(p, n.Relation, cols)

	switch {
	case p.Analyzed && runs > 1.5 && examined >= bigRows:
		add(Insight{
			Severity: SevCrit, NodeID: n.ID,
			Title: fmt.Sprintf("Full scan of %s runs %s times", table, FmtCount(runs)),
			Detail: fmt.Sprintf("It sits inside a loop — the inner side of a nested-loop join, or a subquery run per row — "+
				"so the whole table is read again for every outer row: %s rows read in total.", FmtCount(examined)),
			Fix: orStr(fix, "Index the column "+table+" is matched on, so each loop becomes a lookup."), SQL: sql,
		})
	case cond != "" && p.Analyzed && examined >= bigScan && keptPerLoop*loops <= examined*0.1:
		kept := keptPerLoop * loops
		add(Insight{
			Severity: SevWarn, NodeID: n.ID,
			Title: fmt.Sprintf("Full scan of %s keeps %s of %s rows", table, FmtCount(kept), FmtCount(examined)),
			Detail: fmt.Sprintf("Every row is read and tested against %s; %s survive. "+
				"An index lets the database go straight to the rows that match.", cond, fmtPct(kept, examined)),
			Fix: orStr(fix, "Add an index over the filtered columns."), SQL: sql,
		})
	case !p.Analyzed && perOuterRow(n) && size >= bigRows:
		add(Insight{
			Severity: SevCrit, NodeID: n.ID,
			Title: fmt.Sprintf("Full scan of %s inside a correlated subquery", table),
			Detail: fmt.Sprintf("The subquery runs once per row of the outer query, and each run reads all ~%s rows of %s.",
				FmtCount(size), table),
			Fix: orStr(fix, "Index the column the subquery matches "+table+" on, so each run is a lookup — or rewrite it as a JOIN."),
			SQL: sql,
		})
	case !p.Analyzed && n.FilteredPct > 0 && n.FilteredPct <= 20 && size >= bigScan:
		// MySQL's table format has no condition text, but its "filtered"
		// column is the optimizer's own estimate of what the WHERE keeps
		add(Insight{
			Severity: SevWarn, NodeID: n.ID,
			Title: fmt.Sprintf("Full scan of %s keeps an estimated %.0f%% of ~%s rows", table, n.FilteredPct,
				FmtCount(size)),
			Detail: "Every row is read and tested against the WHERE clause, and the optimizer expects most to fail it. " +
				"An index on the tested columns would read only the rows that match.",
			Fix: "Index the columns the WHERE clause tests on " + table + ".",
		})
	case cond != "" && !p.Analyzed && (size >= bigScan || p.Share(n, MetricCost) >= 0.3):
		add(Insight{
			Severity: SevWarn, NodeID: n.ID,
			Title: fmt.Sprintf("Full scan of %s, filtered by %s", table, cond),
			Detail: "The planner expects to read the whole table and test every row. If the filter is selective, " +
				"an index would read far less — run with ANALYZE to see how many rows it throws away.",
			Fix: orStr(fix, "Add an index over the filtered columns."), SQL: sql,
		})
	case size >= hugeScan:
		add(Insight{
			Severity: SevInfo, NodeID: n.ID,
			Title: fmt.Sprintf("Reads all %s rows of %s", FmtCount(size), table),
			Detail: "A full scan is the right plan when the query needs most of the table (an unfiltered aggregate, " +
				"an export). If it does not, a WHERE clause an index can serve would read far less.",
		})
	case p.Engine == SQLite || p.Engine == Bytdb:
		if n.TableRows >= bigScan {
			add(Insight{
				Severity: SevWarn, NodeID: n.ID,
				Title: fmt.Sprintf("Full scan of %s (%s rows)", table, FmtCount(n.TableRows)),
				Detail: "Every row of the table is visited. If the query filters or joins on " + table +
					", an index on those columns turns the scan into a search.",
				Fix: "Index the columns the query filters or joins " + table + " by.",
			})
		}
	}
}

// perOuterRow reports whether a step sits inside a subquery that runs once
// per outer row — which multiplies whatever the step costs.
func perOuterRow(n *Node) bool {
	for a := n; a != nil; a = a.parent {
		if a.HasFlag("dependent") || a.HasFlag("correlated") || strings.HasPrefix(a.Relationship, "SubPlan") {
			return true
		}
	}
	return false
}

// scanFilter finds the condition a scan applies and the rows it keeps and
// drops per loop. Postgres puts the filter on the scan itself; MySQL's tree
// puts it in a separate Filter step directly above, so the rows it dropped
// are the scan's output less the filter's.
func scanFilter(n *Node) (cond string, removed, kept float64) {
	cond, _ = n.Prop("Filter")
	removed, kept = n.RowsRemoved, n.ActualRows
	if par := n.parent; par != nil && par.Kind == KindFilter && len(par.Children) == 1 {
		if f, ok := par.Prop("Filter"); ok {
			cond = f
			if n.HasActual && par.HasActual {
				// the filter runs as many times as the scan; per-loop counts compare
				kept = par.ActualRows
				removed = math.Max(n.ActualRows-par.ActualRows, 0)
			}
		}
	}
	return cond, removed, kept
}

// ruleIndexWaste: an index scan that fetches rows and then discards most of
// them — the index narrows on one column, but the query also filters on
// another it does not cover.
func ruleIndexWaste(p *Plan, n *Node, add func(Insight)) {
	if n.Kind != KindIndex || !n.HasActual || n.RowsRemoved <= 0 {
		return
	}
	loops := math.Max(n.Loops, 1)
	removed := n.RowsRemoved * loops
	fetched := removed + n.ActualRows*loops
	// Per loop, too: a primary-key lookup that fetches its one row and then
	// filters it out 4,000 times over is a join doing its job, not an index
	// fetching too much — there is nothing for a wider index to save.
	if removed < bigRows || removed < fetched*0.8 || n.RowsRemoved < 10 {
		return
	}
	cond, _ := n.Prop("Filter")
	table := tableOf(n)
	fix := "Add the filtered column to the index (as a later key column), so rows are excluded before they are fetched."
	sql := ""
	if cols := indexColumns(cond, n); len(cols) > 0 && table != "" {
		lead := indexColumns(firstProp(n, "Index Cond", "Recheck Cond", "Lookup", "Range"), n)
		f, s := indexFix(p, n.Relation, dedupe(append(lead, cols...)))
		fix, sql = orStr(f, fix), s
	}
	add(Insight{
		Severity: SevWarn, NodeID: n.ID,
		Title:  fmt.Sprintf("%s fetches %s rows, then discards %.0f%%", n.Title(), FmtCount(fetched), pct(removed, fetched)),
		Detail: fmt.Sprintf("The index finds candidates, but the filter %s rejects most of them after each row was read.", orStr(cond, "applied afterwards")),
		Fix:    fix, SQL: sql,
	})
}

// ruleMisestimate: the planner's row estimate is off by ×10 or more at the
// step where the error starts. Bad estimates are the root of most bad plans —
// a join method or order chosen for 100 rows and run on 100,000.
func ruleMisestimate(p *Plan, n *Node, add func(Insight)) bool {
	if !misestimated(n) {
		return false
	}
	for _, c := range n.Children {
		if misestimated(c) {
			return false // inherited from below: the child is where it starts
		}
	}
	f := n.Misestimate
	table := tableOf(n)
	fix, sql := "Refresh the planner's statistics.", ""
	switch p.Engine {
	case Postgres:
		if n.Relation != "" {
			sql = "ANALYZE " + n.Relation + ";"
			fix = "Refresh statistics with ANALYZE. If the estimate stays off, the filter may combine correlated " +
				"columns (CREATE STATISTICS) or skewed values (ALTER TABLE … ALTER COLUMN … SET STATISTICS 1000)."
		}
	case MySQL:
		if n.Relation != "" {
			sql = "ANALYZE TABLE " + n.Relation + ";"
			fix = "Refresh statistics with ANALYZE TABLE; for skewed columns, a histogram " +
				"(ANALYZE TABLE … UPDATE HISTOGRAM ON col) helps the optimizer most."
		}
	}
	where := ""
	if table != "the table" {
		where = " from " + table
	}
	add(Insight{
		Severity: SevWarn, NodeID: n.ID,
		Title: fmt.Sprintf("Planner expected %s rows%s, got %s (%s)", FmtCount(n.EstRows), where,
			FmtCount(n.ActualRows), FmtFactor(f)),
		Detail: "Every choice above this step — join method, join order, memory for hashing and sorting — was made " +
			"for the wrong number of rows. Fixing the estimate is often what fixes the plan.",
		Fix: fix, SQL: sql,
	})
	return true
}

func misestimated(n *Node) bool {
	if n.Misestimate == 0 || n.NeverExecuted {
		return false
	}
	loops := math.Max(n.Loops, 1)
	big := math.Max(n.ActualRows, n.EstRows)*loops >= bigRows
	return big && (n.Misestimate >= misestimOf || n.Misestimate <= 1/misestimOf)
}

// ruleSpill: a sort or hash that ran out of working memory and wrote to disk.
func ruleSpill(p *Plan, n *Node, add func(Insight)) {
	spillSQL := func() string {
		if p.Engine != Postgres {
			return ""
		}
		// enough for the spilled data twice over (in-memory sorts need more
		// room than the on-disk runs they replace), in a power of two
		mb := math.Max(math.Pow(2, math.Ceil(math.Log2(math.Max(n.SpillKB*2/1024, 8)))), 8)
		return fmt.Sprintf("SET work_mem = '%.0fMB';", mb)
	}
	switch {
	case n.SortSpill:
		size := ""
		if n.SpillKB > 0 {
			size = " " + FmtKB(n.SpillKB)
		}
		key, _ := n.Prop("Sort Key")
		add(Insight{
			Severity: SevCrit, NodeID: n.ID,
			Title: "Sort spilled" + size + " to disk",
			Detail: "The rows to sort did not fit in work_mem, so they were sorted in runs on disk and merged — " +
				"many times slower than an in-memory sort.",
			Fix: "Raise work_mem for this session (or this query), or add an index on the sort key" +
				parens(key) + " so rows come out already in order.",
			SQL: spillSQL(),
		})
	case n.Batches > 1:
		add(Insight{
			Severity: SevWarn, NodeID: n.ID,
			Title: fmt.Sprintf("%s spilled to disk in %s batches", n.Op, FmtCount(n.Batches)),
			Detail: "The hash table did not fit in memory, so its input was split into batches written to temp " +
				"files and processed one at a time.",
			Fix: "Raise work_mem (hash_mem_multiplier on Postgres 13+) so the hash fits in one batch.",
			SQL: spillSQL(),
		})
	}
}

// ruleCorrelated: a subquery that runs once for every row of the outer query.
func ruleCorrelated(p *Plan, n *Node, add func(Insight)) {
	per := n.HasFlag("correlated") || n.HasFlag("dependent")
	loops := 0.0
	if strings.HasPrefix(n.Relationship, "SubPlan") {
		per = true
		loops = n.Loops
	}
	if !per {
		return
	}
	if n.Kind == KindSubquery && len(n.Children) > 0 && loops == 0 {
		loops = n.Children[0].Loops
	}
	times := "once per outer row"
	if loops > 1 {
		times = fmt.Sprintf("%s times — once per outer row", FmtCount(loops))
	}
	sev := SevWarn
	if p.Analyzed && loops > 0 && loops < 100 {
		sev = SevInfo // a handful of runs is cheap; worth knowing, not worrying
	}
	add(Insight{
		Severity: sev, NodeID: n.ID,
		Title:  "Subquery runs " + times,
		Detail: "It refers to a column of the outer query, so it cannot be computed once and reused.",
		Fix: "Rewrite it as a JOIN to a grouped derived table (or LEFT JOIN LATERAL on Postgres), so it runs once; " +
			"or index the column it correlates on, so each run is a lookup.",
	})
}

// ruleAutoIndex: the engine builds a throwaway index at run time because no
// real one exists — SQLite's automatic index, MySQL's <auto_key>.
func ruleAutoIndex(p *Plan, n *Node, add func(Insight)) {
	if !n.HasFlag("auto-index") {
		return
	}
	table := tableOf(n)
	cols := indexColumns(firstProp(n, "Constraint", "Lookup"), n)
	fix, sql := indexFix(p, n.Relation, cols)
	what := "a temporary index"
	if p.Engine == SQLite {
		what = "an automatic index"
	}
	add(Insight{
		Severity: SevWarn, NodeID: n.ID,
		Title: fmt.Sprintf("Builds %s on %s at run time", what, table),
		Detail: "No suitable index exists, so the engine builds one for this query, uses it, and throws it away — " +
			"paying for the build on every run.",
		Fix: orStr(fix, "Create a real index on the columns it looks up."), SQL: sql,
	})
}

// ruleTempSort: rows sorted or grouped through a temporary structure — fine
// for small results, worth an index when it is the expensive part.
func ruleTempSort(p *Plan, n *Node, add func(Insight)) {
	rows := n.RowsOut
	if n.Kind == KindSort && !n.SortSpill && p.Analyzed && p.Share(n, MetricTime) >= 0.3 {
		by := ""
		if key, ok := n.Prop("Sort Key"); ok {
			by = " by " + key
		}
		add(Insight{
			Severity: SevInfo, NodeID: n.ID,
			Title:  fmt.Sprintf("Sorting takes %.0f%% of the time", p.Share(n, MetricTime)*100),
			Detail: fmt.Sprintf("%s rows are sorted%s.", FmtCount(rows), by),
			Fix:    "An index on the sort key can return rows already in order, and with a LIMIT stop early.",
		})
		return
	}
	switch {
	case n.HasFlag("temp-btree"):
		purpose, _ := n.Prop("Purpose")
		sev, detail := SevInfo, "SQLite collects the rows into a temporary B-tree to "+strings.ToLower(purpose)+"."
		if strings.Contains(purpose, "ORDER BY") {
			detail += " An index whose columns match the ORDER BY would deliver rows already in order."
		}
		add(Insight{Severity: sev, NodeID: n.ID, Title: "Uses a temporary B-tree for " + purpose, Detail: detail})
	case n.HasFlag("filesort") && n.EstRows >= bigScan:
		add(Insight{
			Severity: SevInfo, NodeID: n.ID,
			Title:  fmt.Sprintf("Sorts ~%s rows of %s (filesort)", FmtCount(n.EstRows), tableOf(n)),
			Detail: "MySQL sorts the rows after reading them. An index on the ORDER BY columns avoids the sort.",
		})
	}
}

// ruleMySQLIndexUnused: MySQL listed a usable index and then did not use it.
func ruleMySQLIndexUnused(p *Plan, n *Node, add func(Insight)) {
	if !n.HasFlag("index-unused") || n.Kind != KindScan {
		return
	}
	keys, _ := n.Prop("Possible keys")
	sql := ""
	if n.Relation != "" {
		sql = "ANALYZE TABLE " + n.Relation + ";"
	}
	add(Insight{
		Severity: SevWarn, NodeID: n.ID,
		Title: fmt.Sprintf("MySQL could use %s on %s, but scans the table instead", keys, tableOf(n)),
		Detail: "The optimizer judged the index not selective enough — or its statistics are stale, or the " +
			"condition wraps the column in a function or compares mismatched types, which an index cannot serve.",
		Fix: "Refresh statistics with ANALYZE TABLE; check the WHERE compares the bare column to a value of its own type.",
		SQL: sql,
	})
}

// ruleHeapFetches: an index-only scan that still had to visit the table.
func ruleHeapFetches(p *Plan, n *Node, add func(Insight)) {
	if !strings.Contains(n.Op, "Index Only Scan") {
		return
	}
	v, ok := n.Prop("Heap Fetches")
	if !ok {
		return
	}
	hf := leadingFloat(strings.ReplaceAll(v, ",", ""))
	rows := n.RowsOut
	if hf < bigRows || hf < rows*0.3 {
		return
	}
	sql := ""
	if n.Relation != "" {
		sql = "VACUUM " + n.Relation + ";"
	}
	add(Insight{
		Severity: SevInfo, NodeID: n.ID,
		Title:  fmt.Sprintf("Index-only scan still read the table %s times", FmtCount(hf)),
		Detail: "Rows changed since the last VACUUM are not known to be visible from the index alone, so each was checked in the table.",
		Fix:    "VACUUM the table to update its visibility map.", SQL: sql,
	})
}

// ruleWorkers: fewer parallel workers started than were planned.
func ruleWorkers(n *Node, add func(Insight)) {
	if n.WorkersPlanned <= 0 || n.WorkersLaunched >= n.WorkersPlanned {
		return
	}
	add(Insight{
		Severity: SevInfo, NodeID: n.ID,
		Title:  fmt.Sprintf("Only %s of %s parallel workers started", FmtCount(n.WorkersLaunched), FmtCount(n.WorkersPlanned)),
		Detail: "The server had no free background workers when the query ran, so it did more of the work serially than planned.",
		Fix:    "Check max_parallel_workers and max_worker_processes against the load.",
	})
}

// ruleNeverExecuted: a branch the executor never needed. Named once, at its
// top, since everything under it did not run either.
func ruleNeverExecuted(p *Plan, add func(Insight)) {
	for _, n := range p.nodes {
		if n.NeverExecuted && (n.parent == nil || !n.parent.NeverExecuted) {
			add(Insight{
				Severity: SevInfo, NodeID: n.ID,
				Title: n.Title() + " never ran",
				Detail: "The step above it finished without needing its rows (an empty input, a satisfied LIMIT), " +
					"so it cost nothing this time. With other data or parameters it may run, and cost.",
			})
		}
	}
}

// ruleHotspot: where most of the measured time went, when that is not
// already the subject of a finding.
func ruleHotspot(p *Plan, found []Insight, add func(Insight)) {
	if !p.Analyzed || p.Root.Inclusive(MetricTime) < 1 {
		return
	}
	var top *Node
	for _, n := range p.nodes {
		if top == nil || n.SelfMs > top.SelfMs {
			top = n
		}
	}
	share := p.Share(top, MetricTime)
	if top == nil || share < 0.4 {
		return
	}
	for _, in := range found {
		if in.NodeID == top.ID {
			return
		}
	}
	add(Insight{
		Severity: SevInfo, NodeID: top.ID,
		Title: fmt.Sprintf("%.0f%% of the time is spent in %s", share*100, top.Title()),
		Detail: fmt.Sprintf("%s of %s. Speeding up any other step cannot save more than the remaining %.0f%%.",
			FmtMs(top.SelfMs), FmtMs(p.Root.Inclusive(MetricTime)), (1-share)*100),
	})
}

// rulePlanning: planning cost more than running — a short query planned
// afresh on every call.
func rulePlanning(p *Plan, add func(Insight)) {
	if p.PlanningMs < 1 || p.ExecutionMs <= 0 || p.PlanningMs <= p.ExecutionMs {
		return
	}
	add(Insight{
		Severity: SevInfo, NodeID: -1,
		Title:  fmt.Sprintf("Planning (%s) took longer than running (%s)", FmtMs(p.PlanningMs), FmtMs(p.ExecutionMs)),
		Detail: "For a statement run often, a prepared statement plans once and reuses the plan.",
	})
}

// allClear is the finding when there are no others worth a warning — said
// out loud, so an empty list is not mistaken for a broken feature.
func allClear(p *Plan, notes int) Insight {
	scans, indexed := 0, 0
	for _, n := range p.nodes {
		switch n.Kind {
		case KindScan:
			scans++
		case KindIndex:
			indexed++
		}
	}
	detail := "No step matches a known problem pattern."
	switch {
	case notes > 0:
		detail = "No step matches a known problem pattern; the points below are context worth knowing."
	case scans == 0 && indexed > 0:
		detail = "Every table is reached through an index."
	case scans > 0:
		detail = "The full scans here are of small tables."
	}
	if !p.Analyzed && (p.Engine == Postgres || p.Engine == MySQL) {
		detail += " An analyzed run checks the estimates against what really happens."
	}
	return Insight{Severity: SevInfo, NodeID: -1, Title: "No problems found", Detail: detail}
}

// ---------------------------------------------------------------------------
// Index suggestions
// ---------------------------------------------------------------------------

// tableOf names a step's table for a sentence. A MySQL Filter step is
// about the table of the step it filters.
func tableOf(n *Node) string {
	if n.Kind == KindFilter && len(n.Children) == 1 && n.Relation == "" && n.Alias == "" {
		return tableOf(n.Children[0])
	}
	switch {
	case n.Relation != "":
		return n.Relation
	case n.Alias != "":
		return n.Alias
	}
	return "the table"
}

var (
	condStringRe = regexp.MustCompile(`'(?:[^']|'')*'`)
	condCastRe   = regexp.MustCompile(`::[\w ]+(?:\[\])?`)
	condParenRe  = regexp.MustCompile(`\(\s*((?:[A-Za-z_][\w$]*\.)*[A-Za-z_][\w$]*)\s*\)`)
	condIdent    = `((?:[A-Za-z_][\w$]*\.)*[A-Za-z_][\w$]*)`
	condOps      = `(=|<>|!=|<=|>=|<|>|~~\*?|!~~\*?|(?i:\bnot\s+like\b|\blike\b|\bilike\b|\bin\b|\bis\b|\bbetween\b))`
	condLeftRe   = regexp.MustCompile(condIdent + `\s*` + condOps)
	condRightRe  = regexp.MustCompile(`(=|<>|!=|<=|>=|<|>)\s*` + condIdent + `(?:\s|\)|$)`)
)

// condWords are identifiers that are not columns.
var condWords = map[string]bool{
	"null": true, "true": true, "false": true, "not": true, "and": true, "or": true,
	"any": true, "all": true, "array": true, "current_date": true, "current_timestamp": true, "now": true,
}

// indexColumns reads the columns of step n's table that a condition tests,
// equality tests first — the order a B-tree index should list them in, since
// an index serves equality on its leading columns and at most one range after
// them. Columns qualified with another table's alias (the other side of a
// join condition) are left out; unqualified ones are assumed to be this
// table's, which is how every engine prints a single-table filter.
func indexColumns(cond string, n *Node) []string {
	if cond == "" {
		return nil
	}
	c := strings.ReplaceAll(cond, "`", "")
	c = condStringRe.ReplaceAllString(c, "?")
	c = condCastRe.ReplaceAllString(c, "")
	// "((city)::text ~~ …)" is "(city ~~ …)" once the cast is gone: unwrap
	// a lone identifier's parentheses so the operator sits beside it
	for prev := ""; prev != c; {
		prev, c = c, condParenRe.ReplaceAllString(c, "$1")
	}
	var eq, rng []string
	take := func(ident, op string) {
		parts := strings.Split(ident, ".")
		col := parts[len(parts)-1]
		if condWords[strings.ToLower(col)] || strings.EqualFold(col, "rowid") {
			return
		}
		if len(parts) > 1 {
			q := parts[len(parts)-2]
			if q != n.Alias && q != n.Relation && !strings.HasSuffix(n.Relation, "."+q) {
				return
			}
		}
		switch strings.ToLower(strings.Join(strings.Fields(op), " ")) {
		case "=", "in", "is":
			eq = append(eq, col)
		default:
			rng = append(rng, col)
		}
	}
	for _, m := range condLeftRe.FindAllStringSubmatch(c, -1) {
		take(m[1], m[2])
	}
	for _, m := range condRightRe.FindAllStringSubmatch(c, -1) {
		take(m[2], m[1])
	}
	cols := dedupe(eq)
	for _, r := range dedupe(rng) {
		if !contains(cols, r) {
			cols = append(cols, r)
			break // one range column: a B-tree cannot use a second
		}
	}
	if len(cols) > 3 {
		cols = cols[:3]
	}
	return cols
}

// indexFix phrases an index suggestion and the statement that creates it.
// The index is named, since MySQL and SQLite require a name; the name follows
// the common idx_<table>_<columns> habit so it is recognizable later.
func indexFix(p *Plan, table string, cols []string) (fix, sql string) {
	if table == "" || table == "the table" || len(cols) == 0 || strings.HasPrefix(table, "<") {
		return "", ""
	}
	bare := table
	if i := strings.LastIndex(bare, "."); i >= 0 {
		bare = bare[i+1:]
	}
	name := "idx_" + bare + "_" + strings.Join(cols, "_")
	sql = fmt.Sprintf("CREATE INDEX %s ON %s (%s);", name, table, strings.Join(cols, ", "))
	fix = fmt.Sprintf("Index %s on (%s).", table, strings.Join(cols, ", "))
	if p.Engine == Postgres {
		fix += " On a busy table, CREATE INDEX CONCURRENTLY avoids blocking writes while it builds."
	}
	return fix, sql
}

func firstProp(n *Node, keys ...string) string {
	for _, k := range keys {
		if v, ok := n.Prop(k); ok {
			return v
		}
	}
	return ""
}

func dedupe(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// fmtPct is a/b as a percentage at a precision that does not round a
// telling sliver to nothing: 12%, 0.4%, <0.01%.
func fmtPct(a, b float64) string {
	v := pct(a, b)
	switch {
	case v == 0:
		return "none"
	case v < 0.01:
		return "<0.01%"
	case v < 1:
		return trimZero(fmt.Sprintf("%.2f", v)) + "%"
	}
	return fmt.Sprintf("%.0f%%", v)
}

func pct(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return a / b * 100
}

// orStr is s, or def when s is empty.
func orStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func parens(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
