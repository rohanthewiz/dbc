package db

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
)

func TestRowCountWording(t *testing.T) {
	cases := []struct {
		c     RowCount
		short string
		words string
	}{
		{RowCount{N: 0}, "0", "cats with 0 rows"},
		{RowCount{N: 1}, "1", "cats with 1 row"},
		{RowCount{N: 100}, "100", "cats with 100 rows"},
		{RowCount{N: 1234}, "1,234", "cats with 1,234 rows"},
		{RowCount{N: 99_999}, "99,999", "cats with 99,999 rows"},
		// compacted from 100K on, so the column stays narrow
		{RowCount{N: 123_456}, "123K", "cats with 123,456 rows"},
		{RowCount{N: 1_234_567}, "1.2M", "cats with 1,234,567 rows"},
		{RowCount{N: 2_000_000_000}, "2B", "cats with 2,000,000,000 rows"},
		{RowCount{N: 1_250_000, Estimate: true}, "~1.2M",
			"cats with about 1,250,000 rows (estimated from the database's statistics)"},
	}
	for _, tc := range cases {
		if got := tc.c.Short(); got != tc.short {
			t.Errorf("Short(%+v) = %q, want %q", tc.c, got, tc.short)
		}
		if got := tc.c.Sentence("cats"); got != tc.words {
			t.Errorf("Sentence(%+v) = %q, want %q", tc.c, got, tc.words)
		}
	}
}

func TestCountQueryQuoting(t *testing.T) {
	odd := TableRef{Schema: `we"ird`, Name: "a`b\"c"}
	cases := map[string]string{
		"postgres": `SELECT count(*) FROM "we""ird"."a` + "`" + `b""c"`,
		"sqlite":   `SELECT count(*) FROM "we""ird"."a` + "`" + `b""c"`,
		"bytdb":    `SELECT count(*) FROM "we""ird"."a` + "`" + `b""c"`,
		"mysql":    "SELECT count(*) FROM `we\"ird`.`a``b\"c`",
	}
	for drv, want := range cases {
		got, err := CountQuery(drv, odd)
		if err != nil {
			t.Fatalf("%s: %v", drv, err)
		}
		if got != want {
			t.Errorf("%s:\n got %s\nwant %s", drv, got, want)
		}
	}
	if got, _ := CountQuery("sqlite", TableRef{Name: "cats"}); got != `SELECT count(*) FROM "cats"` {
		t.Errorf("no schema: %s", got)
	}
}

func TestRowEstimatesQueryPerDriver(t *testing.T) {
	for drv, want := range map[string]string{
		"postgres": "reltuples", "mysql": "table_rows", "sqlite": "", "bytdb": "",
	} {
		q, err := RowEstimatesQuery(drv)
		if err != nil {
			t.Fatalf("%s: %v", drv, err)
		}
		if want == "" && q != "" || want != "" && !strings.Contains(q, want) {
			t.Errorf("%s: %q", drv, q)
		}
	}
}

// sqliteCountMgr is a shared in-memory SQLite connection holding two
// tables and a view.
func sqliteCountMgr(t *testing.T, dsnName string) *Manager {
	t.Helper()
	cfg := &config.Config{MaxRows: 1000, Connections: []config.Connection{{
		Name: "demo", Driver: "sqlite", DSN: "file:" + dsnName + "?mode=memory&cache=shared",
	}}}
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)
	for _, q := range []string{
		`CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT)`,
		`INSERT INTO cats (name) VALUES ('mia'), ('otto'), ('pip')`,
		`CREATE TABLE "odd ""name""" (x INT)`,
		`INSERT INTO "odd ""name""" VALUES (1)`,
		`CREATE VIEW old_cats AS SELECT * FROM cats`,
	} {
		if _, err := mgr.Run("demo", q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return mgr
}

// catalogRefs is the connection's catalog as the sidebar gets it.
func catalogRefs(t *testing.T, mgr *Manager, conn, driver string) []TableRef {
	t.Helper()
	q, err := TablesQuery(driver)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mgr.Run(conn, q)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return TableRefs(res.Rows)
}

func TestRowCountsSqlite(t *testing.T) {
	mgr := sqliteCountMgr(t, "rowcountsqlite")
	refs := catalogRefs(t, mgr, "demo", "sqlite")
	counts, err := mgr.RowCounts(context.Background(), "demo", refs)
	if err != nil {
		t.Fatalf("RowCounts: %v", err)
	}
	byName := map[string]RowCount{}
	for ref, c := range counts {
		byName[ref.Name] = c
	}
	want := map[string]RowCount{"cats": {N: 3}, `odd "name"`: {N: 1}}
	if !reflect.DeepEqual(byName, want) {
		t.Errorf("counts = %v, want %v (the view left out)", byName, want)
	}
}

// Within the TTL a counting is served from the cache — a row inserted since
// does not show — and once it expires, or the catalog grows a table, or the
// connection is dropped, it is counted afresh.
func TestRowCountsCache(t *testing.T) {
	mgr := sqliteCountMgr(t, "rowcountcache")
	ctx := context.Background()
	refs := catalogRefs(t, mgr, "demo", "sqlite")
	cats := TableRef{Schema: "main", Name: "cats"}
	count := func(refs []TableRef) int64 {
		t.Helper()
		c, err := mgr.RowCounts(ctx, "demo", refs)
		if err != nil {
			t.Fatalf("RowCounts: %v", err)
		}
		return c[cats].N
	}
	if n := count(refs); n != 3 {
		t.Fatalf("first count = %d", n)
	}
	if _, err := mgr.Run("demo", `INSERT INTO cats (name) VALUES ('zed')`); err != nil {
		t.Fatal(err)
	}
	if n := count(refs); n != 3 {
		t.Errorf("within the TTL: %d, want the cached 3", n)
	}

	// a table the cached counting was not asked for forces a recount
	if _, err := mgr.Run("demo", `CREATE TABLE dogs (id INT)`); err != nil {
		t.Fatal(err)
	}
	refs = catalogRefs(t, mgr, "demo", "sqlite")
	if n := count(refs); n != 4 {
		t.Errorf("after a new table: %d, want a fresh 4", n)
	}

	// an expired counting is redone
	if _, err := mgr.Run("demo", `INSERT INTO cats (name) VALUES ('ava')`); err != nil {
		t.Fatal(err)
	}
	mgr.rows.mu.Lock()
	mgr.rows.cache["demo"].at = time.Now().Add(-rowCountTTL - time.Second)
	mgr.rows.mu.Unlock()
	if n := count(refs); n != 5 {
		t.Errorf("after the TTL: %d, want a fresh 5", n)
	}

	mgr.Drop("demo")
	mgr.rows.mu.Lock()
	_, cached := mgr.rows.cache["demo"]
	mgr.rows.mu.Unlock()
	if cached {
		t.Error("Drop should forget the connection's counts")
	}
}

// Callers asking at once share one counting: every one of them gets the
// very same map, which only the single-flight wait (or the cache it fills)
// can hand out.
func TestRowCountsSingleFlight(t *testing.T) {
	mgr := sqliteCountMgr(t, "rowcountflight")
	refs := catalogRefs(t, mgr, "demo", "sqlite")
	const n = 8
	got := make([]map[TableRef]RowCount, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			c, err := mgr.RowCounts(context.Background(), "demo", refs)
			if err != nil {
				t.Errorf("RowCounts: %v", err)
			}
			got[i] = c
		})
	}
	wg.Wait()
	first := reflect.ValueOf(got[0]).Pointer()
	for i, c := range got {
		if reflect.ValueOf(c).Pointer() != first {
			t.Errorf("caller %d got its own counting", i)
		}
	}
}

// A counting the caller cancels is an error, and is not cached as though
// it were the answer.
func TestRowCountsCanceledNotCached(t *testing.T) {
	mgr := sqliteCountMgr(t, "rowcountcancel")
	refs := catalogRefs(t, mgr, "demo", "sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mgr.RowCounts(ctx, "demo", refs); err == nil {
		t.Fatal("a canceled counting should fail")
	}
	mgr.rows.mu.Lock()
	_, cached := mgr.rows.cache["demo"]
	mgr.rows.mu.Unlock()
	if cached {
		t.Error("a canceled counting was cached")
	}
}

// bytdb takes the quoted, schema-qualified count(*) that CountQuery writes.
func TestRowCountsBytdb(t *testing.T) {
	mgr := bytdbMgr(t)
	for _, q := range []string{
		"CREATE TABLE cats (id int PRIMARY KEY, name text)",
		"INSERT INTO cats VALUES (1, 'mia'), (2, 'otto')",
		"CREATE TABLE empty (id int PRIMARY KEY)",
	} {
		if _, err := mgr.Run("bd", q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	refs := catalogRefs(t, mgr, "bd", "bytdb")
	counts, err := mgr.RowCounts(context.Background(), "bd", refs)
	if err != nil {
		t.Fatalf("RowCounts: %v", err)
	}
	byName := map[string]RowCount{}
	for ref, c := range counts {
		byName[ref.Name] = c
	}
	want := map[string]RowCount{"cats": {N: 2}, "empty": {N: 0}}
	if !reflect.DeepEqual(byName, want) {
		t.Errorf("counts = %v, want %v", byName, want)
	}
}
