package explain

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// The fixtures in testdata are real output, captured from Postgres 17 and
// MySQL 8.4 over the same two tables (users 20k rows, orders 200k rows,
// orders indexed only on status) — see the session doc for the queries. They
// pin the parsers to what the engines actually print, not to what their
// documentation says they print.

func loadPGJSON(t *testing.T, name string) *Plan {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePostgresJSON(b)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

func loadLines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func loadTable(t *testing.T, name string) ([]string, [][]string) {
	lines := loadLines(t, name)
	var rows [][]string
	for _, l := range lines[1:] {
		rows = append(rows, strings.Split(l, "\t"))
	}
	return strings.Split(lines[0], "\t"), rows
}

// ops lists the plan's steps in preorder, for structural asserts.
func ops(p *Plan) string {
	var s []string
	for _, n := range p.Nodes() {
		s = append(s, strings.Repeat(" ", n.Depth)+n.Op)
	}
	return strings.Join(s, "\n")
}

// finding returns the first insight whose title contains sub.
func finding(p *Plan, sub string) (Insight, bool) {
	for _, in := range p.Insights {
		if strings.Contains(in.Title, sub) {
			return in, true
		}
	}
	return Insight{}, false
}

func titles(p *Plan) string {
	var s []string
	for _, in := range p.Insights {
		s = append(s, string(in.Severity)+": "+in.Title)
	}
	return strings.Join(s, "\n")
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

func TestPostgresJSONStructure(t *testing.T) {
	p := loadPGJSON(t, "pg_analyze_join.json")
	want := `Sort
 HashAggregate
  Hash Join
   Bitmap Heap Scan
    Bitmap Index Scan
   Hash
    Seq Scan`
	if got := ops(p); got != want {
		t.Fatalf("tree:\n%s\nwant:\n%s", got, want)
	}
	if !p.Analyzed || p.Metric != MetricTime {
		t.Errorf("analyzed=%v metric=%s", p.Analyzed, p.Metric)
	}
	join := p.Node(2)
	if join.Kind != KindJoin || join.Summary() != "(o.user_id = u.id)" {
		t.Errorf("join: kind=%s summary=%q", join.Kind, join.Summary())
	}
	scan := p.Node(6)
	if scan.Relation != "users" || scan.Alias != "u" || scan.Kind != KindScan || scan.RowsRemoved != 4341 {
		t.Errorf("seq scan: %+v", scan)
	}
	if scan.Relationship != "Outer" || p.Node(5).Relationship != "Inner" {
		t.Errorf("relationships: %q %q", scan.Relationship, p.Node(5).Relationship)
	}
	// self times are the inclusive minus the children's, never negative, and
	// the root's inclusive equals the sum of every step's self
	var sum float64
	for _, n := range p.Nodes() {
		if n.SelfMs < 0 {
			t.Errorf("%s: negative self time %v", n.Op, n.SelfMs)
		}
		sum += n.SelfMs
	}
	if !near(sum, p.Root.Inclusive(MetricTime)) {
		t.Errorf("Σ self %v ≠ root inclusive %v", sum, p.Root.Inclusive(MetricTime))
	}
	if !strings.Contains(titles(p), "No problems found") {
		t.Errorf("a well-indexed join should be clear:\n%s", titles(p))
	}
}

// A parallel scan's loops are workers sharing ONE pass, not repeats: the
// finding is the filter's waste, never "runs 2 times".
func TestParallelLoopsAreNotRepeats(t *testing.T) {
	p := loadPGJSON(t, "pg_parallel_seqscan.json")
	if _, bad := finding(p, "runs 2 times"); bad {
		t.Fatalf("parallel workers read as repeated scans:\n%s", titles(p))
	}
	in, ok := finding(p, "keeps 10 of 200,000 rows")
	if !ok || in.Severity != SevWarn || in.SQL != "CREATE INDEX idx_orders_user_id ON orders (user_id);" {
		t.Fatalf("want the missing-index finding with its fix:\n%s\n%+v", titles(p), in)
	}
	var scan *Node
	for _, n := range p.Nodes() {
		if n.Kind == KindScan {
			scan = n
		}
	}
	// 2 loops shared by a leader and one worker: elapsed, not summed, time
	if scan.Participants != 2 || !near(scan.TotalMs, scan.ActualMs) {
		t.Errorf("participants=%v total=%v actual=%v", scan.Participants, scan.TotalMs, scan.ActualMs)
	}
}

func TestSortSpill(t *testing.T) {
	p := loadPGJSON(t, "pg_sort_disk.json")
	in, ok := finding(p, "Sort spilled")
	if !ok || in.Severity != SevCrit || !strings.HasPrefix(in.SQL, "SET work_mem = '") {
		t.Fatalf("want a critical spill with a work_mem fix:\n%s", titles(p))
	}
	if p.Insights[0].Title != in.Title {
		t.Errorf("the critical finding should lead:\n%s", titles(p))
	}
}

// A correlated subquery: the scan inside it runs once per outer row, and the
// subquery itself is named.
func TestSubplanRepeatedScan(t *testing.T) {
	p := loadPGJSON(t, "pg_subplan.json")
	if in, ok := finding(p, "Full scan of orders runs 49 times"); !ok || in.Severity != SevCrit {
		t.Fatalf("want a critical repeated scan:\n%s", titles(p))
	}
	if _, ok := finding(p, "Subquery runs 49 times"); !ok {
		t.Errorf("want the correlated-subquery finding:\n%s", titles(p))
	}
	if len(p.Notes) == 0 || !strings.Contains(p.Notes[0], "JIT") {
		t.Errorf("JIT time should be a note: %v", p.Notes)
	}
}

func TestMisestimate(t *testing.T) {
	p := loadPGJSON(t, "pg_misestimate.json")
	in, ok := finding(p, "Planner expected 1,280 rows from skew, got 34,000 (×27 more)")
	if !ok || in.SQL != "ANALYZE skew;" {
		t.Fatalf("want the misestimate with ANALYZE:\n%s", titles(p))
	}
	if !near(p.Root.Misestimate, 34000.0/1280) {
		t.Errorf("misestimate = %v", p.Root.Misestimate)
	}
}

// Postgres rounds a per-loop average below one row to 0; "0 of 1" is
// rounding, not a planner error — the Memoize over a 40,000-loop lookup.
func TestSubRowRoundingIsNotAMisestimate(t *testing.T) {
	p := loadPGJSON(t, "pg_nested_loop.json")
	for _, n := range p.Nodes() {
		if n.Op == "Memoize" && n.Misestimate != 1 {
			t.Errorf("memoize misestimate = %v", n.Misestimate)
		}
	}
	// and a primary-key lookup that filters out its one row per loop is not
	// "an index fetching too much"
	if _, bad := finding(p, "discards"); bad {
		t.Errorf("per-row PK lookups flagged as index waste:\n%s", titles(p))
	}
}

func TestIndexWaste(t *testing.T) {
	p := loadPGJSON(t, "pg_update.json")
	in, ok := finding(p, "then discards 98%")
	if !ok {
		t.Fatalf("want the bitmap scan's waste:\n%s", titles(p))
	}
	// the suggested index leads with what the index already narrows on
	if in.SQL != "CREATE INDEX idx_orders_status_total ON orders (status, total);" {
		t.Errorf("sql = %q", in.SQL)
	}
	if p.Root.Kind != KindModify || p.Root.Op != "Update" {
		t.Errorf("root = %s (%s)", p.Root.Op, p.Root.Kind)
	}
}

// The text format parses to the same tree as the JSON format of the same run.
func TestPostgresTextMatchesJSON(t *testing.T) {
	for _, pair := range [][2]string{
		{"pg_analyze_join.txt", "pg_analyze_join.json"},
		{"pg_parallel_seqscan.txt", "pg_parallel_seqscan.json"},
		{"pg_estimate_join.txt", "pg_estimate_join.json"},
	} {
		tp, err := ParsePostgresText(loadLines(t, pair[0]), Postgres)
		if err != nil {
			t.Fatal(err)
		}
		jp := loadPGJSON(t, pair[1])
		if ops(tp) != ops(jp) {
			t.Errorf("%s:\n%s\n\nvs %s:\n%s", pair[0], ops(tp), pair[1], ops(jp))
		}
		if tp.Analyzed != jp.Analyzed || (tp.Analyzed && tp.ExecutionMs <= 0) {
			t.Errorf("%s: analyzed=%v exec=%v", pair[0], tp.Analyzed, tp.ExecutionMs)
		}
		for i, n := range tp.Nodes() {
			j := jp.Nodes()[i]
			if n.Relation != j.Relation || n.Alias != j.Alias || n.Index != j.Index || n.EstRows != j.EstRows {
				t.Errorf("%s step %d: text %q/%q/%q/%v, json %q/%q/%q/%v", pair[0], i,
					n.Relation, n.Alias, n.Index, n.EstRows, j.Relation, j.Alias, j.Index, j.EstRows)
			}
		}
	}
}

// bytdb speaks the text format without costs; SubPlan labels and never
// executed steps are part of the grammar too.
func TestPostgresTextGrammar(t *testing.T) {
	lines := []string{
		"Seq Scan on users u  (cost=0.00..10.00 rows=5 width=4) (actual time=0.010..0.020 rows=5 loops=1)",
		"  Filter: (age > 3)",
		"  Rows Removed by Filter: 95",
		"  SubPlan 1",
		"    ->  Index Scan using orders_pkey on orders o  (cost=0.29..8.30 rows=1 width=4) (never executed)",
		"          Index Cond: (id = u.id)",
		"Planning Time: 0.100 ms",
		"Execution Time: 0.500 ms",
	}
	p, err := ParsePostgresText(lines, Postgres)
	if err != nil {
		t.Fatal(err)
	}
	sub := p.Node(1)
	if sub == nil || sub.Relationship != "SubPlan 1" || !sub.NeverExecuted || sub.Index != "orders_pkey" {
		t.Fatalf("subplan child: %+v", sub)
	}
	if v, _ := sub.Prop("Index Cond"); v != "(id = u.id)" {
		t.Errorf("index cond = %q", v)
	}
	if p.Root.RowsRemoved != 95 || p.ExecutionMs != 0.5 || p.PlanningMs != 0.1 {
		t.Errorf("removed=%v exec=%v plan=%v", p.Root.RowsRemoved, p.ExecutionMs, p.PlanningMs)
	}
	if _, ok := finding(p, "never ran"); !ok {
		t.Errorf("want the never-ran note:\n%s", titles(p))
	}

	byt, err := ParsePostgresText([]string{"Limit", "  ->  Sort", "        Sort Key: o.total DESC",
		"        ->  Nested Loop", "              ->  Seq Scan on users u", "                    Filter: (u.age > 30)",
		"              ->  Point Get on orders o", "                    Key: (o.id = 3)"}, Bytdb)
	if err != nil {
		t.Fatal(err)
	}
	if got := ops(byt); got != "Limit\n Sort\n  Nested Loop\n   Seq Scan\n   Point Get" {
		t.Errorf("bytdb tree:\n%s", got)
	}
	if byt.Metric != MetricShape || byt.Root.HasCost {
		t.Errorf("no costs means the shape metric: %s", byt.Metric)
	}
}

// ---------------------------------------------------------------------------
// MySQL
// ---------------------------------------------------------------------------

func TestMySQLTree(t *testing.T) {
	b, _ := os.ReadFile("testdata/my_analyze_subq.txt")
	p, err := ParseMySQLTree(string(b))
	if err != nil {
		t.Fatal(err)
	}
	// two top-level trees gather under a synthetic root
	if p.Root.Op != "Query" || len(p.Root.Children) != 2 {
		t.Fatalf("tree:\n%s", ops(p))
	}
	sel := p.Root.Children[1]
	if sel.Op != "Select #2" || sel.Kind != KindSubquery || !sel.HasFlag("dependent") {
		t.Errorf("subquery step: %+v", sel)
	}
	// "764e-6" and "0.0364..0.211" both parse
	scan := p.Nodes()[len(p.Nodes())-1]
	if scan.Op != "Table scan" || scan.Loops != 49 || scan.ActualRows != 200000 {
		t.Errorf("scan: %+v", scan)
	}
	// a label step with no timing spans its children, so the plan's total
	// includes the subquery's 1.4 s
	if p.ExecutionMs < 1000 {
		t.Errorf("execution = %v ms, want the subquery's time included", p.ExecutionMs)
	}
	p.ResolveAliases("SELECT u.name, (SELECT max(total) FROM orders o WHERE o.user_id = u.id) m FROM users u WHERE u.id < 50")
	in, ok := finding(p, "Full scan of orders runs 49 times")
	if !ok || in.SQL != "CREATE INDEX idx_orders_user_id ON orders (user_id);" {
		t.Fatalf("want the repeated scan with a fix on the resolved table:\n%s\n%+v", titles(p), in)
	}
}

func TestMySQLTreeFilterIsTheScans(t *testing.T) {
	b, _ := os.ReadFile("testdata/my_analyze_scan.txt")
	p, err := ParseMySQLTree(string(b))
	if err != nil {
		t.Fatal(err)
	}
	// MySQL filters in a separate step above the scan; the rows it drops
	// still count against the scan
	if _, ok := finding(p, "Full scan of orders keeps 10 of 200,000 rows"); !ok {
		t.Fatalf("\n%s", titles(p))
	}
	// the misestimate on the Filter step names the table under it
	if _, ok := finding(p, "rows from orders, got 10"); !ok {
		t.Errorf("\n%s", titles(p))
	}
}

func TestMySQLTreeRefusesUnexplainable(t *testing.T) {
	if _, err := ParseMySQLTree("-> <not executable by iterator executor>\n"); err == nil {
		t.Error("the tree format's refusal must be an error, so the caller falls back")
	}
}

func TestMySQLTable(t *testing.T) {
	cols, rows := loadTable(t, "my_tabular_scan.tsv")
	p, err := ParseMySQLTable(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root.Op != "Full table scan" || p.Root.Kind != KindScan || p.Metric != MetricRows {
		t.Fatalf("root %s/%s metric %s", p.Root.Op, p.Root.Kind, p.Metric)
	}
	if in, ok := finding(p, "keeps an estimated 10% of ~199,798 rows"); !ok || in.Severity != SevWarn {
		t.Errorf("the filtered column is evidence:\n%s", titles(p))
	}

	cols, rows = loadTable(t, "my_tabular_subq.tsv")
	p, err = ParseMySQLTable(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	if in, ok := finding(p, "inside a correlated subquery"); !ok || in.Severity != SevCrit {
		t.Errorf("a scan in a dependent subquery runs per row:\n%s", titles(p))
	}

	cols, rows = loadTable(t, "my_tabular_join.tsv")
	p, err = ParseMySQLTable(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root.Op != "Nested loop join" || len(p.Root.Children) != 2 || !p.Root.Children[0].HasFlag("filesort") {
		t.Errorf("join:\n%s", ops(p))
	}
}

// ---------------------------------------------------------------------------
// SQLite
// ---------------------------------------------------------------------------

func TestSQLite(t *testing.T) {
	cols := []string{"id", "parent", "notused", "detail"}
	rows := [][]string{
		{"2", "0", "0", "MERGE (UNION)"},
		{"4", "2", "0", "LEFT"},
		{"7", "4", "216", "SCAN users"},
		{"13", "4", "0", "USE TEMP B-TREE FOR ORDER BY"},
		{"22", "2", "0", "RIGHT"},
		{"25", "22", "214", "SCAN orders USING COVERING INDEX orders_status"},
	}
	p, err := ParseSQLite(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	if got := ops(p); got != "MERGE (UNION)\n LEFT\n  SCAN\n  Temp B-tree for order by\n RIGHT\n  SCAN (index order)" {
		t.Fatalf("tree:\n%s", got)
	}
	cov := p.Nodes()[5]
	if cov.Kind != KindIndex || cov.Index != "orders_status" {
		t.Errorf("covering scan: %+v", cov)
	}

	// two table accesses side by side are one nested-loop join
	p, err = ParseSQLite(cols, [][]string{
		{"9", "0", "62", "SEARCH o USING INDEX orders_status (status=?)"},
		{"14", "0", "45", "SEARCH u USING INTEGER PRIMARY KEY (rowid=?)"},
		{"19", "0", "0", "USE TEMP B-TREE FOR GROUP BY"},
		{"30", "0", "0", "CORRELATED SCALAR SUBQUERY 1"},
		{"33", "30", "0", "SEARCH x USING AUTOMATIC COVERING INDEX (k=?)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ops(p); got != "Query\n Nested loop\n  SEARCH\n  SEARCH\n Temp B-tree for group by\n CORRELATED SCALAR SUBQUERY 1\n  SEARCH" {
		t.Fatalf("tree:\n%s", got)
	}
	p.ResolveAliases("SELECT * FROM orders o JOIN users u ON u.id = o.user_id, (SELECT 1) y JOIN things x ON 1")
	if in, ok := finding(p, "Builds an automatic index on things"); !ok || in.SQL != "CREATE INDEX idx_things_k ON things (k);" {
		t.Errorf("auto index:\n%s", titles(p))
	}
	if _, ok := finding(p, "Subquery runs once per outer row"); !ok {
		t.Errorf("correlated:\n%s", titles(p))
	}
}

// ---------------------------------------------------------------------------
// Strip, Aliases, Detect
// ---------------------------------------------------------------------------

func TestStrip(t *testing.T) {
	cases := []struct {
		in, inner string
		analyze   bool
		ok        bool
	}{
		{"SELECT 1", "SELECT 1", false, false},
		{"EXPLAIN SELECT 1", "SELECT 1", false, true},
		{"explain analyze select 1", "select 1", true, true},
		{"EXPLAIN ANALYSE VERBOSE SELECT 1", "SELECT 1", true, true},
		{"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT 1", "SELECT 1", true, true},
		{"EXPLAIN (ANALYZE false, COSTS off) SELECT 1", "SELECT 1", false, true},
		{"EXPLAIN FORMAT=TREE SELECT 1", "SELECT 1", false, true},
		{"EXPLAIN FORMAT = JSON SELECT 1", "SELECT 1", false, true},
		{"EXPLAIN QUERY PLAN SELECT 1", "SELECT 1", false, true},
		{"-- why slow?\nEXPLAIN ANALYZE SELECT 1", "SELECT 1", true, true},
		{"/* c */ explain select 1", "select 1", false, true},
		{"EXPLAIN", "EXPLAIN", false, false},
		{"explainer SELECT 1", "explainer SELECT 1", false, false},
	}
	for _, c := range cases {
		inner, analyze, ok := Strip(c.in)
		if inner != c.inner || analyze != c.analyze || ok != c.ok {
			t.Errorf("Strip(%q) = %q, %v, %v; want %q, %v, %v", c.in, inner, analyze, ok, c.inner, c.analyze, c.ok)
		}
	}
}

func TestAliases(t *testing.T) {
	got := Aliases(`SELECT * FROM orders o JOIN public.users AS u ON u.id = o.user_id
		LEFT JOIN "items" i ON i.id = o.item_id, legacy l WHERE o.note = 'FROM fake f'`)
	want := map[string]string{"orders": "orders", "o": "orders", "public.users": "public.users",
		"users": "public.users", "u": "public.users", "items": "items", "i": "items", "legacy": "legacy", "l": "legacy"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("alias %q = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if _, ok := got["fake"]; ok {
		t.Error("a FROM inside a string literal is not a table")
	}
	// two schemas' "orders": the bare name maps to neither
	amb := Aliases("SELECT * FROM a.orders x JOIN b.orders y ON x.id = y.id")
	if _, ok := amb["orders"]; ok || amb["x"] != "a.orders" || amb["y"] != "b.orders" {
		t.Errorf("ambiguous: %v", amb)
	}
}

// The schema the statement names goes back onto a Postgres relation, so the
// index suggestion lands on the right table.
func TestResolveAliasesQualifies(t *testing.T) {
	p := loadPGJSON(t, "pg_parallel_seqscan.json")
	p.ResolveAliases("SELECT * FROM shop.orders WHERE user_id = 42 ORDER BY created_at DESC LIMIT 5")
	in, _ := finding(p, "keeps 10")
	if in.SQL != "CREATE INDEX idx_orders_user_id ON shop.orders (user_id);" {
		t.Errorf("sql = %q", in.SQL)
	}
}

func TestDetect(t *testing.T) {
	text := loadLines(t, "pg_analyze_join.txt")
	res := &model.Result{Conn: "pg", Query: "EXPLAIN ANALYZE SELECT 1", Columns: []string{"QUERY PLAN"}}
	for _, l := range text {
		res.Rows = append(res.Rows, []string{l})
	}
	p, ok := Detect(res, "postgres")
	if !ok || !p.Analyzed || p.Statement != "SELECT 1" || p.Conn != "pg" {
		t.Fatalf("pg text: ok=%v %+v", ok, p)
	}

	// pgx hands json back decoded; Detect marshals the raw value back
	var decoded any
	b, _ := os.ReadFile("testdata/pg_misestimate.json")
	_ = json.Unmarshal(b, &decoded)
	res = &model.Result{Query: "EXPLAIN (FORMAT JSON) SELECT 1", Columns: []string{"QUERY PLAN"},
		Rows: [][]string{{"[map[Plan:…]]"}}, Raw: [][]any{{decoded}}}
	if p, ok = Detect(res, "postgres"); !ok || p.Root.Relation != "skew" {
		t.Fatalf("pg json via Raw: ok=%v", ok)
	}

	tree, _ := os.ReadFile("testdata/my_tree_join.txt")
	res = &model.Result{Columns: []string{"EXPLAIN"}, Rows: [][]string{{string(tree)}}}
	if p, ok = Detect(res, "mysql"); !ok || p.Engine != MySQL {
		t.Fatalf("mysql tree: ok=%v", ok)
	}

	cols, rows := loadTable(t, "my_tabular_join.tsv")
	if _, ok = Detect(&model.Result{Columns: cols, Rows: rows}, "mysql"); !ok {
		t.Fatal("mysql table")
	}
	res = &model.Result{Columns: []string{"id", "parent", "notused", "detail"}, Rows: [][]string{{"2", "0", "0", "SCAN cats"}}}
	if p, ok = Detect(res, "sqlite"); !ok || p.Engine != SQLite {
		t.Fatal("sqlite")
	}
	if _, ok = Detect(&model.Result{Columns: []string{"id", "name"}, Rows: [][]string{{"1", "x"}}}, "sqlite"); ok {
		t.Error("an ordinary result is not a plan")
	}
	if _, ok = Detect(&model.Result{Columns: []string{"QUERY PLAN"}}, "postgres"); ok {
		t.Error("an empty result is not a plan")
	}
}

// ---------------------------------------------------------------------------
// Index columns, formatting, rendering
// ---------------------------------------------------------------------------

func TestIndexColumns(t *testing.T) {
	n := &Node{Relation: "orders", Alias: "o"}
	cases := map[string]string{
		"(user_id = 42)": "user_id",
		"((status = 'paid'::text) AND (total > 400))":        "status,total",
		"((total > 400) AND (status = 'paid'::text))":        "status,total", // equality first
		"(o.user_id = u.id)":                                 "user_id",      // the other side is u's
		"(u.id = o.user_id)":                                 "user_id",
		"((a > 1) AND (b < 2))":                              "a", // one range column
		"(`dbc`.`o`.`created_at` >= '2024-01-01')":           "created_at",
		"((city)::text ~~ 'lon%'::text)":                     "city",
		"(name IS NOT NULL)":                                 "name",
		"((kind = ANY ('{a,b}'::text[])) AND (x = 'y = z'))": "kind,x",
	}
	for cond, want := range cases {
		if got := strings.Join(indexColumns(cond, n), ","); got != want {
			t.Errorf("indexColumns(%q) = %q, want %q", cond, got, want)
		}
	}
}

func TestFormatting(t *testing.T) {
	cases := map[string]string{
		FmtRows(950): "950", FmtRows(36650): "36.6k", FmtRows(1_250_000): "1.2M", FmtRows(0.333): "0.3",
		FmtCount(9799510): "9,799,510", FmtCount(-1200): "-1,200",
		FmtMs(0.84): "840 µs", FmtMs(21.63): "21.6 ms", FmtMs(3420): "3.42 s", FmtMs(125000): "2m05s",
		FmtKB(25): "25 kB", FmtKB(4300): "4.2 MB",
		FmtFactor(26.6): "×27 more", FmtFactor(0.04): "×25 fewer", FmtFactor(1.4): "",
		Bar(0, 10): "", Bar(0.01, 10): "▏", Bar(1, 4): "████", Bar(0.5, 3): "█▌",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestText(t *testing.T) {
	p := loadPGJSON(t, "pg_subplan.json")
	p.Statement = "SELECT …"
	text := p.Text(TextOptions{Width: 110, Insights: true})
	for _, want := range []string{
		"Plan · postgres · analyzed · execution 619 ms",
		"note: JIT compilation took 6.1 ms",
		"└─ Seq Scan · orders o  (user_id = u.id)",
		"✖ Full scan of orders runs 49 times  [Seq Scan · orders o]",
		"CREATE INDEX idx_orders_user_id ON orders (user_id);",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b[") {
		t.Error("no color unless asked")
	}
	if !strings.Contains(p.Text(TextOptions{Color: true}), "\x1b[") {
		t.Error("color when asked")
	}
	for _, l := range strings.Split(text, "\n") {
		if w := len([]rune(l)); w > 110 {
			t.Errorf("line of %d cells exceeds the width: %q", w, l)
		}
	}
}

// Finalize is idempotent: the db layer calls it again after adding table
// sizes and resolving aliases, and nothing may double.
func TestFinalizeIdempotent(t *testing.T) {
	p := loadPGJSON(t, "pg_cte_union.json")
	before, _ := p.JSON()
	p.Finalize()
	p.Finalize()
	after, _ := p.JSON()
	if string(before) != string(after) {
		t.Error("Finalize changed a finalized plan")
	}
}

func TestJSONDocument(t *testing.T) {
	p := loadPGJSON(t, "pg_analyze_join.json")
	b, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Headline string   `json:"headline"`
		Metrics  []string `json:"metrics"`
		Root     struct {
			Op      string             `json:"op"`
			Weights map[string]float64 `json:"weights"`
		} `json:"root"`
	}
	if err = json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Root.Op != "Sort" || len(doc.Metrics) != 4 || doc.Root.Weights["time"] <= 0 || !strings.HasPrefix(doc.Headline, "Plan · postgres") {
		t.Errorf("doc = %+v", doc)
	}
}
