package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
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
		// relkind 'm': information_schema.tables leaves it out, so this
		// checks TablesQuery reads pg_class rather than that view (N-042)
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

	// a sidebar loaded one schema at a time (SetSchemas) still reaches a
	// table in another — a quoted mixed-case one included, which must keep
	// its case to be found (N-081)
	part := NewTableIndex([]TableRef{{Schema: "dbc_live2", Name: "cats"}}).
		SetSchemas([]string{"dbc_live", "dbc_live2"})
	refs := part.Mentioned(`SELECT * FROM dbc_live."MixedCase" JOIN dbc_live.CATS USING (id)`, "")
	cols, err := mgr.Columns(context.Background(), "live", refs)
	if err != nil {
		t.Fatalf("Columns (unloaded schema): %v", err)
	}
	if len(refs) != 2 || len(cols) != 2 || len(cols[0]) != 2 || len(cols[1]) != 6 {
		t.Errorf("unloaded schema: refs %v, columns %v", refs, cols)
	}

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
	// "Show columns": information_schema.columns for a table, and the
	// pg_attribute branch for the matview the view leaves out. The two
	// branches are UNIONed, so this is also the check that their types
	// line up on a real server.
	info := infoRows(t, mgr, "live", "postgres", TableRef{Schema: "dbc_live", Name: "cats"})
	if len(info) != 6 || !strings.HasPrefix(info[1], "2 | name | character varying | NO | NULL | 80") {
		t.Errorf("cats info columns = %q", info)
	}
	info = infoRows(t, mgr, "live", "postgres", TableRef{Schema: "dbc_live", Name: "cat_names"})
	if len(info) != 2 || !strings.HasPrefix(info[1], "2 | name | character varying(80) | YES") {
		t.Errorf("cat_names (matview) info columns = %q", info)
	}
}

// The sidebar's catalog on a shared Postgres database, read as a role that
// is not the owner of everything — the case that went wrong in real use:
//
//   - a schema whose tables belong to another role and were never granted
//     listed as empty under information_schema.tables; pg_class lists them
//   - a schema with no tables at all is still in the schema list
//   - a catalog bigger than max_rows (10 here) comes back whole, where Run
//     used to cut it, losing the schemas sorted after the cut
func TestLiveCatalogPostgres(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	u, err := url.Parse(os.Getenv("DBC_LIVE_PG_DSN"))
	if err != nil || u.Scheme == "" {
		t.Skip("needs a URL-form DBC_LIVE_PG_DSN, to log in as a second role")
	}
	drop := []string{
		`DROP SCHEMA IF EXISTS dbc_live_cat_a CASCADE`,
		`DROP SCHEMA IF EXISTS dbc_live_cat_locked CASCADE`,
		`DROP SCHEMA IF EXISTS dbc_live_cat_empty CASCADE`,
		`DROP ROLE IF EXISTS dbc_live_reader`,
	}
	liveExec(t, mgr, drop...)
	t.Cleanup(func() {
		for _, s := range drop {
			_, _ = mgr.Run("live", s)
		}
	})
	liveExec(t, mgr,
		`CREATE ROLE dbc_live_reader LOGIN PASSWORD 'pw'`,
		`CREATE SCHEMA dbc_live_cat_a`,
		`GRANT USAGE ON SCHEMA dbc_live_cat_a TO dbc_live_reader`,
		// USAGE on the schema, but no grant on its table
		`CREATE SCHEMA dbc_live_cat_locked`,
		`GRANT USAGE ON SCHEMA dbc_live_cat_locked TO dbc_live_reader`,
		`CREATE TABLE dbc_live_cat_locked.orders (id int)`,
		`CREATE SCHEMA dbc_live_cat_empty`,
	)
	for i := range 12 {
		liveExec(t, mgr, fmt.Sprintf(`CREATE TABLE dbc_live_cat_a.t%02d (id int)`, i))
	}
	liveExec(t, mgr, `GRANT SELECT ON ALL TABLES IN SCHEMA dbc_live_cat_a TO dbc_live_reader`)

	u.User = url.UserPassword("dbc_live_reader", "pw")
	reader := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres", func(c *config.Config) {
		c.MaxRows = 10
		c.Connections[0].DSN = u.String()
	})
	res, err := reader.Catalog(context.Background(), "live", "")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if res.Truncated {
		t.Error("catalog cut short")
	}
	per := map[string]int{}
	for _, r := range TableRefs(res.Rows) {
		per[r.Schema]++
	}
	if per["dbc_live_cat_a"] != 12 {
		t.Errorf("dbc_live_cat_a has %d tables listed, want all 12 despite max_rows = 10", per["dbc_live_cat_a"])
	}
	if per["dbc_live_cat_locked"] != 1 {
		t.Errorf("dbc_live_cat_locked has %d tables listed, want its 1 ungranted table", per["dbc_live_cat_locked"])
	}
	// one schema's tables, as the sidebar loads them
	one, err := reader.Catalog(context.Background(), "live", "dbc_live_cat_locked")
	if err != nil || len(one.Rows) != 1 || one.Rows[0][1] != "orders" {
		t.Errorf("dbc_live_cat_locked alone = %v, %v", one, err)
	}
	schemas, err := reader.SchemaSummary(context.Background(), "live")
	if err != nil {
		t.Fatalf("schemas: %v", err)
	}
	got := map[string]SchemaInfo{}
	for _, s := range schemas {
		got[s.Name] = s
		if strings.HasPrefix(s.Name, "pg_") || s.Name == "information_schema" {
			t.Errorf("system schema %s listed", s.Name)
		}
	}
	for name, n := range map[string]int{"dbc_live_cat_a": 12, "dbc_live_cat_locked": 1, "dbc_live_cat_empty": 0} {
		if s, ok := got[name]; !ok || s.Tables != n {
			t.Errorf("schema %s = %+v (listed %v), want %d tables", name, s, ok, n)
		}
	}
	if !got["public"].Default {
		t.Errorf("public should be the search_path's first schema: %+v", schemas)
	}
	// the names-only fallback lists the same schemas, default and all,
	// with every count unknown
	names, err := reader.SchemaNames(context.Background(), "live")
	if err != nil || len(names) != len(schemas) {
		t.Fatalf("schema names = %+v, %v; want the summary's %d schemas", names, err, len(schemas))
	}
	for i, s := range names {
		if s.Name != schemas[i].Name || s.Default != schemas[i].Default || s.Tables != TablesUnknown {
			t.Errorf("schema name %+v, want %+v without its count", s, schemas[i])
		}
	}
	// the ungranted table is listed, and is what Postgres refuses to read —
	// a count of it is simply left out, not a failed counting
	counts, err := reader.RowCounts(context.Background(), "live", TableRefs(res.Rows))
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if _, ok := counts[TableRef{Schema: "dbc_live_cat_a", Name: "t00"}]; !ok {
		t.Error("a granted table should have a count")
	}
}

// A second database on the same server: listed by Databases (the one the
// DSN opens marked current), and reached through the derived connection
// "live/<database>" — the same server and credentials, another pool.
func TestLiveDatabasesPostgres(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	const other = "dbc_live_other"
	drop := `DROP DATABASE IF EXISTS ` + other + ` WITH (FORCE)`
	liveExec(t, mgr, drop, `CREATE DATABASE `+other)
	t.Cleanup(func() {
		mgr.Drop("live") // closes live/dbc_live_other's pool too, so the drop can go
		_, _ = mgr.Run("live", drop)
	})

	dbs, err := mgr.Databases(context.Background(), "live")
	if err != nil {
		t.Fatalf("databases: %v", err)
	}
	var current, found bool
	for _, d := range dbs {
		if d.Name == other {
			found = !d.Current
		}
		current = current || d.Current
		if d.Name == "template0" || d.Name == "template1" {
			t.Errorf("template %s listed", d.Name)
		}
	}
	if !found || !current {
		t.Errorf("databases = %+v, want %s (not current) and one current", dbs, other)
	}

	derived := config.DerivedName("live", other)
	liveExec(t, mgr, `SELECT 1`) // the base pool is open before the derived one
	if _, err := mgr.Run(derived, `CREATE TABLE only_here (id int)`); err != nil {
		t.Fatalf("create on %s: %v", derived, err)
	}
	res, err := mgr.Run(derived, `SELECT current_database()`)
	if err != nil || res.Rows[0][0] != other {
		t.Fatalf("%s is on %v (%v), want %s", derived, res, err, other)
	}
	cat, err := mgr.Catalog(context.Background(), derived, "public")
	if err != nil || len(cat.Rows) != 1 || cat.Rows[0][1] != "only_here" {
		t.Errorf("%s catalog = %v, %v", derived, cat, err)
	}
	// the base still opens its own database
	if res, err := mgr.Run("live", `SELECT current_database()`); err != nil || res.Rows[0][0] == other {
		t.Errorf("live is on %v (%v)", res, err)
	}
	if DefaultDatabase(mustConn(t, mgr, "live")) == other {
		t.Error("the base's default database should be its DSN's")
	}
}

// The same on MySQL (N-079): its server's databases listed, the server's own
// schemas left out, the DSN's marked current; a derived "live/<database>"
// lands on its database — DATABASE() and the sidebar's catalog both — while
// the base stays on its own; and the derived pool also opens through the
// TLS connector, the other route a MySQL pool takes (DBC_LIVE_MYSQL_DSN's
// server has TLS on by default, as mysql:8.4 does).
func TestLiveDatabasesMySQL(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
	const other = "dbc_live_other"
	drop := `DROP DATABASE IF EXISTS ` + other
	liveExec(t, mgr, drop, `CREATE DATABASE `+other)
	t.Cleanup(func() {
		mgr.Drop("live")
		_, _ = mgr.Run("live", drop)
	})
	base := DefaultDatabase(mustConn(t, mgr, "live"))
	if base == "" || base == other {
		t.Fatalf("the DSN's database is %q; the test wants a DSN naming one", base)
	}

	dbs, err := mgr.Databases(context.Background(), "live")
	if err != nil {
		t.Fatalf("databases: %v", err)
	}
	var found, current bool
	for _, d := range dbs {
		switch d.Name {
		case other:
			found = !d.Current
		case base:
			current = d.Current
		case "information_schema", "mysql", "performance_schema", "sys":
			t.Errorf("the server's own %s listed", d.Name)
		}
	}
	if !found || !current {
		t.Errorf("databases = %+v, want %s (not current) and %s current", dbs, other, base)
	}

	derived := config.DerivedName("live", other)
	liveExec(t, mgr, `SELECT 1`) // the base pool is open before the derived one
	if _, err := mgr.Run(derived, `CREATE TABLE only_here (id int)`); err != nil {
		t.Fatalf("create on %s: %v", derived, err)
	}
	if res, err := mgr.Run(derived, `SELECT DATABASE()`); err != nil || res.Rows[0][0] != other {
		t.Fatalf("%s is on %v (%v), want %s", derived, res, err, other)
	}
	cat, err := mgr.Catalog(context.Background(), derived, "")
	if err != nil || len(cat.Rows) != 1 || cat.Rows[0][1] != "only_here" {
		t.Errorf("%s catalog = %v, %v", derived, cat, err)
	}
	// the derived connection's own listing marks its database current
	dbs, err = mgr.Databases(context.Background(), derived)
	if err != nil || !slices.ContainsFunc(dbs, func(d DatabaseInfo) bool { return d.Name == other && d.Current }) {
		t.Errorf("databases on %s = %+v, %v", derived, dbs, err)
	}
	if res, err := mgr.Run("live", `SELECT DATABASE()`); err != nil || res.Rows[0][0] != base {
		t.Errorf("live is on %v (%v), want %s", res, err, base)
	}

	// the TLS route: a connector with both the TLS config and the database
	tlsMgr := liveTLS(t, "DBC_LIVE_MYSQL_DSN", "mysql", config.TLSOpts{TLS: "require"})
	res, err := tlsMgr.Run(derived, `SELECT DATABASE()`)
	if err != nil || res.Rows[0][0] != other {
		t.Fatalf("%s over TLS is on %v (%v), want %s", derived, res, err, other)
	}
	if res, err := tlsMgr.Run(derived, `SHOW SESSION STATUS LIKE 'Ssl_cipher'`); err != nil || res.Rows[0][1] == "" {
		t.Errorf("%s over TLS has no cipher: %v (%v)", derived, res, err)
	}
	tlsMgr.Drop("live")
}

func mustConn(t *testing.T, mgr *Manager, name string) config.Connection {
	t.Helper()
	cc, ok := mgr.cfg.ConnByName(name)
	if !ok {
		t.Fatalf("no connection %s", name)
	}
	return cc
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

	// "Show columns": MySQL 8 names information_schema's columns in upper
	// case; infoRows checks the aliases bring them back to lower
	info := infoRows(t, mgr, "live", "mysql", TableRef{Name: "dbc_live_cats"})
	if len(info) != 6 || !strings.HasPrefix(info[1], "2 | name | varchar | NO | NULL | 80") {
		t.Errorf("dbc_live_cats info columns = %q", info)
	}
}

// TestLiveSchemaPostgres reads petsDDL back for a diagram: pg_constraint's
// attnum arrays against a real server, including a dropped column's gap in
// attnum, and a same-named table in a second schema that must stay apart.
func TestLiveSchemaPostgres(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	drop := []string{`DROP SCHEMA IF EXISTS dbc_erd CASCADE`, `DROP SCHEMA IF EXISTS dbc_erd2 CASCADE`}
	liveExec(t, mgr, drop...)
	t.Cleanup(func() {
		for _, s := range drop {
			_, _ = mgr.Run("live", s)
		}
	})
	liveExec(t, mgr, `CREATE SCHEMA dbc_erd`, `CREATE SCHEMA dbc_erd2`)
	// qualified rather than SET search_path: that is per session, and Run
	// takes whichever pooled connection is free
	for _, s := range petsDDL {
		s = strings.NewReplacer("TABLE ", "TABLE dbc_erd.", "REFERENCES ", "REFERENCES dbc_erd.").Replace(s)
		liveExec(t, mgr, s)
	}
	liveExec(t, mgr,
		`ALTER TABLE dbc_erd.cats ADD COLUMN gone int`,
		`ALTER TABLE dbc_erd.cats DROP COLUMN gone`,
		`ALTER TABLE dbc_erd.cats ADD COLUMN vet_id int REFERENCES dbc_erd.owners(id)`,
		`CREATE TABLE dbc_erd2.cats (id int PRIMARY KEY)`)
	s, err := mgr.Schema(context.Background(), "live")
	if err != nil {
		t.Fatal(err)
	}
	s, _ = s.Around([]string{"dbc_erd.cats", "dbc_erd.notes"}, -1)
	checkPets(t, s, "dbc_erd.")
	cats, _ := s.Find("dbc_erd.cats")
	if c := cats.Col("vet_id"); c == nil || !c.FK {
		t.Errorf("the key on a column past the dropped one: %+v", c)
	}

	// A partitioned table: its partitions (one in the other schema, one
	// itself partitioned) are hidden, with the keys Postgres clones onto
	// them and the ones it clones to reference them.
	liveExec(t, mgr,
		`CREATE TABLE dbc_erd.weighs (cat_id int NOT NULL REFERENCES dbc_erd.cats(id), day date NOT NULL,
		   grams int, PRIMARY KEY (cat_id, day)) PARTITION BY RANGE (day)`,
		`CREATE TABLE dbc_erd.weighs_2026 PARTITION OF dbc_erd.weighs FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		`CREATE TABLE dbc_erd2.weighs_2027 PARTITION OF dbc_erd.weighs FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')
		   PARTITION BY RANGE (day)`,
		`CREATE TABLE dbc_erd2.weighs_2027h1 PARTITION OF dbc_erd2.weighs_2027 FOR VALUES FROM ('2027-01-01') TO ('2027-07-01')`,
		`CREATE TABLE dbc_erd.weigh_notes (cat_id int, day date, body text,
		   FOREIGN KEY (cat_id, day) REFERENCES dbc_erd.weighs (cat_id, day))`)
	s, err = mgr.Schema(context.Background(), "live")
	if err != nil {
		t.Fatal(err)
	}
	// the partitions are no boxes at all; were they drawn, their cloned
	// keys would hang them off cats and weigh_notes, not off weighs
	for _, n := range []string{"dbc_erd.weighs_2026", "dbc_erd2.weighs_2027", "dbc_erd2.weighs_2027h1"} {
		if _, ok := s.Find(n); ok {
			t.Errorf("partition %s is in the diagram", n)
		}
	}
	notes, _ := s.Around([]string{"dbc_erd.weigh_notes"}, 1)
	if len(notes.Tables) != 2 || len(notes.Rels) != 1 {
		t.Errorf("around weigh_notes: %d tables, %d keys (a clone to a partition?)", len(notes.Tables), len(notes.Rels))
	}
	s, _ = s.Around([]string{"dbc_erd.weighs"}, 1)
	var names, keys []string
	for _, tb := range s.Tables {
		names = append(names, tb.Label)
	}
	for _, r := range s.Rels {
		keys = append(keys, r.Child.Label+"→"+r.Parent.Label)
	}
	if strings.Join(names, " ") != "dbc_erd.cats dbc_erd.weigh_notes dbc_erd.weighs" {
		t.Errorf("around weighs: %q", names)
	}
	if strings.Join(keys, " ") != "dbc_erd.cats→dbc_erd.cats dbc_erd.weigh_notes→dbc_erd.weighs dbc_erd.weighs→dbc_erd.cats" {
		t.Errorf("keys around weighs: %q", keys)
	}

	// Scoped to dbc_erd2 (completion's read of a catalog too big for the
	// whole): its one table, its partitions still hidden, and nothing of
	// dbc_erd — the SQL runs on a real server, the IN list included.
	s, err = mgr.SchemaIn(context.Background(), "live", []string{"dbc_erd2"})
	if err != nil {
		t.Fatal(err)
	}
	names = nil
	for _, tb := range s.Tables {
		names = append(names, tb.Schema+"."+tb.Name)
	}
	if strings.Join(names, " ") != "dbc_erd2.cats" {
		t.Errorf("scoped to dbc_erd2: %q", names)
	}
	// scoped to dbc_erd: weighs and its key to cats, as read whole
	s, err = mgr.SchemaIn(context.Background(), "live", []string{"dbc_erd"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Find("dbc_erd2.cats"); ok {
		t.Error("scoped to dbc_erd: dbc_erd2.cats is in it")
	}
	if _, ok := s.Find("dbc_erd.weighs_2026"); ok {
		t.Error("scoped to dbc_erd: a partition is in it")
	}
	if a, _ := s.Around([]string{"dbc_erd.weighs"}, 1); len(a.Rels) != 3 {
		t.Errorf("scoped to dbc_erd, around weighs: %d keys, want 3", len(a.Rels))
	}
}

// TestLiveSchemaMySQL reads petsDDL back through information_schema's
// key_column_usage.
func TestLiveSchemaMySQL(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
	drop := `DROP TABLE IF EXISTS dbc_erd_profiles, dbc_erd_visits, dbc_erd_cats, dbc_erd_owners, dbc_erd_notes`
	liveExec(t, mgr, drop)
	t.Cleanup(func() { _, _ = mgr.Run("live", drop) })
	for _, s := range petsDDL {
		s = strings.NewReplacer("TABLE ", "TABLE dbc_erd_", "REFERENCES ", "REFERENCES dbc_erd_").Replace(s)
		liveExec(t, mgr, s)
	}
	s, err := mgr.Schema(context.Background(), "live")
	if err != nil {
		t.Fatal(err)
	}
	checkPets(t, s, "dbc_erd_")
}

// liveRowCounts counts the connection's catalog as the sidebar does, keyed
// by schema.name for the tables whose schema starts with prefix (or, when
// prefix is "", every table).
func liveRowCounts(t *testing.T, mgr *Manager, driver, prefix string) map[string]RowCount {
	t.Helper()
	counts, err := mgr.RowCounts(context.Background(), "live", catalogRefs(t, mgr, "live", driver))
	if err != nil {
		t.Fatalf("RowCounts: %v", err)
	}
	out := map[string]RowCount{}
	for ref, c := range counts {
		if strings.HasPrefix(ref.Schema+"."+ref.Name, prefix) {
			out[ref.Schema+"."+ref.Name] = c
		}
	}
	return out
}

// On Postgres a table whose statistics say it is huge keeps the estimate;
// the rest are counted exactly, a quoted name included, and views (plain
// and materialized) are left out. The "huge" table is faked by writing
// reltuples directly, which a superuser may, rather than inserting a
// million rows.
func TestLiveRowCountsPostgres(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	drop := `DROP SCHEMA IF EXISTS dbc_live_rc CASCADE`
	liveExec(t, mgr, drop)
	t.Cleanup(func() { _, _ = mgr.Run("live", drop) })
	liveExec(t, mgr,
		`CREATE SCHEMA dbc_live_rc`,
		`CREATE TABLE dbc_live_rc.cats (id int)`,
		`INSERT INTO dbc_live_rc.cats SELECT generate_series(1, 3)`,
		`CREATE TABLE dbc_live_rc."Odd ""One""" (id int)`,
		`INSERT INTO dbc_live_rc."Odd ""One""" VALUES (1)`,
		`CREATE TABLE dbc_live_rc.big (id int)`,
		`INSERT INTO dbc_live_rc.big VALUES (1), (2)`,
		`ANALYZE dbc_live_rc.big`,
		`UPDATE pg_class SET reltuples = 5e6 WHERE oid = 'dbc_live_rc.big'::regclass`,
		`CREATE VIEW dbc_live_rc.v AS SELECT * FROM dbc_live_rc.cats`,
		`CREATE MATERIALIZED VIEW dbc_live_rc.mv AS SELECT * FROM dbc_live_rc.cats`,
	)
	got := liveRowCounts(t, mgr, "postgres", "dbc_live_rc.")
	want := map[string]RowCount{
		"dbc_live_rc.cats":      {N: 3},
		`dbc_live_rc.Odd "One"`: {N: 1},
		"dbc_live_rc.big":       {N: 5_000_000, Estimate: true},
	}
	if len(got) != len(want) {
		t.Errorf("counts = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v, want %+v", k, got[k], w)
		}
	}
}

// A partitioned parent gets its leaf partitions' estimates summed (N-073),
// through a sub-partitioned level, rather than an exact count(*) that reads
// every partition. A leaf never analyzed adds nothing to the sum, and a
// parent none of whose leaves has an estimate is counted exactly. As above,
// the big leaves are faked by writing reltuples; the rest stay at -1, the
// never-analyzed mark, since too few rows go in to wake autovacuum.
//
//	ev (p) ─┬─ ev_a  (r, 3e6)
//	        └─ ev_b (p) ─┬─ ev_b1 (r, 2e6)
//	                     └─ ev_b2 (r, -1, 2 rows)
//	small (p) ─┬─ small_a (r, -1, 2 rows)
//	           └─ small_b (r, -1, 1 row)
func TestLiveRowCountsPostgresPartitioned(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	drop := `DROP SCHEMA IF EXISTS dbc_live_rcp CASCADE`
	liveExec(t, mgr, drop)
	t.Cleanup(func() { _, _ = mgr.Run("live", drop) })
	liveExec(t, mgr,
		`CREATE SCHEMA dbc_live_rcp`,
		`CREATE TABLE dbc_live_rcp.ev (k int) PARTITION BY RANGE (k)`,
		`CREATE TABLE dbc_live_rcp.ev_a PARTITION OF dbc_live_rcp.ev FOR VALUES FROM (0) TO (10)`,
		`CREATE TABLE dbc_live_rcp.ev_b PARTITION OF dbc_live_rcp.ev FOR VALUES FROM (10) TO (20) PARTITION BY RANGE (k)`,
		`CREATE TABLE dbc_live_rcp.ev_b1 PARTITION OF dbc_live_rcp.ev_b FOR VALUES FROM (10) TO (15)`,
		`CREATE TABLE dbc_live_rcp.ev_b2 PARTITION OF dbc_live_rcp.ev_b FOR VALUES FROM (15) TO (20)`,
		`INSERT INTO dbc_live_rcp.ev VALUES (1), (11), (16), (17)`,
		`UPDATE pg_class SET reltuples = 3e6 WHERE oid = 'dbc_live_rcp.ev_a'::regclass`,
		`UPDATE pg_class SET reltuples = 2e6 WHERE oid = 'dbc_live_rcp.ev_b1'::regclass`,
		`CREATE TABLE dbc_live_rcp.small (k int) PARTITION BY LIST (k)`,
		`CREATE TABLE dbc_live_rcp.small_a PARTITION OF dbc_live_rcp.small FOR VALUES IN (1)`,
		`CREATE TABLE dbc_live_rcp.small_b PARTITION OF dbc_live_rcp.small FOR VALUES IN (2)`,
		`INSERT INTO dbc_live_rcp.small VALUES (1), (1), (2)`,
	)
	got := liveRowCounts(t, mgr, "postgres", "dbc_live_rcp.")
	want := map[string]RowCount{
		"dbc_live_rcp.ev":      {N: 5_000_000, Estimate: true},
		"dbc_live_rcp.ev_a":    {N: 3_000_000, Estimate: true},
		"dbc_live_rcp.ev_b":    {N: 2_000_000, Estimate: true},
		"dbc_live_rcp.ev_b1":   {N: 2_000_000, Estimate: true},
		"dbc_live_rcp.ev_b2":   {N: 2},
		"dbc_live_rcp.small":   {N: 3},
		"dbc_live_rcp.small_a": {N: 2},
		"dbc_live_rcp.small_b": {N: 1},
	}
	if len(got) != len(want) {
		t.Errorf("counts = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v, want %+v", k, got[k], w)
		}
	}
}

// On MySQL small tables are counted exactly (table_rows, InnoDB's sampled
// estimate, is read but only stands in above exactCountLimit) and a view is
// left out.
func TestLiveRowCountsMySQL(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
	drop := []string{
		`DROP VIEW IF EXISTS dbc_live_rc_v`,
		"DROP TABLE IF EXISTS dbc_live_rc_cats, `dbc_live_rc_odd``one`",
	}
	liveExec(t, mgr, drop...)
	t.Cleanup(func() {
		for _, s := range drop {
			_, _ = mgr.Run("live", s)
		}
	})
	liveExec(t, mgr,
		`CREATE TABLE dbc_live_rc_cats (id int)`,
		`INSERT INTO dbc_live_rc_cats VALUES (1), (2), (3)`,
		"CREATE TABLE `dbc_live_rc_odd``one` (id int)",
		"INSERT INTO `dbc_live_rc_odd``one` VALUES (1)",
		`CREATE VIEW dbc_live_rc_v AS SELECT * FROM dbc_live_rc_cats`,
	)
	got := map[string]RowCount{}
	for k, c := range liveRowCounts(t, mgr, "mysql", "") {
		if name := k[strings.Index(k, ".")+1:]; strings.HasPrefix(name, "dbc_live_rc") {
			got[name] = c
		}
	}
	want := map[string]RowCount{"dbc_live_rc_cats": {N: 3}, "dbc_live_rc_odd`one": {N: 1}}
	if len(got) != len(want) || got["dbc_live_rc_cats"] != want["dbc_live_rc_cats"] ||
		got["dbc_live_rc_odd`one"] != want["dbc_live_rc_odd`one"] {
		t.Errorf("counts = %v, want %v", got, want)
	}
}
