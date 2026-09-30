package db

import (
	"context"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/erd"
)

// petsDDL is one schema in the dialect bytdb, SQLite, Postgres and MySQL all
// take, with every key shape the diagram reads: a unique key, a nullable
// foreign key, a NOT NULL one inside a composite primary key (identifying),
// a 1:1 (unique foreign key), a self-reference, and a table with no
// relationships (it has a primary key, which bytdb requires of every table).
//
// The foreign keys are table constraints, not inline REFERENCES on a
// column: MySQL 8 parses an inline REFERENCES and silently ignores it.
var petsDDL = []string{
	`CREATE TABLE owners (id int PRIMARY KEY, email varchar(80) UNIQUE, name text NOT NULL)`,
	`CREATE TABLE cats (id int PRIMARY KEY, owner_id int, mother_id int, name text,
		FOREIGN KEY (owner_id) REFERENCES owners(id), FOREIGN KEY (mother_id) REFERENCES cats(id))`,
	`CREATE TABLE visits (cat_id int NOT NULL, seq int NOT NULL, note text, PRIMARY KEY (cat_id, seq),
		FOREIGN KEY (cat_id) REFERENCES cats(id))`,
	`CREATE TABLE profiles (id int PRIMARY KEY, owner_id int NOT NULL UNIQUE, bio text,
		FOREIGN KEY (owner_id) REFERENCES owners(id))`,
	`CREATE TABLE notes (id int PRIMARY KEY, body text)`,
}

// checkPets asserts what every engine must report for petsDDL. prefix is
// the tables' name prefix on a live server (whose database is shared).
func checkPets(t *testing.T, s *erd.Schema, prefix string) {
	t.Helper()
	find := func(name string) *erd.Table {
		tb, ok := s.Find(prefix + name)
		if !ok {
			t.Fatalf("no table %s%s in %v", prefix, name, s.Tables)
		}
		return tb
	}
	owners, cats, visits, notes := find("owners"), find("cats"), find("visits"), find("notes")
	if strings.Join(owners.PK, ",") != "id" || strings.Join(visits.PK, ",") != "cat_id,seq" {
		t.Errorf("PKs: owners %v, visits %v", owners.PK, visits.PK)
	}
	if len(owners.Uniques) != 1 || owners.Uniques[0][0] != "email" || !owners.Col("email").Unique {
		t.Errorf("owners.Uniques = %v", owners.Uniques)
	}
	if len(notes.Cols) != 2 || notes.Col("body").PK || notes.Col("body").FK {
		t.Errorf("notes = %+v", notes)
	}
	if c := cats.Col("owner_id"); c == nil || !c.FK || !c.Nullable {
		t.Errorf("cats.owner_id = %+v", c)
	}
	// keyed by Label, not Name: on Postgres the prefix is a schema
	// (dbc_erd.cats), which only the label carries
	rels := map[string]*erd.Rel{}
	for _, r := range s.Rels {
		rels[r.Child.Label+"."+strings.Join(r.ChildCols, ",")+"→"+r.Parent.Label+"."+strings.Join(r.ParentCols, ",")] = r
	}
	p := prefix
	for _, want := range []struct {
		key                          string
		optional, oneToOne, identify bool
	}{
		{p + "cats.owner_id→" + p + "owners.id", true, false, false},
		{p + "cats.mother_id→" + p + "cats.id", true, false, false},
		{p + "visits.cat_id→" + p + "cats.id", false, false, true},
		{p + "profiles.owner_id→" + p + "owners.id", false, true, false},
	} {
		r := rels[want.key]
		if r == nil {
			t.Errorf("no relationship %s in %v", want.key, keysOf(rels))
			continue
		}
		if r.Optional() != want.optional || r.OneToOne() != want.oneToOne || r.Identifying() != want.identify {
			t.Errorf("%s: optional=%v 1:1=%v identifying=%v", want.key, r.Optional(), r.OneToOne(), r.Identifying())
		}
	}
}

func keysOf(m map[string]*erd.Rel) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSchemaOnBytdb(t *testing.T) {
	mgr := bytdbMgr(t)
	for _, s := range petsDDL {
		if _, err := mgr.Run("bd", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	s, err := mgr.Schema(context.Background(), "bd")
	if err != nil {
		t.Fatal(err)
	}
	checkPets(t, s, "")
	if len(s.Rels) != 4 {
		t.Errorf("%d relationships", len(s.Rels))
	}
}

func TestSchemaOnSqlite(t *testing.T) {
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{
			Name: "demo", Driver: "sqlite",
			DSN: "file:schematest?mode=memory&cache=shared",
		}},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()
	ddl := append([]string{}, petsDDL...)
	// SQLite's shorthand: REFERENCES a table, not a column, means its
	// primary key, which the pragma reports as a NULL "to"
	ddl = append(ddl, `CREATE TABLE tags (cat_id int REFERENCES cats, tag text)`,
		`CREATE UNIQUE INDEX tags_one ON tags (cat_id, tag)`,
		`CREATE VIEW old_cats AS SELECT id, name FROM cats`)
	for _, s := range ddl {
		if _, err := mgr.Run("demo", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	s, err := mgr.Schema(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	checkPets(t, s, "")
	tags, _ := s.Find("tags")
	var implicit *erd.Rel
	for _, r := range s.Rels {
		if r.Child == tags {
			implicit = r
		}
	}
	if implicit == nil || implicit.ParentCols[0] != "id" {
		t.Errorf("tags → cats = %+v", implicit)
	}
	if len(tags.Uniques) != 1 || strings.Join(tags.Uniques[0], ",") != "cat_id,tag" {
		t.Errorf("tags.Uniques = %v", tags.Uniques)
	}
	if v, ok := s.Find("old_cats"); !ok || !v.View || len(v.Cols) != 2 {
		t.Errorf("the view = %+v", v)
	}
	if got := s.WithoutViews(); len(got.Tables) != len(s.Tables)-1 {
		t.Errorf("WithoutViews kept %d of %d", len(got.Tables), len(s.Tables))
	}
}

// More rows than max_rows must not cut the diagram: the schema is read past
// the grid's cap.
func TestSchemaIgnoresMaxRows(t *testing.T) {
	mgr := bytdbMgr(t)
	mgr.cfg.MaxRows = 3
	for _, s := range petsDDL {
		if _, err := mgr.Run("bd", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	s, err := mgr.Schema(context.Background(), "bd")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tables) != 5 {
		t.Errorf("%d tables with max_rows 3", len(s.Tables))
	}
}

// BuildSchema on rows as Postgres returns them: key columns as attnum
// arrays, a dropped column's gap in attnum, a table in a second schema with
// the same name, and a foreign key to a table outside the catalog.
func TestBuildSchemaPostgresRows(t *testing.T) {
	tables := []TableRef{{Schema: "app", Name: "owners"}, {Schema: "app", Name: "cats"}, {Schema: "old", Name: "cats"}}
	cols := [][]string{
		{"app", "owners", "id", "integer", "NO", "1"},
		{"app", "cats", "id", "integer", "NO", "1"},
		// attnum 2 was dropped
		{"app", "cats", "owner_id", "integer", "YES", "3"},
		{"app", "cats", "name", "character varying(80)", "YES", "4"},
		{"old", "cats", "id", "integer", "YES", "1"},
		{"app", "ghost_idx", "x", "int", "NO", "1"}, // not in the catalog
	}
	keys := [][]string{
		{"app", "owners", "owners_pkey", "p", "{1}", "", "", "", ""},
		{"app", "cats", "cats_pkey", "p", "{1}", "", "", "", ""},
		{"app", "cats", "cats_owner_fkey", "f", "{3}", "", "app", "owners", "{1}"},
		{"app", "cats", "cats_name_key", "u", "{4}", "", "", "", ""},
		{"app", "cats", "cats_far_fkey", "f", "{3}", "", "elsewhere", "far", "{1}"},
		{"app", "cats", "cats_bad_fkey", "f", "{9}", "", "app", "owners", "{1}"}, // attnum not listed
	}
	s := BuildSchema("postgres", "pg", tables, cols, keys, nil)
	if len(s.Tables) != 3 || len(s.Rels) != 1 {
		t.Fatalf("tables %d rels %d: %+v", len(s.Tables), len(s.Rels), s.Rels)
	}
	r := s.Rels[0]
	if r.Child.Label != "app.cats" || r.Parent.Label != "app.owners" || r.ChildCols[0] != "owner_id" || r.ParentCols[0] != "id" {
		t.Errorf("rel = %s.%v → %s.%v", r.Child.Label, r.ChildCols, r.Parent.Label, r.ParentCols)
	}
	cats, _ := s.Find("app.cats")
	if c := cats.Col("name"); !c.Unique || c.Type != "character varying(80)" {
		t.Errorf("app.cats.name = %+v", c)
	}
	old, _ := s.Find("old.cats")
	if len(old.Cols) != 1 || len(old.PK) != 0 {
		t.Errorf("old.cats merged with app.cats: %+v", old)
	}
}

// MySQL reports table_schema in the server's stored case, which may differ
// from the catalog's; the table name alone must still resolve.
func TestBuildSchemaMySQLSchemaCase(t *testing.T) {
	tables := []TableRef{{Schema: "Shop", Name: "orders"}, {Schema: "Shop", Name: "lines"}}
	cols := [][]string{
		{"shop", "orders", "id", "int", "NO", "1"},
		{"shop", "lines", "order_id", "int", "NO", "1"},
		{"shop", "lines", "n", "int", "NO", "2"},
	}
	keys := [][]string{
		{"shop", "orders", "PRIMARY", "p", "1", "id", "", "", ""},
		{"shop", "lines", "PRIMARY", "p", "2", "n", "", "", ""},
		{"shop", "lines", "PRIMARY", "p", "1", "order_id", "", "", ""}, // out of order
		{"shop", "lines", "lines_ibfk_1", "f", "1", "order_id", "shop", "orders", "id"},
	}
	s := BuildSchema("mysql", "my", tables, cols, keys, nil)
	lines, _ := s.Find("lines")
	if strings.Join(lines.PK, ",") != "order_id,n" || len(s.Rels) != 1 || !s.Rels[0].Identifying() {
		t.Errorf("lines = %+v, rels %d", lines, len(s.Rels))
	}
}

// A partitioned table's partitions are hidden, with the foreign keys
// Postgres cloned onto them (conparentid <> 0) and the ones it cloned to
// reference them; the partitioned table keeps its own key, and the labels
// stay qualified as the sidebar's are.
func TestBuildSchemaHidesPartitions(t *testing.T) {
	tables := []TableRef{
		{Schema: "app", Name: "owners"},
		{Schema: "app", Name: "events"},
		{Schema: "parts", Name: "events_2026_01"},
		{Schema: "parts", Name: "events_2026_02"},
		{Schema: "app", Name: "notes"},
	}
	cols := [][]string{{"app", "owners", "id", "integer", "NO", "1"}, {"app", "notes", "event_id", "integer", "YES", "1"}}
	for _, tb := range []string{"events", "events_2026_01", "events_2026_02"} {
		sch := "parts"
		if tb == "events" {
			sch = "app"
		}
		cols = append(cols, []string{sch, tb, "id", "integer", "NO", "1"}, []string{sch, tb, "owner_id", "integer", "NO", "2"})
	}
	keys := [][]string{
		{"app", "owners", "owners_pkey", "p", "{1}", "", "", "", ""},
		{"app", "events", "events_pkey", "p", "{1}", "", "", "", ""},
		{"app", "events", "events_owner_fkey", "f", "{2}", "", "app", "owners", "{1}"},
		// the clones on each partition
		{"parts", "events_2026_01", "events_owner_fkey", "f", "{2}", "", "app", "owners", "{1}"},
		{"parts", "events_2026_02", "events_owner_fkey", "f", "{2}", "", "app", "owners", "{1}"},
		// a key to the partitioned table, and its clones to each partition
		{"app", "notes", "notes_event_fkey", "f", "{1}", "", "app", "events", "{1}"},
		{"app", "notes", "notes_event_fkey1", "f", "{1}", "", "parts", "events_2026_01", "{1}"},
		{"app", "notes", "notes_event_fkey2", "f", "{1}", "", "parts", "events_2026_02", "{1}"},
	}
	hidden := [][]string{{"parts", "events_2026_01"}, {"parts", "events_2026_02"}}
	s := BuildSchema("postgres", "pg", tables, cols, keys, hidden)
	var names, rels []string
	for _, tb := range s.Tables {
		names = append(names, tb.Label)
	}
	for _, r := range s.Rels {
		rels = append(rels, r.Name)
	}
	if strings.Join(names, " ") != "app.events app.notes app.owners" {
		t.Errorf("tables = %q", names)
	}
	if strings.Join(rels, " ") != "events_owner_fkey notes_event_fkey" {
		t.Errorf("rels = %q", rels)
	}
}

func TestPartitionsQuery(t *testing.T) {
	if q, err := PartitionsQuery("postgres"); err != nil || !strings.Contains(q, "relispartition") {
		t.Errorf("postgres: %q, %v", q, err)
	}
	for _, d := range []string{"bytdb", "mysql", "sqlite"} {
		if q, err := PartitionsQuery(d); err != nil || q != "" {
			t.Errorf("%s: %q, %v", d, q, err)
		}
	}
}

func TestPgArray(t *testing.T) {
	for in, want := range map[string]string{"{1,2}": "1|2", "{3}": "3", "": "", "{}": "", " { 4 , 5 } ": "4|5"} {
		if got := strings.Join(pgArray(in), "|"); got != want {
			t.Errorf("pgArray(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSchemaQueriesPerDriver(t *testing.T) {
	for _, d := range []string{"postgres", "bytdb", "mysql", "sqlite"} {
		if _, err := SchemaColumnsQuery(d); err != nil {
			t.Errorf("%s columns: %v", d, err)
		}
		if _, err := SchemaKeysQuery(d); err != nil {
			t.Errorf("%s keys: %v", d, err)
		}
	}
	if _, err := SchemaKeysQuery("oracle"); err == nil {
		t.Error("an unknown driver has no key query")
	}
}
