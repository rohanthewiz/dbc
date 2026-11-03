package db

import (
	"context"
	"errors"
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
		{"public", "orders", "o_st_ref", "i", "", "btree", "2", "ref", "pattern", "recoll"},
		// a gap at position 2 (a row the driver could not read) pads both
		{"public", "orders", "o_gap", "i", "", "btree", "1", "a", "c"},
		{"public", "orders", "o_gap", "i", "", "btree", "3", "b", "c"},
		// the eight-column shape still reads, with no order
		{"public", "orders", "orders_pkey", "p", "", "btree", "1", "id"},
		// all "" (MySQL, SQLite, a linguistic collation): no Order at all
		{"public", "orders", "o_ref", "u", "partial", "BTREE", "1", "ref", "", "NOCASE"},
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
	// the tenth column's mark, kept in step with Cols; anything else (an
	// unresolved SQLite collation name) and a short row read as false, and
	// an all-false Recollated is dropped
	if !slices.Equal(got[0].Recollated, []bool{false, true}) {
		t.Errorf("o_st_ref recollated: %v", got[0].Recollated)
	}
	for _, x := range got[1:] {
		if x.Recollated != nil {
			t.Errorf("%s recollated: %v", x.Name, x.Recollated)
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
// MySQL leaves it ”.
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
		if pgRecoll := strings.Contains(q, "attcollation"); pgRecoll != (drv == "postgres") {
			t.Errorf("%s: compares the key's collation with the column's? %v\n%s", drv, pgRecoll, q)
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
		// N-242: a key in another collation than its column's own —
		// RTRIM on a BINARY column, NOCASE on an RTRIM one, and a name
		// that needs quoting (a quote and a dot) in the probe
		`CREATE TABLE "we""ird.t" ("a.b" TEXT COLLATE RTRIM, c TEXT)`,
		`CREATE INDEX w_ab ON "we""ird.t" ("a.b")`,
		`CREATE INDEX w_ab_nc ON "we""ird.t" ("a.b" COLLATE NOCASE)`,
		`CREATE INDEX w_c_rt ON "we""ird.t" (c COLLATE RTRIM, "a.b" COLLATE RTRIM)`,
	} {
		if _, err := mgr.Run("lite", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	got, err := mgr.Indexes(context.Background(), "lite",
		[]TableRef{{Schema: "main", Name: "orders"}, {Schema: "main", Name: "tags"}, {Schema: "main", Name: `we"ird.t`}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		cols, order []string
		primary     bool
		recoll      []bool // Recollated: the key's collation is not its column's
	}{
		"o_ref": {[]string{"ref"}, nil, false, nil},
		// NOCASE on a BINARY column, alone and second: recollated
		"o_ref_nc": {[]string{"ref"}, []string{OrderNocase}, false, []bool{true}},
		"o_st_ref": {[]string{"status", "ref"}, []string{"", OrderNocase}, false, []bool{false, true}},
		// NOCASE inherited from a NOCASE column: its own
		"o_code": {[]string{"code"}, []string{OrderNocase}, false, nil},
		// BINARY named on a NOCASE column: recollated the other way
		"o_code_bin": {[]string{"code"}, nil, false, []bool{true}},
		// an expression is never recollated (no column to compare with)
		"o_lower": {[]string{"", "note"}, []string{"", OrderNocase}, false, []bool{false, true}},
		"o_part":  {[]string{"note"}, []string{OrderNocase}, false, []bool{true}},
		// a WITHOUT ROWID table's primary key is an index of its own; its
		// other columns ride along in it (key = 0) and are not keys
		"sqlite_autoindex_tags_1": {[]string{"name"}, []string{OrderNocase}, true, nil},
		// tags_n carries the primary key after n (key = 0): cut
		"tags_n":  {[]string{"n"}, nil, false, nil},
		"w_ab":    {[]string{"a.b"}, nil, false, nil},
		"w_ab_nc": {[]string{"a.b"}, []string{OrderNocase}, false, []bool{true}},
		"w_c_rt":  {[]string{"c", "a.b"}, nil, false, []bool{true, false}},
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
		if !slices.Equal(x.Recollated, w.recoll) {
			t.Errorf("%s: recollated %v, want %v", x.Name, x.Recollated, w.recoll)
		}
		if x.Partial != (x.Name == "o_part") {
			t.Errorf("%s: partial %v", x.Name, x.Partial)
		}
	}
}

// sqliteRecoll asks only about named key columns in a built-in collation,
// each (table, column) once, and on a failed probe leaves every row
// unresolved ("", the column's own) rather than guessing.
func TestSqliteRecoll(t *testing.T) {
	mk := func() [][]string {
		return [][]string{
			{"main", "t", "a", "i", "", "btree", "1", "ref", "nocase", "NOCASE"},
			{"main", "t", "b", "i", "", "btree", "1", "ref", "", "BINARY"},
			{"main", "t", "c", "i", "", "btree", "1", "", "", "BINARY"},  // an expression
			{"main", "t", "d", "i", "", "btree", "1", "x", "", "MYCOLL"}, // not probed
		}
	}
	var asked string
	rows := mk()
	sqliteRecoll(rows, func(q string) ([]string, error) {
		asked = q
		return []string{"2", "2"}, nil // ref is BINARY
	})
	if n := strings.Count(asked, "UNION SELECT 'a'"); n != 2 {
		t.Errorf("%d probes, want 2 (ref once, two pairs): %s", n, asked)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r[9])
	}
	if want := []string{"recoll", "", "", ""}; !slices.Equal(got, want) {
		t.Errorf("marks %q, want %q", got, want)
	}

	rows = mk()
	sqliteRecoll(rows, func(string) ([]string, error) { return nil, errors.New("boom") })
	for _, x := range BuildIndexes(rows) {
		if x.Recollated != nil {
			t.Errorf("failed probe: %s recollated %v", x.Name, x.Recollated)
		}
	}
}
