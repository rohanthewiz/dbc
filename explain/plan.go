// Package explain turns what a database says about how it will run (or did
// run) a statement into one engine-neutral plan tree, works out where the time
// or the work goes, and says what looks wrong in plain words.
//
// Four engines, six shapes of output, one tree:
//
//	Postgres  EXPLAIN (FORMAT JSON [, ANALYZE, BUFFERS])  ─► ParsePostgresJSON ─┐
//	Postgres  EXPLAIN [ANALYZE] (text, as psql shows it)  ─► ParsePostgresText ─┤
//	bytdb     EXPLAIN (Postgres-style text, no costs)     ─► ParsePostgresText ─┤
//	MySQL     EXPLAIN FORMAT=TREE / EXPLAIN ANALYZE       ─► ParseMySQLTree ────┼─► *Plan ─► Finalize
//	MySQL     EXPLAIN (the classic table), MariaDB too    ─► ParseMySQLTable ───┤      │    (metrics,
//	SQLite    EXPLAIN QUERY PLAN (id, parent, detail)     ─► ParseSQLite ───────┘      │     insights)
//	                                                                                   ▼
//	                                             TUI plan view · text · JSON · interactive HTML
//	                                             PDF · JPEG/PNG (picture.go) · Mermaid (mermaid.go)
//
// THE TREE IS THE LOWEST COMMON DENOMINATOR, NOT THE UNION. A Node has the
// fields every consumer needs to draw and judge a step — its operation, the
// table and index it touches, estimated and actual rows, cost, time — typed,
// so the insight rules can be written once. Everything else an engine says
// about a step (a Postgres "Heap Blocks: exact=1471", a MySQL key_len) rides
// along as ordered Props: shown in the detail panel exactly as the engine
// worded it, never interpreted. Adding an engine means writing a parser that
// fills the typed fields it can; the views and rules then just work.
//
// WHAT IS MISSING IS SAID, NOT FAKED. SQLite reports no costs and no rows;
// bytdb reports neither either; only Postgres and MySQL report per-step
// timings. A Plan records what it has (HasCost, HasEst, Analyzed), and the
// views pick the best metric that exists (see Metric) and label it, rather
// than drawing a confident bar chart out of numbers nobody measured.
package explain

import (
	"fmt"
	"math"
	"strings"
)

// Engine names, as the config's driver names them.
const (
	Postgres = "postgres"
	MySQL    = "mysql"
	SQLite   = "sqlite"
	Bytdb    = "bytdb"
)

// Plan is one explained statement.
type Plan struct {
	Engine    string `json:"engine"`
	Conn      string `json:"conn,omitempty"`      // connection name it ran on
	Statement string `json:"statement,omitempty"` // the statement explained
	Command   string `json:"command,omitempty"`   // the EXPLAIN actually sent, for the record

	// Analyzed: the statement was executed and every step carries measured
	// rows, loops and time. Measured: the statement was executed and timed
	// as a whole, on an engine that cannot time its steps (SQLite, bytdb) —
	// the root carries the total, the steps carry nothing.
	Analyzed bool `json:"analyzed"`
	Measured bool `json:"measured,omitempty"`

	PlanningMs  float64 `json:"planning_ms,omitempty"`
	ExecutionMs float64 `json:"execution_ms,omitempty"`
	ResultRows  float64 `json:"result_rows,omitempty"` // Measured: the rows the statement returned

	// Notes are facts about how the plan was obtained that a reader must
	// know to read it right ("ran inside a transaction that was rolled
	// back"). They are not findings about the plan; Insights are.
	Notes    []string  `json:"notes,omitempty"`
	Root     *Node     `json:"root"`
	Insights []Insight `json:"insights,omitempty"`

	// Metric is the best measure this plan has, which the views size bars
	// and flame blocks by unless the user picks another (see Metrics).
	Metric Metric `json:"metric"`

	// Raw is the engine's own output, verbatim — what "copy the plan"
	// copies, since that is what a DBA or a search engine recognizes.
	Raw string `json:"raw,omitempty"`

	nodes []*Node // preorder; nodes[i].ID == i, set by Finalize
}

// Node is one step of a plan.
type Node struct {
	ID int `json:"id"`

	// Op is the step as the engine names it ("Hash Join", "Seq Scan",
	// "SEARCH", "Index lookup"); Kind is what sort of step that is, which is
	// what the views color and the rules reason by.
	Op   string `json:"op"`
	Kind Kind   `json:"kind"`

	Relation string `json:"relation,omitempty"` // the table, when the step reads one
	Alias    string `json:"alias,omitempty"`    // what the statement calls it
	Index    string `json:"index,omitempty"`    // the index it uses, if any

	// Relationship is how the step feeds its parent when that is not the
	// plain input it is by default: "Inner"/"Outer" of a join, "SubPlan",
	// "InitPlan", "CTE recent". Shown, never interpreted.
	Relationship string `json:"relationship,omitempty"`

	// Props is everything else the engine said about the step, in its
	// order and its words: "Filter" → "(age > 30)".
	Props []Prop `json:"props,omitempty"`

	// Estimates — per execution of the step, as every engine reports them.
	HasCost     bool    `json:"has_cost,omitempty"`
	StartupCost float64 `json:"startup_cost,omitempty"`
	TotalCost   float64 `json:"total_cost,omitempty"`
	HasEst      bool    `json:"has_est,omitempty"`
	EstRows     float64 `json:"est_rows,omitempty"`
	Width       float64 `json:"width,omitempty"`

	// Actuals — Postgres and MySQL report time and rows as an average per
	// loop, so a step inside a nested loop that ran 4,000 times says
	// "0.001 ms, 1 row" and means four milliseconds and four thousand rows.
	// Finalize multiplies it out (TotalMs, RowsOut).
	HasActual     bool    `json:"has_actual,omitempty"`
	ActualStartMs float64 `json:"actual_start_ms,omitempty"`
	ActualMs      float64 `json:"actual_ms,omitempty"`
	ActualRows    float64 `json:"actual_rows,omitempty"`
	Loops         float64 `json:"loops,omitempty"`
	NeverExecuted bool    `json:"never_executed,omitempty"`

	// Typed extras the insight rules read. Each is zero when the engine did
	// not say, which every rule treats as "no evidence", never as "zero".
	RowsRemoved     float64 `json:"rows_removed,omitempty"` // discarded by a filter or recheck, per loop
	SortSpill       bool    `json:"sort_spill,omitempty"`   // a sort that went to disk
	SpillKB         float64 `json:"spill_kb,omitempty"`     // how much it wrote there
	Batches         float64 `json:"batches,omitempty"`      // hash / hash-aggregate batches (>1 = spilled)
	WorkersPlanned  float64 `json:"workers_planned,omitempty"`
	WorkersLaunched float64 `json:"workers_launched,omitempty"`
	SharedHit       float64 `json:"shared_hit,omitempty"`   // buffer pages found in cache
	SharedRead      float64 `json:"shared_read,omitempty"`  // pages read from disk (or the OS cache)
	TempBlocks      float64 `json:"temp_blocks,omitempty"`  // pages written to temp files
	TableRows       float64 `json:"table_rows,omitempty"`   // the whole table's size, when looked up
	FilteredPct     float64 `json:"filtered_pct,omitempty"` // MySQL's estimate of the rows a filter keeps, 0–100

	// Flags are engine facts the rules key off that have no typed field:
	// "filesort", "temporary", "join-buffer", "dependent", "correlated",
	// "auto-index", "temp-btree". Lowercase, stable, never shown raw.
	Flags []string `json:"flags,omitempty"`

	Children []*Node `json:"children,omitempty"`

	// Derived by Finalize — see there for how each is worked out.
	Depth    int     `json:"depth"`
	TotalMs  float64 `json:"total_ms,omitempty"` // inclusive, across every loop
	SelfMs   float64 `json:"self_ms,omitempty"`  // TotalMs less the children's
	SelfCost float64 `json:"self_cost,omitempty"`
	RowsOut  float64 `json:"rows_out,omitempty"` // rows the step produced, across every loop
	// Participants is how many processes share the step's loops: the
	// enclosing Gather's workers plus its leader for a step under one, 1
	// otherwise. Loops/Participants is how many times the step really ran.
	Participants float64 `json:"participants,omitempty"`
	// Misestimate is actual rows over estimated rows, per loop: 1 is spot on,
	// 27 is "27× more than planned", 0.04 is "25× fewer". 0 when either side
	// is unknown.
	Misestimate float64 `json:"misestimate,omitempty"`
	// Weights is Self for every metric the plan offers, precomputed so a
	// consumer of the JSON (the HTML view) sizes blocks exactly as the
	// terminal does without re-deriving the shape heuristic.
	Weights map[Metric]float64 `json:"weights,omitempty"`

	parent *Node
}

// Prop is one key/value line of a step's detail.
type Prop struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Kind classifies a step for coloring and reasoning.
type Kind string

const (
	KindScan     Kind = "scan"     // reads a whole table
	KindIndex    Kind = "index"    // reaches rows through an index
	KindJoin     Kind = "join"     // combines two inputs
	KindSort     Kind = "sort"     // orders rows
	KindAgg      Kind = "agg"      // groups, counts, deduplicates, windows
	KindLimit    Kind = "limit"    // stops early
	KindFilter   Kind = "filter"   // drops rows (MySQL's separate Filter step)
	KindHash     Kind = "hash"     // builds an in-memory table: Hash, Memoize, Materialize
	KindSubquery Kind = "subquery" // a subquery, CTE, or derived table
	KindSet      Kind = "set"      // UNION and friends
	KindParallel Kind = "parallel" // gathers the output of parallel workers
	KindModify   Kind = "modify"   // INSERT / UPDATE / DELETE / MERGE
	KindResult   Kind = "result"   // the statement itself, or a constant row
	KindOther    Kind = "other"
)

// Glyph is the one-cell symbol the views draw beside a step of this kind.
// Each is a single-width character from the BMP math and arrows blocks, which
// every terminal font carries — no emoji, whose width terminals disagree on.
func (k Kind) Glyph() string {
	switch k {
	case KindScan:
		return "▤"
	case KindIndex:
		return "◇"
	case KindJoin:
		return "⋈"
	case KindSort:
		return "⇅"
	case KindAgg:
		return "Σ"
	case KindLimit:
		return "≤"
	case KindFilter:
		return "⊃"
	case KindHash:
		return "#"
	case KindSubquery:
		return "↻"
	case KindSet:
		return "∪"
	case KindParallel:
		return "∥"
	case KindModify:
		return "✎"
	case KindResult:
		return "◈"
	}
	return "•"
}

// Metric is what a bar, a percentage, or a flame block's width measures.
type Metric string

const (
	MetricTime  Metric = "time"  // measured milliseconds (Analyzed plans)
	MetricCost  Metric = "cost"  // the planner's cost units
	MetricRows  Metric = "rows"  // rows each step handles
	MetricShape Metric = "shape" // no numbers at all: a heuristic by kind
)

// Label is how the views name the metric.
func (m Metric) Label() string {
	switch m {
	case MetricTime:
		return "time"
	case MetricCost:
		return "cost"
	case MetricRows:
		return "rows"
	}
	return "shape"
}

// Severity grades an insight.
type Severity string

const (
	SevInfo Severity = "info"
	SevWarn Severity = "warn"
	SevCrit Severity = "crit"
)

// Insight is one finding about a plan, in words a user can act on.
type Insight struct {
	Severity Severity `json:"severity"`
	NodeID   int      `json:"node"` // the step it is about; -1 for the plan as a whole
	Title    string   `json:"title"`
	Detail   string   `json:"detail,omitempty"`
	// Fix is the next step, in words; SQL, when there is one, is a statement
	// that would carry it out (a CREATE INDEX, an ANALYZE). It is offered to
	// copy or insert into the editor — never run on the user's behalf.
	Fix string `json:"fix,omitempty"`
	SQL string `json:"sql,omitempty"`
}

// ---------------------------------------------------------------------------
// Tree access
// ---------------------------------------------------------------------------

// Nodes returns every step in preorder — a parent before its children,
// children in the order the engine listed them — so Nodes()[i].ID == i.
func (p *Plan) Nodes() []*Node { return p.nodes }

// Node returns the step with the given ID, or nil.
func (p *Plan) Node(id int) *Node {
	if id < 0 || id >= len(p.nodes) {
		return nil
	}
	return p.nodes[id]
}

// Parent returns the step's parent, nil for the root.
func (n *Node) Parent() *Node { return n.parent }

// Prop returns the value of the first prop named key.
func (n *Node) Prop(key string) (string, bool) {
	for _, p := range n.Props {
		if p.Key == key {
			return p.Value, true
		}
	}
	return "", false
}

// HasFlag reports whether the step carries flag f.
func (n *Node) HasFlag(f string) bool {
	for _, x := range n.Flags {
		if x == f {
			return true
		}
	}
	return false
}

func (n *Node) addProp(k, v string) {
	if v = strings.TrimSpace(v); v != "" {
		n.Props = append(n.Props, Prop{Key: k, Value: v})
	}
}

func (n *Node) flag(f string) {
	if !n.HasFlag(f) {
		n.Flags = append(n.Flags, f)
	}
}

// Target names what the step works on, as a reader would say it:
// "orders o", "orders using orders_status", "orders_status". "" for a step
// that touches no table.
func (n *Node) Target() string {
	var b strings.Builder
	switch {
	case n.Relation != "" && n.Alias != "" && n.Alias != n.Relation:
		b.WriteString(n.Relation + " " + n.Alias)
	case n.Relation != "":
		b.WriteString(n.Relation)
	case n.Alias != "":
		b.WriteString(n.Alias)
	}
	if n.Index != "" {
		if b.Len() > 0 {
			b.WriteString(" using ")
		}
		b.WriteString(n.Index)
	}
	return b.String()
}

// Title is the step's one-line name: its operation and what it works on.
func (n *Node) Title() string {
	if t := n.Target(); t != "" {
		return n.Op + " · " + t
	}
	return n.Op
}

// summaryKeys are the props worth showing inline beside a step, most
// telling first — the condition a join matches on says more about it than
// its filter does.
var summaryKeys = []string{
	"Hash Cond", "Merge Cond", "Join Filter", "Index Cond", "Condition",
	"Recheck Cond", "Filter", "Key", "Lookup", "Sort Key", "Group Key",
	"Cache Key", "Limit", "Range", "Extra",
}

// Summary is the single most telling detail of the step, for the tree's
// inline hint.
func (n *Node) Summary() string {
	for _, k := range summaryKeys {
		if v, ok := n.Prop(k); ok {
			return v
		}
	}
	return ""
}

// RowsExamined is how many rows the step looked at across every loop: the
// ones it passed on plus the ones its filter threw away. For a scan this is
// the work it did, which RowsOut alone understates — a scan that reads
// 200,000 rows to return 10 "produced" 10.
func (n *Node) RowsExamined() float64 {
	loops := math.Max(n.Loops, 1)
	if n.HasActual {
		return (n.ActualRows + n.RowsRemoved) * loops
	}
	return n.RowsOut
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// Metrics lists the measures this plan can be viewed by, best first.
func (p *Plan) Metrics() []Metric {
	var ms []Metric
	if p.Analyzed {
		ms = append(ms, MetricTime)
	}
	if p.Root != nil && anyNode(p.Root, func(n *Node) bool { return n.HasCost }) {
		ms = append(ms, MetricCost)
	}
	if p.Root != nil && anyNode(p.Root, func(n *Node) bool { return n.HasEst || n.HasActual }) {
		ms = append(ms, MetricRows)
	}
	return append(ms, MetricShape)
}

// Self is the step's own share of metric m — what it costs excluding its
// children. Always ≥ 0.
func (n *Node) Self(m Metric) float64 {
	switch m {
	case MetricTime:
		return n.SelfMs
	case MetricCost:
		return n.SelfCost
	case MetricRows:
		// the rows a step handles is the honest "work" measure when there
		// is no clock: a scan's examined rows, anything else's output
		if n.Kind == KindScan || n.Kind == KindIndex {
			return n.RowsExamined()
		}
		return n.RowsOut
	}
	return shapeWeight(n)
}

// Inclusive is the step's metric including everything under it. It is
// computed as Self plus the children's Inclusive rather than read from the
// engine's own inclusive figure, so a flame block is always at least as wide
// as its children together — engines' inclusive numbers do not always add up
// (parallel workers, loops, subplans), and a child drawn wider than its
// parent would be nonsense.
func (n *Node) Inclusive(m Metric) float64 {
	t := n.Self(m)
	for _, c := range n.Children {
		t += c.Inclusive(m)
	}
	return t
}

// Share is the step's Self as a fraction (0–1) of the whole plan's.
func (p *Plan) Share(n *Node, m Metric) float64 {
	if p.Root == nil {
		return 0
	}
	total := p.Root.Inclusive(m)
	if total <= 0 {
		return 0
	}
	return n.Self(m) / total
}

// shapeWeight is the fallback metric for an engine that reports no numbers:
// a rough "how much work is a step like this" by kind, so a SQLite plan's
// flame graph still puts a full table scan's block ahead of a primary-key
// lookup's. A known table size makes a full scan weigh what the table weighs
// (in log terms, so a million-row scan does not squeeze everything else out
// of view). The views label it "shape", never "cost".
func shapeWeight(n *Node) float64 {
	switch n.Kind {
	case KindScan:
		if n.TableRows > 0 {
			return 4 + math.Log10(n.TableRows+1)*2
		}
		return 8
	case KindSort, KindHash:
		return 3
	case KindIndex, KindAgg, KindSubquery:
		return 2
	}
	return 1
}

func anyNode(n *Node, f func(*Node) bool) bool {
	if f(n) {
		return true
	}
	for _, c := range n.Children {
		if anyNode(c, f) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Finalize
// ---------------------------------------------------------------------------

// Finalize numbers the steps, links parents, derives every computed field
// and runs the insight rules. Parsers call it last; it is idempotent, so a
// caller that edits a plan (the db layer adding table sizes) calls it again.
//
// Time is the subtle part. Engines report a step's time as an average per
// loop, INCLUSIVE of its children, so:
//
//	TotalMs(n) = ActualMs(n) × Loops(n)        (÷ workers inside a Gather)
//	SelfMs(n)  = TotalMs(n) − Σ TotalMs(child)  clamped at 0
//
// The clamp matters: a Postgres InitPlan, a CTE referenced twice, or a
// parallel child whose per-worker average does not add up can make the naive
// difference negative, and a negative bar is worse than a slightly
// generous one.
func (p *Plan) Finalize() {
	p.nodes = p.nodes[:0]
	if p.Root == nil {
		p.Root = &Node{Op: "Empty plan", Kind: KindResult}
	}
	var walk func(n, parent *Node, depth int, workers float64)
	walk = func(n, parent *Node, depth int, workers float64) {
		n.ID, n.parent, n.Depth = len(p.nodes), parent, depth
		p.nodes = append(p.nodes, n)
		if n.Kind == "" {
			n.Kind = Classify(n.Op)
		}
		// Inside a Gather each worker runs the subtree and reports its own
		// loop, and the leader usually joins in; Loops counts them all while
		// the wall clock ran them side by side. Dividing by the participants
		// turns summed worker time back into elapsed time.
		if n.Kind == KindParallel && n.WorkersLaunched > 0 {
			workers = n.WorkersLaunched + 1
		}
		loops := math.Max(n.Loops, 1)
		n.Participants = math.Max(workersFor(n, workers), 1)
		n.TotalMs, n.RowsOut = 0, 0
		if n.HasActual {
			n.TotalMs = n.ActualMs * loops / n.Participants
			n.RowsOut = n.ActualRows * loops
		} else if n.HasEst {
			n.RowsOut = n.EstRows * loops
		}
		n.Misestimate = 0
		if n.HasActual && n.HasEst && !n.NeverExecuted {
			// floored at one row: Postgres rounds a per-loop average below one
			// to 0, and "0 of an estimated 1" is rounding, not a misestimate
			n.Misestimate = math.Max(n.ActualRows, 1) / math.Max(n.EstRows, 1)
		}
		for _, c := range n.Children {
			walk(c, n, depth+1, workers)
		}
		var childMs, childCost float64
		for _, c := range n.Children {
			childMs += c.TotalMs
			childCost += c.TotalCost
		}
		if !n.HasActual && !n.NeverExecuted {
			// a label step with no timing of its own (MySQL's "Select #2", a
			// synthetic root) spans exactly its children
			n.TotalMs = childMs
		}
		n.SelfMs = math.Max(n.TotalMs-childMs, 0)
		n.SelfCost = 0
		if n.HasCost {
			n.SelfCost = math.Max(n.TotalCost-childCost, 0)
		}
	}
	walk(p.Root, nil, 0, 1)

	if p.ExecutionMs == 0 && p.Analyzed {
		p.ExecutionMs = p.Root.TotalMs
	}
	ms := p.Metrics()
	p.Metric = ms[0]
	for _, n := range p.nodes {
		n.Weights = make(map[Metric]float64, len(ms))
		for _, m := range ms {
			n.Weights[m] = n.Self(m)
		}
	}
	p.Insights = analyze(p)
}

// workersFor is the parallelism a step's reported loops include: the
// enclosing Gather's participants for a step below it, 1 otherwise. The
// Gather itself runs once, in the leader.
func workersFor(n *Node, workers float64) float64 {
	if n.Kind == KindParallel {
		return 1
	}
	return workers
}

// Classify maps an operation name to its Kind. It is keyword matching over
// every engine's vocabulary at once — the names barely overlap, so one table
// serves all four, and a parser only calls it for steps it did not classify
// itself.
func Classify(op string) Kind {
	o := strings.ToLower(op)
	has := func(ws ...string) bool {
		for _, w := range ws {
			if strings.Contains(o, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("modifytable", "insert", "update", "delete", "merge on"):
		return KindModify
	case has("gather"):
		return KindParallel
	case has("temp b-tree for order by", "temp b-tree for right part of order by"):
		return KindSort
	case has("temp b-tree for group by", "temp b-tree for distinct"):
		return KindAgg
	case has("join", "nested loop", "semi", "anti"):
		return KindJoin
	case has("bitmap", "index", "search", "point get", "lookup", "primary key"):
		return KindIndex
	case has("seq scan", "table scan", "full scan", "scan "), o == "scan":
		if has("temporary", "<temporary>") {
			return KindHash
		}
		return KindScan
	case has("sort"):
		return KindSort
	case has("aggregate", "group", "unique", "windowagg", "window", "distinct", "setop"):
		return KindAgg
	case has("limit"):
		return KindLimit
	case has("filter"):
		return KindFilter
	case has("hash", "memoize", "materialize", "temporary"):
		return KindHash
	case has("append", "union", "compound", "intersect", "except", "left-most", "recursive"):
		return KindSet
	case has("subquery", "subplan", "initplan", "cte", "co-routine", "select #", "derived", "function scan", "values scan"):
		return KindSubquery
	case has("result", "query", "constant", "statement"):
		return KindResult
	}
	return KindOther
}

// ---------------------------------------------------------------------------
// Number formatting, shared by every view so they agree to the digit
// ---------------------------------------------------------------------------

// FmtRows formats a row count compactly: 950, 36.7k, 1.2M. Fractional
// per-loop averages below 10 keep one decimal ("0.7").
func FmtRows(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return trimZero(fmt.Sprintf("%.1f", v/1e9)) + "G"
	case a >= 1e6:
		return trimZero(fmt.Sprintf("%.1f", v/1e6)) + "M"
	case a >= 1e4:
		return trimZero(fmt.Sprintf("%.1f", v/1e3)) + "k"
	case a >= 10 || v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	}
	return trimZero(fmt.Sprintf("%.1f", v))
}

// FmtCount formats a count in full with thousands separators: 36,650.
func FmtCount(v float64) string {
	s := fmt.Sprintf("%.0f", math.Round(v))
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// FmtMs formats milliseconds at a precision that suits the size: 840 µs,
// 21.6 ms, 3.42 s, 2m05s.
func FmtMs(ms float64) string {
	switch {
	case ms <= 0:
		return "0 ms"
	case ms < 1:
		return fmt.Sprintf("%.0f µs", ms*1000)
	case ms < 100:
		return trimZero(fmt.Sprintf("%.1f", ms)) + " ms"
	case ms < 1000:
		return fmt.Sprintf("%.0f ms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.2f s", ms/1000)
	}
	s := int(ms / 1000)
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// FmtCost formats planner cost units compactly.
func FmtCost(c float64) string {
	if c < 10 {
		return trimZero(fmt.Sprintf("%.2f", c))
	}
	return FmtRows(c)
}

// FmtKB formats a size given in kilobytes.
func FmtKB(kb float64) string {
	switch {
	case kb >= 1024*1024:
		return trimZero(fmt.Sprintf("%.1f", kb/1024/1024)) + " GB"
	case kb >= 1024:
		return trimZero(fmt.Sprintf("%.1f", kb/1024)) + " MB"
	}
	return fmt.Sprintf("%.0f kB", kb)
}

// FmtMetric formats a value of metric m.
func FmtMetric(m Metric, v float64) string {
	switch m {
	case MetricTime:
		return FmtMs(v)
	case MetricCost:
		return FmtCost(v)
	case MetricRows:
		return FmtRows(v)
	}
	return ""
}

// FmtFactor describes a misestimate: "×27 more" / "×25 fewer", "" when it
// is within 2× (planner estimates are rarely closer, and nobody acts on 1.3×).
func FmtFactor(f float64) string {
	switch {
	case f == 0:
		return ""
	case f >= 2:
		return "×" + FmtRows(math.Round(f)) + " more"
	case f <= 0.5:
		return "×" + FmtRows(math.Round(1/f)) + " fewer"
	}
	return ""
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
