package db

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

func TestTablesQueryPerDriver(t *testing.T) {
	cases := []struct {
		driver string
		want   string // a fragment only that driver's catalog query has
	}{
		{"postgres", "pg_catalog"},
		{"pg", "pg_catalog"},
		{"mysql", "DATABASE()"},
		{"mariadb", "DATABASE()"},
		{"sqlite", "sqlite_master"},
		{"sqlite3", "sqlite_master"},
		{"bytdb", "relkind"},
	}
	for _, c := range cases {
		q, err := TablesQuery(c.driver)
		if err != nil {
			t.Errorf("TablesQuery(%q): %v", c.driver, err)
			continue
		}
		if !strings.Contains(q, c.want) {
			t.Errorf("TablesQuery(%q) does not mention %q:\n%s", c.driver, c.want, q)
		}
		// every driver reports the same three columns, so the results table
		// reads the same whichever database is behind it
		for _, col := range []string{"table_schema", "table_name", "table_type"} {
			if !strings.Contains(q, col) {
				t.Errorf("TablesQuery(%q) is missing the %s column:\n%s", c.driver, col, q)
			}
		}
	}
	if _, err := TablesQuery("cassandra"); err == nil {
		t.Error("an unknown driver should not yield a catalog query")
	}
}

// The sqlite query has to actually run and find the seeded table — a catalog
// query is exactly the kind of SQL that compiles in the head and not in the
// database.
func TestTablesQueryRunsOnSqlite(t *testing.T) {
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{
			Name: "demo", Driver: "sqlite",
			DSN: "file:catalogtest?mode=memory&cache=shared",
		}},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()
	if err := SeedDemo(mgr, "demo"); err != nil {
		t.Fatalf("seed demo: %v", err)
	}
	if _, err := mgr.Run("demo", "CREATE VIEW IF NOT EXISTS old_cats AS SELECT * FROM cats WHERE age > 4"); err != nil {
		t.Fatalf("create view: %v", err)
	}

	q, err := TablesQuery("sqlite")
	if err != nil {
		t.Fatalf("TablesQuery: %v", err)
	}
	res, err := mgr.Run("demo", q)
	if err != nil {
		t.Fatalf("run catalog query: %v", err)
	}

	found := map[string]string{}
	for _, row := range res.Rows {
		found[row[1]] = row[2]
	}
	if found["cats"] != "TABLE" {
		t.Errorf("cats listed as %q, want TABLE — got %v", found["cats"], res.Rows)
	}
	if found["old_cats"] != "VIEW" {
		t.Errorf("old_cats listed as %q, want VIEW — got %v", found["old_cats"], res.Rows)
	}
	for name := range found {
		if strings.HasPrefix(name, "sqlite_") {
			t.Errorf("internal table %q should not be listed", name)
		}
	}
}
