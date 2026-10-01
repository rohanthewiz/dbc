package db

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

func TestTablesQueryPerDriver(t *testing.T) {
	cases := []struct {
		driver string
		want   string // a fragment only that driver's catalog query has
	}{
		{"postgres", "pg_class"},
		{"pg", "pg_class"},
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
	// information_schema.tables hides relations the user holds no
	// privilege on, which emptied whole schemas on a shared database, so
	// the Postgres listing must not go back to it
	if q, _ := TablesQuery("postgres"); strings.Contains(q, "information_schema.tables") {
		t.Errorf("the Postgres catalog query reads information_schema.tables:\n%s", q)
	}
	if _, err := TablesQuery("cassandra"); err == nil {
		t.Error("an unknown driver should not yield a catalog query")
	}
}

// Only Postgres is navigated a level at a time; for the others the
// database and schema listings are "" and the tables are listed whole.
func TestNavigatorQueriesPerDriver(t *testing.T) {
	for driver, want := range map[string]bool{
		"postgres": true, "pg": true, "mysql": false, "sqlite": false, "bytdb": false,
	} {
		if Navigable(driver) != want {
			t.Errorf("Navigable(%q) = %v, want %v", driver, !want, want)
		}
		dq, err := DatabasesQuery(driver)
		if err != nil || strings.Contains(dq, "pg_database") != want {
			t.Errorf("DatabasesQuery(%q) = %q, %v", driver, dq, err)
		}
		sq, err := SchemaSummaryQuery(driver)
		if err != nil || strings.Contains(sq, "pg_namespace") != want {
			t.Errorf("SchemaSummaryQuery(%q) = %q, %v", driver, sq, err)
		}
		tq, err := SchemaTablesQuery(driver, "sales")
		if want && (err != nil || !strings.Contains(tq, "n.nspname = 'sales'")) {
			t.Errorf("SchemaTablesQuery(%q) = %q, %v", driver, tq, err)
		}
		if !want && err == nil {
			t.Errorf("SchemaTablesQuery(%q) should refuse: its tables are listed whole", driver)
		}
	}
	// the schema is a literal, quoted like every other name inlined
	if q, _ := SchemaTablesQuery("postgres", "o'brien"); !strings.Contains(q, "'o''brien'") {
		t.Errorf("schema not escaped:\n%s", q)
	}
	if _, err := DatabasesQuery("cassandra"); err == nil {
		t.Error("an unknown driver should not yield a database query")
	}
}

// An index built from one schema's tables qualifies names by the
// database's schemas, and reports schema.table words that name a schema it
// did not load — but not an alias.column, nor a table of a loaded schema
// that does not exist.
func TestTableIndexSetSchemas(t *testing.T) {
	x := NewTableIndex([]TableRef{{Schema: "public", Name: "cats"}}).
		SetSchemas([]string{"public", "Billing", "hr"})
	if got := x.Display(TableRef{Schema: "public", Name: "cats"}); got != "public.cats" {
		t.Errorf("Display = %q, want qualified on a three-schema database", got)
	}
	got := x.Mentioned(`SELECT * FROM cats c JOIN billing.invoices i ON i.id = c.id
		JOIN public.nope n ON true JOIN hr.staff s ON true JOIN billing.invoices again ON true`, "")
	want := []TableRef{{Schema: "public", Name: "cats"}, {Schema: "Billing", Name: "invoices"}, {Schema: "hr", Name: "staff"}}
	if !slices.Equal(got, want) {
		t.Errorf("Mentioned = %v, want %v", got, want)
	}
	// a quoted name keeps its case (Postgres would not fold it), and so
	// does a quoted schema's real name; an unquoted one is folded, as
	// Postgres folds it. A dot inside quotes is part of the name.
	got = x.Mentioned(`SELECT * FROM billing."Invoices" JOIN "Billing"."Line.Items" ON true
		JOIN hr.Staff ON true JOIN mydb.billing."Invoices" ON true`, "")
	want = []TableRef{{Schema: "Billing", Name: "Invoices"}, {Schema: "Billing", Name: "Line.Items"},
		{Schema: "hr", Name: "staff"}}
	if !slices.Equal(got, want) {
		t.Errorf("Mentioned (quoted) = %v, want %v", got, want)
	}
	// prose has no quoting to read: folded
	if got = x.Mentioned("", "how many rows in billing.Invoices?"); !slices.Equal(got,
		[]TableRef{{Schema: "Billing", Name: "invoices"}}) {
		t.Errorf("Mentioned (prose) = %v", got)
	}
	// with every schema loaded there is nothing to guess
	full := NewTableIndex([]TableRef{{Schema: "public", Name: "cats"}}).SetSchemas([]string{"public"})
	if got := full.Mentioned("SELECT 1 FROM c.x", ""); len(got) != 0 {
		t.Errorf("Mentioned = %v, want nothing", got)
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

func TestInfoColumnsQueryPerDriver(t *testing.T) {
	odd := TableRef{Schema: "public", Name: `o'dd\name`}
	cases := []struct{ driver, want string }{
		{"postgres", "information_schema.columns"},
		{"bytdb", "information_schema.columns"},
		{"mysql", "information_schema.columns"},
		{"sqlite", "pragma_table_info"},
	}
	for _, c := range cases {
		q, err := InfoColumnsQuery(c.driver, odd)
		if err != nil {
			t.Errorf("InfoColumnsQuery(%q): %v", c.driver, err)
			continue
		}
		if !strings.Contains(q, c.want) || !strings.Contains(q, `'o''dd\`) {
			t.Errorf("InfoColumnsQuery(%q) = \n%s", c.driver, q)
		}
	}
	// only Postgres needs the matview branch
	if q, _ := InfoColumnsQuery("postgres", odd); !strings.Contains(q, "relkind = 'm'") {
		t.Errorf("postgres should read matviews from pg_attribute:\n%s", q)
	}
	if q, _ := InfoColumnsQuery("mysql", odd); !strings.Contains(q, `'o''dd\\name'`) {
		t.Errorf("mysql should double the backslash too:\n%s", q)
	}
	if _, err := InfoColumnsQuery("sqlite", TableRef{}); err == nil {
		t.Error("no table should be an error")
	}
	if _, err := InfoColumnsQuery("cassandra", odd); err == nil {
		t.Error("an unknown driver should not yield a columns query")
	}
}

// Lookup takes the sidebar's spelling of a name, qualified or not, and
// refuses rather than guesses when the name is ambiguous.
func TestTableIndexLookup(t *testing.T) {
	x := NewTableIndex([]TableRef{
		{Schema: "public", Name: "cats"},
		{Schema: "public", Name: "Cats"},
		{Schema: "public", Name: "owners"},
		{Schema: "audit", Name: "owners"},
		{Schema: "audit", Name: "log"},
	})
	cases := []struct {
		q          string
		wantSchema string
		wantName   string
	}{
		{"cats", "public", "cats"},        // exact case wins over "Cats"
		{"Cats", "public", "Cats"},        // and the other way
		{"CATS", "", ""},                  // two case-insensitive matches: refused
		{"public.Cats", "public", "Cats"}, // qualified
		{"owners", "", ""},                // in two schemas: refused
		{"audit.owners", "audit", "owners"},
		{"log", "audit", "log"}, // unique bare name, in any schema
		{"AUDIT.LOG", "audit", "log"},
		{"public.log", "", ""},
		{"nope", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		got, ok := x.Lookup(c.q)
		if ok != (c.wantName != "") || got.Schema != c.wantSchema || got.Name != c.wantName {
			t.Errorf("Lookup(%q) = %+v, %v; want %s.%s", c.q, got, ok, c.wantSchema, c.wantName)
		}
	}
	var none *TableIndex
	if _, ok := none.Lookup("cats"); ok {
		t.Error("a nil index should find nothing")
	}
}

// infoRows runs InfoColumnsQuery and returns its rows joined with " | "
// (NULL reads "NULL"), after checking the column names every driver shares.
func infoRows(t *testing.T, mgr *Manager, conn, driver string, tbl TableRef) []string {
	t.Helper()
	q, err := InfoColumnsQuery(driver, tbl)
	if err != nil {
		t.Fatalf("InfoColumnsQuery: %v", err)
	}
	res, err := mgr.Run(conn, q)
	if err != nil {
		t.Fatalf("run:\n%s\n%v", q, err)
	}
	want := "ordinal_position column_name data_type is_nullable column_default character_maximum_length"
	if got := strings.Join(res.Columns, " "); got != want {
		t.Errorf("columns = %s", got)
	}
	var out []string
	for _, row := range res.Rows {
		out = append(out, strings.Join(row, " | "))
	}
	return out
}

func TestInfoColumnsOnSqlite(t *testing.T) {
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{
			Name: "demo", Driver: "sqlite",
			DSN: "file:infocolumnstest?mode=memory&cache=shared",
		}},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()
	if _, err := mgr.Run("demo", `CREATE TABLE pets (id INTEGER PRIMARY KEY, name VARCHAR(80) NOT NULL, kind TEXT DEFAULT 'cat')`); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := infoRows(t, mgr, "demo", "sqlite", TableRef{Schema: "main", Name: "pets"})
	if len(got) != 3 || !strings.HasPrefix(got[0], "1 | id | INTEGER") ||
		!strings.HasPrefix(got[1], "2 | name | VARCHAR(80) | NO") ||
		!strings.HasPrefix(got[2], "3 | kind | TEXT | YES | 'cat'") {
		t.Errorf("rows = %q", got)
	}
}

func TestInfoColumnsOnBytdb(t *testing.T) {
	mgr := bytdbMgr(t)
	if _, err := mgr.Run("bd", "CREATE TABLE cats (id int PRIMARY KEY, name varchar(40) NOT NULL, age int DEFAULT 1)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := infoRows(t, mgr, "bd", "bytdb", TableRef{Schema: "public", Name: "cats"})
	if len(got) != 3 || !strings.HasPrefix(got[0], "1 | id | ") ||
		!strings.Contains(got[1], "| name |") || !strings.Contains(got[1], "| NO |") ||
		!strings.HasPrefix(got[2], "3 | age | ") {
		t.Errorf("rows = %q", got)
	}
}
