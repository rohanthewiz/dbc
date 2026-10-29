// Package designer turns a visual query design — tables on a canvas, the
// joins drawn between them, the columns ticked, the filters and sorts —
// into a SELECT, and judges it while it is being built: is it CORRECT (will
// the numbers it returns be the ones the user means?), is it EFFICIENT
// (will the engine have to read far more than it keeps?), and what does it
// say, in plain words?
//
//	dbc web (designer.js)                       this package
//	  canvas, cards, inspector ──Design JSON──► Analyze(design, catalog)
//	                                              │ normalize (aliases, ids)
//	                                              │ plan the FROM tree      tree.go
//	                                              │ write the SQL           sql.go
//	                                              │ run the rules           lint.go
//	                                              │ tell the story          story.go
//	  cards, edges, guide, SQL ◄──Analysis JSON───┘
//	  AddTable (FK auto-join) ────────────────────► suggest.go
//	  live EXPLAIN (estimates only) ──────────────► plan.go (paints the plan
//	                                                 back onto the cards)
//
// THE DESIGN IS THE SOURCE OF TRUTH, NOT THE SQL. The page never edits SQL
// text; it edits the design, and the SQL is regenerated from it every
// time. That is what lets the designer be correct BY CONSTRUCTION where
// hand-written SQL is most often wrong: GROUP BY always lists exactly the
// non-aggregated columns, an OR group is always parenthesized, a filter on
// an aggregate always lands in HAVING, a date filter is always written as a
// range an index can serve, a LIKE value always has its % and _ escaped.
// The rules in lint.go are then left with what construction cannot decide
// for the user — mostly what the JOINS do to the row count, which is where
// real queries go silently wrong.
//
// The package is a leaf like erd and explain: it knows nothing of drivers
// or connections. A Catalog (the schema, its indexes, row estimates) is
// handed in, so everything here is tested without a database.
package designer

// Design is what the page edits, saves with the tab, and sends to Analyze.
// Tables, joins, filters and sorts refer to tables by ID, never by alias:
// an alias may be renamed, an ID never changes.
type Design struct {
	Tables  []Table  `json:"tables"`
	Joins   []Join   `json:"joins"`
	Columns []Output `json:"columns"`
	Filters []Filter `json:"filters"`
	Sorts   []Sort   `json:"sorts"`
	// Main is the ID of the table the query starts FROM; "" means the
	// first table. It decides which side of each join is "kept" by a left
	// join's reading order, and which table the story starts with.
	Main     string `json:"main,omitempty"`
	Distinct bool   `json:"distinct,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Offset   int    `json:"offset,omitempty"`
}

// Table is one table (or view) on the canvas. The same table may be on the
// canvas twice (a self-join: an employee and their manager) under two IDs
// and two aliases.
type Table struct {
	ID     string  `json:"id"`
	Schema string  `json:"schema"`
	Name   string  `json:"name"`
	Alias  string  `json:"alias,omitempty"` // filled in by Analyze when empty
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
}

// JoinKind is how two tables are combined.
type JoinKind string

// The kinds. The last three are the designer's own, beyond the four the
// SQL standard spells as JOINs, because they are what a person most often
// means when an INNER JOIN gives them the wrong number of rows:
//
//	inner      rows that match on both sides
//	left       every row of Left, matched or not
//	full       every row of either side
//	exists     rows of Left that HAVE a matching Right: WHERE EXISTS (…).
//	           Each Left row once, however many Right rows match — no
//	           fan-out, and the engine stops at the first match.
//	notexists  rows of Left that have NO matching Right: WHERE NOT EXISTS.
//	           Unlike NOT IN, a NULL on the Right side cannot empty it.
//	summary    Right is aggregated per join key BEFORE the join, then
//	           attached as one row per Left row: the totals of a one-to-many
//	           child without multiplying Left's rows.
const (
	JoinInner     JoinKind = "inner"
	JoinLeft      JoinKind = "left"
	JoinFull      JoinKind = "full"
	JoinExists    JoinKind = "exists"
	JoinNotExists JoinKind = "notexists"
	JoinSummary   JoinKind = "summary"
)

// tests reports whether the kind only tests Right (EXISTS, NOT EXISTS),
// never bringing its columns or rows into the result.
func (k JoinKind) tests() bool { return k == JoinExists || k == JoinNotExists }

// subquery reports whether Right is read in a subquery of its own: the
// testing kinds and summary. Such a Right must be a leaf of the join tree
// (joined to nothing else), and its filters go inside the subquery.
func (k JoinKind) subquery() bool { return k.tests() || k == JoinSummary }

// Join connects two tables of the canvas on one or more column pairs,
// ANDed. Left and Right are table IDs; for a left join Left is the side
// whose rows are all kept, for the testing kinds and summary Right is the
// side tested or summarized.
type Join struct {
	ID    string   `json:"id"`
	Left  string   `json:"left"`
	Right string   `json:"right"`
	Kind  JoinKind `json:"kind"`
	On    []Pair   `json:"on"`
	// FK names the foreign key the join was made from, "" for one drawn by
	// hand or guessed by name (Guess).
	FK    string `json:"fk,omitempty"`
	Guess bool   `json:"guess,omitempty"`
}

// Pair is one condition of a join: Left's column L equals Right's column R.
type Pair struct {
	L string `json:"l"`
	R string `json:"r"`
}

// Agg is an aggregate function, or "" for none.
type Agg string

// The aggregates. Count with no column is count(*): the rows of the group.
const (
	AggNone          Agg = ""
	AggCount         Agg = "count"
	AggCountDistinct Agg = "count_distinct"
	AggSum           Agg = "sum"
	AggAvg           Agg = "avg"
	AggMin           Agg = "min"
	AggMax           Agg = "max"
)

// duplicates reports whether a row repeated by a join changes the
// aggregate's answer. min and max see the same values however often each
// repeats, and count(DISTINCT) counts each value once; sum, avg and count
// count every copy.
func (a Agg) duplicates() bool { return a == AggSum || a == AggAvg || a == AggCount }

// Ref names a column of a canvas table, possibly aggregated. It is how
// outputs, filters and sorts point at what they are about. Column "" with
// AggCount is count(*): over the whole result when Table is "" too, over
// one summarized table's rows when Table names it.
type Ref struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Agg    Agg    `json:"agg,omitempty"`
}

// Output is one column of the result, in order.
type Output struct {
	Ref
	Alias string `json:"alias,omitempty"`
}

// Filter is one condition. Filters are ANDed, except those sharing a Group,
// which are ORed together first and parenthesized as one condition — so the
// classic `a AND b OR c` precedence slip cannot be built.
//
// A filter on an aggregate (Agg set) is a condition on groups: it is written
// into HAVING. Every other filter is on rows, written into WHERE — or, for a
// table read in a subquery (exists, notexists, summary), into the
// subquery's own WHERE, where it means "has a matching row WITH this".
type Filter struct {
	Ref
	Op     string   `json:"op"`
	Values []string `json:"values,omitempty"`
	Group  string   `json:"group,omitempty"`
	// OrNull also keeps the rows where the column is NULL. A NULL compared
	// with anything is unknown, so `status <> 'void'` drops NULL statuses
	// too — usually not what was meant (see lint.go, ruleNullDrop).
	OrNull bool `json:"orNull,omitempty"`
	// InJoin writes the filter into its table's join condition rather than
	// WHERE. On a left-joined table that is the difference between "keep
	// every customer, with only their 2024 orders" (in the join) and "keep
	// only customers with a 2024 order" (in WHERE, which turns the left
	// join back into an inner one).
	InJoin bool `json:"inJoin,omitempty"`
	// IgnoreCase makes contains, starts with and ends with match letters
	// whatever their case; other operators ignore it. A plain LIKE's case
	// rule is the engine's own — case-sensitive on Postgres, A–Z-blind on
	// SQLite, the column's collation on MySQL — so without it the same
	// design can match different rows per engine. With it, Postgres and
	// bytdb write ILIKE and the others lower(x) LIKE lower(v) (sql.go like).
	// Either way an ordinary index on the column no longer serves a
	// starts-with (lint.go ruleContains says so, and what would).
	IgnoreCase bool `json:"ignoreCase,omitempty"`
}

// likeOp reports whether op is written as a LIKE pattern: the operators
// IgnoreCase applies to.
func likeOp(op string) bool { return op == OpContains || op == OpStarts || op == OpEnds }

// folds reports whether the filter is a LIKE written to ignore case.
func (f *Filter) folds() bool { return f.IgnoreCase && likeOp(f.Op) }

// The filter operators. Most are what they say; the designer's own are the
// date ones, which are written as RANGES (`d >= '2024-01-01' AND d <
// '2025-01-01'`, never `EXTRACT(year FROM d) = 2024`) so that an index on
// the column can serve them, and range, the half-open interval [a, b) that
// BETWEEN on a timestamp should usually have been.
const (
	OpEq       = "eq"
	OpNe       = "ne"
	OpLt       = "lt"
	OpLe       = "le"
	OpGt       = "gt"
	OpGe       = "ge"
	OpBetween  = "between"  // a <= x <= b
	OpRange    = "range"    // a <= x < b
	OpIn       = "in"       // one of the values
	OpNotIn    = "notin"    // none of the values
	OpNull     = "null"     // IS NULL
	OpNotNull  = "notnull"  // IS NOT NULL
	OpContains = "contains" // LIKE '%v%'
	OpStarts   = "starts"   // LIKE 'v%'
	OpEnds     = "ends"     // LIKE '%v'
	OpYear     = "year"     // within calendar year v
	OpOn       = "on"       // within day v (all of it, for a timestamp)
	OpLastDays = "lastdays" // within the last v days, up to now
)

// opArity is how many values each operator takes; -1 is one or more.
var opArity = map[string]int{
	OpEq: 1, OpNe: 1, OpLt: 1, OpLe: 1, OpGt: 1, OpGe: 1,
	OpBetween: 2, OpRange: 2, OpIn: -1, OpNotIn: -1,
	OpNull: 0, OpNotNull: 0,
	OpContains: 1, OpStarts: 1, OpEnds: 1,
	OpYear: 1, OpOn: 1, OpLastDays: 1,
}

// Sort is one ORDER BY key, in order.
type Sort struct {
	Ref
	Desc bool `json:"desc,omitempty"`
}

// clone is a deep copy, for the fixes: each fix is a whole new design the
// page can swap in, so it must share no slice with the original.
func (d Design) clone() Design {
	c := d
	c.Tables = append([]Table(nil), d.Tables...)
	c.Joins = make([]Join, len(d.Joins))
	for i, j := range d.Joins {
		j.On = append([]Pair(nil), j.On...)
		c.Joins[i] = j
	}
	c.Columns = append([]Output(nil), d.Columns...)
	c.Filters = make([]Filter, len(d.Filters))
	for i, f := range d.Filters {
		f.Values = append([]string(nil), f.Values...)
		c.Filters[i] = f
	}
	c.Sorts = append([]Sort(nil), d.Sorts...)
	return c
}

// table finds a canvas table by ID.
func (d *Design) table(id string) *Table {
	for i := range d.Tables {
		if d.Tables[i].ID == id {
			return &d.Tables[i]
		}
	}
	return nil
}

// join finds a join by ID.
func (d *Design) join(id string) *Join {
	for i := range d.Joins {
		if d.Joins[i].ID == id {
			return &d.Joins[i]
		}
	}
	return nil
}
