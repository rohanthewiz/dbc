package db

import (
	"slices"
	"strings"
	"testing"
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

// Each engine's statement returns the nine columns; only Postgres reads an
// operator class and collation for the ninth.
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
	}
}
