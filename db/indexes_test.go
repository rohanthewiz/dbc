package db

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// BuildIndexes reads the listing's rows: one per key column, grouped into
// indexes, with the Postgres order column kept in step with the columns.
func TestBuildIndexes(t *testing.T) {
	rows := [][]string{
		// a pattern index on (status, ref): only ref's class is a pattern one
		{"public", "orders", "o_st_ref", "i", "", "btree", "1", "status", ""},
		{"public", "orders", "o_st_ref", "i", "", "btree", "2", "ref", "pattern"},
		// a gap at position 2 (a row the driver could not read) pads both
		{"public", "orders", "o_gap", "i", "", "btree", "1", "a", "c"},
		{"public", "orders", "o_gap", "i", "", "btree", "3", "b", "c"},
		// the eight-column shape still reads, with no order
		{"public", "orders", "orders_pkey", "p", "", "btree", "1", "id"},
		// all "" (MySQL, SQLite, a linguistic collation): no Order at all
		{"public", "orders", "o_ref", "u", "partial", "BTREE", "1", "ref", ""},
	}
	got := BuildIndexes(rows)
	if len(got) != 4 {
		t.Fatalf("%d indexes: %+v", len(got), got)
	}
	want := []struct {
		name        string
		cols, order []string
	}{
		{"o_st_ref", []string{"status", "ref"}, []string{"", "pattern"}},
		{"o_gap", []string{"a", "", "b"}, []string{"c", "", "c"}},
		{"orders_pkey", []string{"id"}, nil},
		{"o_ref", []string{"ref"}, nil},
	}
	for i, w := range want {
		x := got[i]
		if x.Name != w.name || !slices.Equal(x.Cols, w.cols) || !slices.Equal(x.Order, w.order) {
			t.Errorf("index %d: %+v, want %s %q %q", i, x, w.name, w.cols, w.order)
		}
	}
	if x := got[2]; !x.Primary || !x.Unique {
		t.Errorf("primary key: %+v", x)
	}
	if x := got[3]; x.Primary || !x.Unique || !x.Partial || x.Method != "btree" {
		t.Errorf("unique partial: %+v", x)
	}
}

// Each engine's statement returns the nine columns; Postgres reads an
// operator class and collation for the ninth, SQLite a collation, and
// MySQL leaves it ''.
func TestIndexesQueryOrderColumn(t *testing.T) {
	tables := []TableRef{{Schema: "public", Name: "orders"}}
	for _, drv := range []string{"postgres", "mysql", "sqlite"} {
		q, err := IndexesQuery(drv, tables)
		if err != nil {
			t.Fatalf("%s: %v", drv, err)
		}
		pg := strings.Contains(q, "indclass") && strings.Contains(q, "indcollation") && strings.Contains(q, "'pattern'")
		if pg != (drv == "postgres") {
			t.Errorf("%s: reads the order? %v\n%s", drv, pg, q)
		}
		lite := strings.Contains(q, "pragma_index_xinfo") && strings.Contains(q, "'nocase'")
		if lite != (drv == "sqlite") {
			t.Errorf("%s: reads the SQLite collation? %v\n%s", drv, lite, q)
		}
	}
}

// The SQLite listing, run: each key column's collation read from
// index_xinfo (NOCASE named in the index, inherited from the column, or
// spelled in lower case), the columns past the key (the rowid, a WITHOUT
// ROWID table's primary key) left out, and an expression kept as "".
func TestIndexesOnSqlite(t *testing.T) {
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{
			Name: "lite", Driver: "sqlite",
			DSN: "file:indexestest?mode=memory&cache=shared",
		}},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()
	for _, s := range []string{
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, status TEXT, ref TEXT, code TEXT COLLATE NOCASE, note TEXT)`,
		`CREATE INDEX o_ref ON orders (ref)`,
		`CREATE INDEX o_ref_nc ON orders (ref COLLATE NOCASE)`,
		`CREATE INDEX o_st_ref ON orders (status, ref collate nocase)`,
		`CREATE INDEX o_code ON orders (code)`,
		`CREATE INDEX o_code_bin ON orders (code COLLATE BINARY)`,
		`CREATE INDEX o_lower ON orders (lower(note), note COLLATE NOCASE)`,
		`CREATE INDEX o_part ON orders (note COLLATE NOCASE) WHERE note IS NOT NULL`,
		`CREATE TABLE tags (name TEXT PRIMARY KEY COLLATE NOCASE, n INTEGER, extra TEXT) WITHOUT ROWID`,
		`CREATE INDEX tags_n ON tags (n)`,
	} {
		if _, err := mgr.Run("lite", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	got, err := mgr.Indexes(context.Background(), "lite",
		[]TableRef{{Schema: "main", Name: "orders"}, {Schema: "main", Name: "tags"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		cols, order []string
		primary     bool
	}{
		"o_ref":      {[]string{"ref"}, nil, false},
		"o_ref_nc":   {[]string{"ref"}, []string{OrderNocase}, false},
		"o_st_ref":   {[]string{"status", "ref"}, []string{"", OrderNocase}, false},
		"o_code":     {[]string{"code"}, []string{OrderNocase}, false},
		"o_code_bin": {[]string{"code"}, nil, false},
		"o_lower":    {[]string{"", "note"}, []string{"", OrderNocase}, false},
		"o_part":     {[]string{"note"}, []string{OrderNocase}, false},
		// a WITHOUT ROWID table's primary key is an index of its own; its
		// other columns ride along in it (key = 0) and are not keys
		"sqlite_autoindex_tags_1": {[]string{"name"}, []string{OrderNocase}, true},
		// tags_n carries the primary key after n (key = 0): cut
		"tags_n": {[]string{"n"}, nil, false},
	}
	if len(got) != len(want) {
		t.Errorf("%d indexes, want %d: %+v", len(got), len(want), got)
	}
	for _, x := range got {
		w, ok := want[x.Name]
		if !ok {
			t.Errorf("unexpected index %+v", x)
			continue
		}
		if !slices.Equal(x.Cols, w.cols) || !slices.Equal(x.Order, w.order) || x.Primary != w.primary || x.Method != "btree" {
			t.Errorf("%s: cols %q order %q primary %v, want %q %q %v", x.Name, x.Cols, x.Order, x.Primary, w.cols, w.order, w.primary)
		}
		if x.Partial != (x.Name == "o_part") {
			t.Errorf("%s: partial %v", x.Name, x.Partial)
		}
	}
}
