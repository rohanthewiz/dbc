package db

import (
	"context"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

func TestTablesQueryPerDriver(t *testing.T) {
	cases := []struct {
		driver string
		want   string // a fragment only that driver's catalog query has
	}{
		{"postgres", "pg_matviews"},
		{"pg", "pg_matviews"},
		{"mysql", "DATABASE()"},
		{"mariadb", "DATABASE()"},
		{"sqlite", "sqlite_master"},
		{"sqlite3", "sqlite_master"},
		{"bytdb", "information_schema.tables"},
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
	// bytdb has no pg_matviews, so the Postgres query would fail there
	if q, _ := TablesQuery("bytdb"); strings.Contains(q, "pg_matviews") {
		t.Errorf("bytdb's catalog query reads pg_matviews:\n%s", q)
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

func TestMentionedTables(t *testing.T) {
	x := NewTableIndex([]TableRef{
		{Schema: "main", Name: "cats"},
		{Schema: "main", Name: "owners"},
		{Schema: "main", Name: "Adoptions"},
		{Schema: "main", Name: "name"},
	})
	names := func(refs []TableRef) string {
		var s []string
		for _, r := range refs {
			s = append(s, r.Name)
		}
		return strings.Join(s, ",")
	}
	cases := []struct {
		sql, prose, want string
	}{
		// order of first mention, each once, case-insensitive
		{"SELECT * FROM owners o JOIN CATS c ON c.owner_id = o.id JOIN cats", "", "owners,cats"},
		// strings and comments are not mentions; a quoted identifier is
		{"SELECT 'cats' -- owners\nFROM \"Adoptions\"", "", "Adoptions"},
		// alias.column never falls back to a table named after the column
		{"SELECT c.name FROM cats c", "", "cats"},
		// qualified names match as schema.table, with or without quotes
		{`SELECT * FROM main.owners, "main"."cats"`, "", "owners,cats"},
		// prose: apostrophes are not strings, a trailing period is not a qualifier
		{"", "what's the oldest of the cats.", "cats"},
		// the statement's tables come before the question's
		{"SELECT 1 FROM owners", "join cats to owners", "owners,cats"},
		{"SELECT 1", "hello", ""},
	}
	for _, c := range cases {
		if got := names(x.Mentioned(c.sql, c.prose)); got != c.want {
			t.Errorf("Mentioned(%q, %q) = %q, want %q", c.sql, c.prose, got, c.want)
		}
	}

	// with more than one schema, a bare name matches every schema's table
	// and Display qualifies it
	x = NewTableIndex([]TableRef{{Schema: "public", Name: "cats"}, {Schema: "audit", Name: "cats"}})
	got := x.Mentioned("SELECT * FROM cats", "")
	if len(got) != 2 || x.Display(got[0]) != "public.cats" {
		t.Errorf("bare name across schemas = %v", got)
	}
	if got := x.Mentioned("SELECT * FROM mydb.audit.cats", ""); len(got) != 1 || got[0].Schema != "audit" {
		t.Errorf("db.schema.table = %v", got)
	}
	var nilIdx *TableIndex
	if nilIdx.Mentioned("SELECT * FROM cats", "") != nil {
		t.Error("no catalog means no mentions")
	}
}

func TestColumnsQueryPerDriver(t *testing.T) {
	tables := []TableRef{{Schema: "public", Name: "cats"}, {Schema: "public", Name: `o'dd\name`}}
	cases := []struct{ driver, want string }{
		{"postgres", "format_type"},
		{"bytdb", "format_type"},
		{"mysql", "column_type"},
		{"sqlite", "pragma_table_info"},
	}
	for _, c := range cases {
		q, err := ColumnsQuery(c.driver, tables)
		if err != nil {
			t.Errorf("ColumnsQuery(%q): %v", c.driver, err)
			continue
		}
		if !strings.Contains(q, c.want) || !strings.Contains(q, "'cats'") {
			t.Errorf("ColumnsQuery(%q):\n%s", c.driver, q)
		}
		// a quote in a name is doubled, never closes the literal
		if !strings.Contains(q, `'o''dd\`) {
			t.Errorf("ColumnsQuery(%q) does not escape the quote:\n%s", c.driver, q)
		}
	}
	if q, _ := ColumnsQuery("mysql", tables); !strings.Contains(q, `'o''dd\\name'`) {
		t.Errorf("mysql should double the backslash too:\n%s", q)
	}
	if _, err := ColumnsQuery("sqlite", nil); err == nil {
		t.Error("no tables should be an error, not an empty IN ()")
	}
	if _, err := ColumnsQuery("cassandra", tables); err == nil {
		t.Error("an unknown driver should not yield a columns query")
	}
}

// The column lookups have to run for real: catalog SQL is exactly the kind
// that reads fine and fails in the database.
func TestColumnsOnSqlite(t *testing.T) {
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{
			Name: "demo", Driver: "sqlite",
			DSN: "file:columnstest?mode=memory&cache=shared",
		}},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()
	if err := SeedDemo(mgr, "demo"); err != nil {
		t.Fatalf("seed demo: %v", err)
	}
	if _, err := mgr.Run("demo", "CREATE VIEW IF NOT EXISTS old_cats AS SELECT id, name FROM cats WHERE age > 4"); err != nil {
		t.Fatalf("create view: %v", err)
	}
	cols, err := mgr.Columns(context.Background(), "demo", []TableRef{
		{Schema: "main", Name: "cats"}, {Schema: "main", Name: "old_cats", View: true}, {Schema: "main", Name: "gone"},
	})
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	if len(cols) != 3 || len(cols[0]) == 0 || cols[0][0].Name != "id" || cols[0][0].Type == "" {
		t.Fatalf("cats columns = %v", cols)
	}
	if len(cols[1]) != 2 || cols[1][1].Name != "name" {
		t.Errorf("a view's columns = %v", cols[1])
	}
	if cols[2] != nil {
		t.Errorf("a table the catalog lacks should have no columns, got %v", cols[2])
	}
}

func TestColumnsOnBytdb(t *testing.T) {
	mgr := bytdbMgr(t)
	if _, err := mgr.Run("bd", "CREATE TABLE cats (id int PRIMARY KEY, name text, age int)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := mgr.Run("bd", "CREATE TABLE owners (id int PRIMARY KEY, email text)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := mgr.Run("bd", "CREATE VIEW old_cats AS SELECT id, name FROM cats WHERE age > 4"); err != nil {
		t.Fatalf("create view: %v", err)
	}
	cols, err := mgr.Columns(context.Background(), "bd", []TableRef{
		{Schema: "public", Name: "cats"}, {Schema: "public", Name: "old_cats", View: true},
	})
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	got := []string{}
	for _, c := range cols[0] {
		got = append(got, c.Name+" "+c.Type)
	}
	if len(got) != 3 || !strings.HasPrefix(got[0], "id ") || !strings.HasPrefix(got[2], "age ") {
		t.Errorf("cats columns = %v", got)
	}
	for _, g := range got {
		if strings.HasSuffix(g, " ") {
			t.Errorf("column without a type: %q", g)
		}
	}
	// a view describes its output columns, as a table does
	if len(cols[1]) != 2 || cols[1][0].Name != "id" || cols[1][1].Name != "name" {
		t.Errorf("old_cats columns = %v", cols[1])
	}
}
