package db

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// The live tests run the assistant's schema lookup against real servers,
// which the rest of the suite cannot reach. They are opt-in: each one skips
// unless its DSN is set, e.g.
//
//	DBC_LIVE_PG_DSN='postgres://postgres:pw@127.0.0.1:5432/dbc?sslmode=disable' \
//	DBC_LIVE_MYSQL_DSN='root:pw@tcp(127.0.0.1:3306)/dbc' \
//	go test ./db -run Live -v
//
// Point them at a throwaway database. They create and drop their own objects
// — the dbc_live / dbc_live2 schemas on Postgres, dbc_live_* tables on MySQL —
// and drop any leftovers of those names first, so an interrupted run does not
// wedge the next one.
//
// Each test walks the same path the assistant does, rather than calling
// ColumnsQuery in isolation: list the catalog with TablesQuery, index it,
// find the tables a statement mentions, then describe them with Columns. That
// way a catalog row the lookup cannot map back (a schema reported in another
// letter case, a name the index does not match) fails here too, not only a
// query the server rejects.

// liveMgr is a Manager on the one connection named by env, or a skip. tune,
// if given, adjusts the config first — the timeout tests set theirs there.
func liveMgr(t *testing.T, env, driver string, tune ...func(*config.Config)) *Manager {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("set %s to run against a live %s", env, driver)
	}
	cfg := &config.Config{
		MaxRows:     1000,
		Connections: []config.Connection{{Name: "live", Driver: driver, DSN: dsn}},
	}
	for _, f := range tune {
		f(cfg)
	}
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)
	return mgr
}

// liveExec runs setup statements one at a time, so a failure names its line.
func liveExec(t *testing.T, mgr *Manager, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := mgr.Run("live", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// liveLookup is the assistant's lookup: the catalog, the tables sql
// mentions, and their columns as "name type" strings, keyed by the display
// name the assistant would show.
func liveLookup(t *testing.T, mgr *Manager, driver, sql string) map[string][]string {
	t.Helper()
	q, err := TablesQuery(driver)
	if err != nil {
		t.Fatalf("TablesQuery: %v", err)
	}
	res, err := mgr.Run("live", q)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	idx := NewTableIndex(TableRefs(res.Rows))
	refs := idx.Mentioned(sql, "")
	if len(refs) == 0 {
		t.Fatalf("no tables found in %q (catalog: %v)", sql, res.Rows)
	}
	cols, err := mgr.Columns(context.Background(), "live", refs)
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	out := map[string][]string{}
	for i, r := range refs {
		var got []string
		for _, c := range cols[i] {
			got = append(got, c.Name+" "+c.Type)
		}
		out[idx.Display(r)] = got
	}
	return out
}

// wantCols compares one table's "name type" list exactly, in order.
func wantCols(t *testing.T, got map[string][]string, table string, want ...string) {
	t.Helper()
	if strings.Join(got[table], " | ") != strings.Join(want, " | ") {
		t.Errorf("%s:\n got %q\nwant %q", table, got[table], want)
	}
}

func TestLiveColumnsPostgres(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	drop := []string{
		`DROP SCHEMA IF EXISTS dbc_live CASCADE`,
		`DROP SCHEMA IF EXISTS dbc_live2 CASCADE`,
	}
	liveExec(t, mgr, drop...)
	t.Cleanup(func() {
		for _, s := range drop {
			_, _ = mgr.Run("live", s)
		}
	})
	liveExec(t, mgr,
		`CREATE SCHEMA dbc_live`,
		`CREATE SCHEMA dbc_live2`,
		`CREATE TYPE dbc_live.mood AS ENUM ('calm', 'feisty')`,
		// modifiers, an array, a user enum, and a column dropped in the
		// middle (attisdropped, and a gap in attnum)
		`CREATE TABLE dbc_live.cats (
			id serial PRIMARY KEY,
			name varchar(80) NOT NULL,
			gone int,
			weight numeric(5,2),
			tags text[],
			mood dbc_live.mood,
			born timestamptz
		)`,
		`ALTER TABLE dbc_live.cats DROP COLUMN gone`,
		// the same table name in a second schema must not merge into the first
		`CREATE TABLE dbc_live2.cats (id int, owner text)`,
		`CREATE VIEW dbc_live.old_cats AS SELECT id, name FROM dbc_live.cats`,
		// relkind 'm': information_schema.tables leaves it out, so it is
		// found only through TablesQuery's pg_matviews half (N-042)
		`CREATE MATERIALIZED VIEW dbc_live.cat_names AS SELECT id, name FROM dbc_live.cats`,
		// relkind 'p' for the parent, 'r' for the partition
		`CREATE TABLE dbc_live.events (id bigint, at date) PARTITION BY RANGE (at)`,
		`CREATE TABLE dbc_live.events_2026 PARTITION OF dbc_live.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		// a quoted mixed-case name, and a column name needing quotes
		`CREATE TABLE dbc_live."MixedCase" (id int, "Weird Col" text)`,
	)

	got := liveLookup(t, mgr, "postgres", `
		SELECT c.name, o.owner, v.id, e.at, m."Weird Col"
		FROM dbc_live.cats c
		JOIN dbc_live2.cats o USING (id)
		JOIN dbc_live.old_cats v USING (id)
		JOIN dbc_live.cat_names n USING (id)
		JOIN dbc_live.events e USING (id)
		JOIN dbc_live."MixedCase" m USING (id)`)

	// format_type qualifies the enum because dbc_live is not on the
	// search_path — which is what the model should see, too
	wantCols(t, got, "dbc_live.cats",
		"id integer", "name character varying(80)", "weight numeric(5,2)",
		"tags text[]", "mood dbc_live.mood", "born timestamp with time zone")
	wantCols(t, got, "dbc_live2.cats", "id integer", "owner text")
	wantCols(t, got, "dbc_live.old_cats", "id integer", "name character varying(80)")
	wantCols(t, got, "dbc_live.cat_names", "id integer", "name character varying(80)")
	wantCols(t, got, "dbc_live.events", "id bigint", "at date")
	wantCols(t, got, "dbc_live.MixedCase", "id integer", "Weird Col text")

	// the sidebar and the assistant both read the matview as a view
	q, _ := TablesQuery("postgres")
	res, err := mgr.Run("live", q)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	var matview *TableRef
	for _, r := range TableRefs(res.Rows) {
		if r.Schema == "dbc_live" && r.Name == "cat_names" {
			matview = &r
		}
	}
	if matview == nil || !matview.View {
		t.Errorf("dbc_live.cat_names listed as %+v, want a view (catalog: %v)", matview, res.Rows)
	}
}

func TestLiveColumnsMySQL(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
	drop := []string{
		`DROP VIEW IF EXISTS dbc_live_old_cats`,
		`DROP TABLE IF EXISTS dbc_live_cats, dbc_live_MixedCase`,
	}
	liveExec(t, mgr, drop...)
	t.Cleanup(func() {
		for _, s := range drop {
			_, _ = mgr.Run("live", s)
		}
	})
	liveExec(t, mgr,
		// column_type keeps what data_type would drop: the length, the
		// enum's values, unsigned, the scale, fractional seconds
		`CREATE TABLE dbc_live_cats (
			id int unsigned AUTO_INCREMENT PRIMARY KEY,
			name varchar(80) NOT NULL,
			mood enum('calm','feisty'),
			weight decimal(5,2),
			born datetime(3),
			meta json
		)`,
		`CREATE VIEW dbc_live_old_cats AS SELECT id, name FROM dbc_live_cats`,
		"CREATE TABLE `dbc_live_MixedCase` (id int, `Weird Col` text)",
	)

	got := liveLookup(t, mgr, "mysql",
		"SELECT c.name, v.id, m.`Weird Col` FROM dbc_live_cats c "+
			"JOIN dbc_live_old_cats v USING (id) JOIN `dbc_live_MixedCase` m USING (id)")

	wantCols(t, got, "dbc_live_cats",
		"id int unsigned", "name varchar(80)", "mood enum('calm','feisty')",
		"weight decimal(5,2)", "born datetime(3)", "meta json")
	wantCols(t, got, "dbc_live_old_cats", "id int unsigned", "name varchar(80)")
	wantCols(t, got, "dbc_live_MixedCase", "id int", "Weird Col text")
}
