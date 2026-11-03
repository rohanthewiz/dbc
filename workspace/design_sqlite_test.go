package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/designer"
)

// TestDesignerSQLitePrefixIndex: what the designer says of a starts-with
// on SQLite, checked against SQLite's own planner. SQLite's LIKE ignores
// the case of A–Z, so it turns LIKE 'r1%' into a range only on an index
// kept in COLLATE NOCASE; a plain (BINARY) one is scanned past. The card's
// ⚡ (ColView.NoPrefix) must say the same as EXPLAIN QUERY PLAN for the
// designer's own SQL, for:
//
//	a plain index                        NoPrefix, the plan reads every row
//	the filter rule's fix (NOCASE)       served: SEARCH … (ref>? AND ref<?)
//	… with an escaped value (ESCAPE '!') served too
//	a plain index on a NOCASE column     served (the key takes its collation)
//
// It runs on the driver dbc ships (modernc), so a LIKE the driver put in
// place of the built-in one — which SQLite would never turn into a range —
// shows here. SQLite keeps no row estimates, so the size rules are quiet;
// the rule's fix is read from a catalog given one by hand.
func TestDesignerSQLitePrefixIndex(t *testing.T) {
	cfg := &config.Config{
		MaxRows: 1000, MaxDisplayRows: 2000, AIContextRows: config.DefaultAIContextRows,
		DefaultConnection: demo,
		Connections:       []config.Connection{{Name: demo, Driver: "sqlite", DSN: memDSN()}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	exec := func(stmts ...string) {
		t.Helper()
		for _, s := range stmts {
			if _, err := mgr.Run(demo, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	exec(`CREATE TABLE qd_orders (id INTEGER PRIMARY KEY, status TEXT, ref VARCHAR(40), code TEXT COLLATE NOCASE)`,
		`INSERT INTO qd_orders VALUES (1, 'open', 'r1', 'C1'), (2, 'open', 'R10', 'c2'), (3, 'shut', 'x_1', 'd3')`)
	w := New(cfg, mgr, nil, Options{})
	t.Cleanup(w.Close)
	if ev := w.Connect(demo).Job().(*Connected); ev.Err != nil {
		t.Fatalf("connect: %v", ev.Err)
	}

	ctx := context.Background()
	keys := []designer.TableKey{{Schema: "main", Name: "qd_orders"}}
	catalog := func() *designer.Catalog {
		t.Helper()
		// the indexes change under the test: read them afresh each time
		w.ForgetDesignCatalog()
		cat, err := w.DesignCatalog(ctx, keys)
		if err != nil {
			t.Fatal(err)
		}
		if !cat.IndexesKnown {
			t.Fatal("the indexes were not read")
		}
		return cat
	}
	d, _, err := designer.AddTable(designer.Design{}, catalog(), "main", "qd_orders", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	o := d.Tables[0].ID
	d.Columns = []designer.Output{{Ref: designer.Ref{Table: o, Column: "id"}}}
	starts := func(col, v string) designer.Design {
		k := d
		k.Filters = []designer.Filter{{Ref: designer.Ref{Table: o, Column: col}, Op: designer.OpStarts, Values: []string{v}}}
		return k
	}
	card := func(a *designer.Analysis, col string) designer.ColView {
		t.Helper()
		for _, cv := range a.Tables[0].Cols {
			if cv.Name == col {
				return cv
			}
		}
		t.Fatalf("no %s on the card", col)
		return designer.ColView{}
	}
	// searched reports whether SQLite's plan for sql looks rows up by a
	// range on col (SEARCH … (col>? AND col<?)) rather than reading them
	// all. A plain index can still show as SCAN … USING COVERING INDEX —
	// read whole, in its order — which is no lookup.
	searched := func(sql, col string) bool {
		t.Helper()
		res, err := mgr.Run(demo, "EXPLAIN QUERY PLAN "+sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		for _, r := range res.Rows {
			if detail := r[len(r)-1]; strings.HasPrefix(detail, "SEARCH") && strings.Contains(detail, col+">?") {
				return true
			}
		}
		return false
	}
	rows := func(sql string) int {
		t.Helper()
		res, err := mgr.Run(demo, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return len(res.Rows)
	}

	// a plain index: = and ranges can use it, a starts-with cannot
	exec(`CREATE INDEX qd_orders_ref_plain ON qd_orders (ref)`)
	a := designer.Analyze(starts("ref", "r1"), catalog())
	if cv := card(a, "ref"); !cv.Indexed || !cv.NoPrefix || cv.NoRange {
		t.Errorf("a plain index's card: %+v", cv)
	}
	if searched(a.SQL, "ref") {
		t.Errorf("the planner finds the prefix on a BINARY index, the card says it cannot:\n%s", a.SQL)
	}
	// LIKE ignores case: 'r1' finds r1 and R10
	if n := rows(a.SQL); n != 2 {
		t.Errorf("%d rows, want 2:\n%s", n, a.SQL)
	}

	// the rule's fix, given a size to speak at, run as written
	cat := catalog()
	cat.Rows = map[designer.TableKey]int64{keys[0]: 2_000_000}
	var fix string
	for _, l := range designer.Analyze(starts("ref", "r1"), cat).Lints {
		if l.Rule == "filter-no-index" && len(l.Fixes) > 0 {
			fix = l.Fixes[0].SQL
		}
	}
	if !strings.Contains(fix, "(ref COLLATE NOCASE)") {
		t.Fatalf("the rule's fix: %q", fix)
	}
	exec(fix)
	cat = catalog()
	a = designer.Analyze(starts("ref", "r1"), cat)
	if cv := card(a, "ref"); !cv.Indexed || cv.NoPrefix || cv.NoRange {
		t.Errorf("with the NOCASE index, the card: %+v", cv)
	}
	if !searched(a.SQL, "ref") {
		t.Errorf("the planner does not use the NOCASE index for the prefix:\n%s", a.SQL)
	}
	if n := rows(a.SQL); n != 2 {
		t.Errorf("%d rows through the index, want 2:\n%s", n, a.SQL)
	}
	cat.Rows = map[designer.TableKey]int64{keys[0]: 2_000_000}
	for _, l := range designer.Analyze(starts("ref", "r1"), cat).Lints {
		if l.Rule == "filter-no-index" {
			t.Errorf("after its own fix: %+v", l)
		}
	}
	// an escaped value (ESCAPE '!') keeps the range: SQLite needs the
	// escape to be one ASCII character, which ! is
	a = designer.Analyze(starts("ref", "x_"), catalog())
	if !strings.Contains(a.SQL, "ESCAPE '!'") || !searched(a.SQL, "ref") || rows(a.SQL) != 1 {
		t.Errorf("an escaped prefix is not looked up by the NOCASE index:\n%s", a.SQL)
	}

	// a column declared COLLATE NOCASE: a plain index on it is NOCASE too
	exec(`CREATE INDEX qd_orders_code ON qd_orders (code)`)
	a = designer.Analyze(starts("code", "c"), catalog())
	if cv := card(a, "code"); !cv.Indexed || cv.NoPrefix || cv.NoRange {
		t.Errorf("a NOCASE column's card: %+v", cv)
	}
	if !searched(a.SQL, "code") || rows(a.SQL) != 2 {
		t.Errorf("the planner does not use the NOCASE column's index:\n%s", a.SQL)
	}

	// N-242: a key in another collation than its column's own. plan
	// reports whether SQLite's plan for sql has a line with want in it.
	plan := func(sql, want string) bool {
		t.Helper()
		res, err := mgr.Run(demo, "EXPLAIN QUERY PLAN "+sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		for _, r := range res.Rows {
			if strings.Contains(r[len(r)-1], want) {
				return true
			}
		}
		return false
	}
	// the NOCASE index alone on the BINARY ref: the starts-with only —
	// = is a SCAN and ORDER BY a temp b-tree, as the card now says
	exec(`DROP INDEX qd_orders_ref_plain`)
	a = designer.Analyze(starts("ref", "r1"), catalog())
	if cv := card(a, "ref"); !cv.Indexed || cv.NoPrefix || !cv.NoRange || !cv.NoEq {
		t.Errorf("the NOCASE index alone, the card: %+v", cv)
	}
	if !searched(a.SQL, "ref") {
		t.Errorf("the NOCASE index alone does not find the prefix:\n%s", a.SQL)
	}
	if plan(`SELECT id FROM qd_orders WHERE ref = 'r1'`, "SEARCH") ||
		!plan(`SELECT id FROM qd_orders ORDER BY ref`, "TEMP B-TREE FOR ORDER BY") {
		t.Errorf("the planner serves = or a sort from the NOCASE index on a BINARY column")
	}
	// and the reverse: BINARY named on the NOCASE code serves nothing —
	// not =, which compares in NOCASE, nor the LIKE, which wants NOCASE
	exec(`DROP INDEX qd_orders_code`, `CREATE INDEX qd_orders_code_bin ON qd_orders (code COLLATE BINARY)`)
	a = designer.Analyze(starts("code", "c"), catalog())
	if cv := card(a, "code"); !cv.Indexed || !cv.NoPrefix || !cv.NoRange || !cv.NoEq {
		t.Errorf("a BINARY index on a NOCASE column, the card: %+v", cv)
	}
	if searched(a.SQL, "code") || plan(`SELECT id FROM qd_orders WHERE code = 'c1'`, "SEARCH") {
		t.Errorf("the planner uses the BINARY index on the NOCASE column")
	}
}
