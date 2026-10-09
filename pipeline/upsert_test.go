package pipeline

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// sql.write's mode upsert, through the runner on SQLite: a created table
// filled, then a re-run that updates the rows whose key is there and adds
// the new one — batches smaller than the load, so the upsert holds across
// INSERT statements. (The Postgres staging-table path is etl's
// TestLivePGUpsert.)
func TestSQLWriteUpsert(t *testing.T) {
	h := newTestHost(t)
	spec := func(upTo int) *Spec {
		return mustParse(t, `{"name": "up", "fragments": [{"name": "load", "batch": 2, "nodes": [
			{"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id, name, age FROM cats WHERE id <= `+strconv.Itoa(upTo)+`"}},
			{"id": "dst", "plugin": "sql.write", "cfg": {"conn": "b", "table": "pets", "create": "true", "mode": "upsert", "key": "id"}}],
			"edges": [["src", "dst"]]}]}`)
	}
	st, err := Run(h.ctx, h, spec(5), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows() != 5 {
		t.Errorf("first run: %d rows, want 5", st.Rows())
	}
	h.exec(t, "a", `UPDATE cats SET name = 'renamed', age = 99 WHERE id IN (2, 5)`)
	if st, err = Run(h.ctx, h, spec(6), Options{}); err != nil {
		t.Fatal(err)
	}
	if st.Fragments[0].Direct {
		t.Error("an upsert ran direct")
	}
	want := [][]string{{"1", "cat01", "1"}, {"2", "renamed", "99"}, {"3", "cat03", "3"}, {"4", "cat04", "4"},
		{"5", "renamed", "99"}, {"6", "cat06", "6"}}
	if got := h.rows(t, "b", "SELECT id, name, age FROM pets ORDER BY id"); !reflect.DeepEqual(got, want) {
		t.Errorf("after the second run:\n got %q\nwant %q", got, want)
	}
}

// mode upsert needs key: Check says so where the node is (so Run refuses
// the spec before touching a database), and New refuses to build the
// node should it be reached some other way.
func TestSQLWriteUpsertNeedsKey(t *testing.T) {
	h := newTestHost(t)
	spec := mustParse(t, `{"name": "up", "fragments": [{"name": "load", "nodes": [
		{"id": "src", "plugin": "sql.table", "cfg": {"conn": "a", "table": "cats"}},
		{"id": "dst", "plugin": "sql.write", "cfg": {"conn": "b", "table": "pets", "mode": "upsert"}}],
		"edges": [["src", "dst"]]}]}`)
	ds := Check(spec, CheckOptions{})
	if len(ds) != 1 || ds[0].Where != "load/dst" || !strings.Contains(ds[0].Msg, "mode upsert needs key") {
		t.Errorf("Check = %v", ds)
	}
	if st, err := Run(h.ctx, h, spec, Options{}); err == nil || st.Status != Failed {
		t.Errorf("Run: %v, err = %v; want a refusal", st.Status, err)
	}
	p, _ := Lookup("sql.write")
	if _, err := p.New(p.Defaults(Config{"conn": "b", "table": "pets", "mode": "upsert"})); err == nil ||
		!strings.Contains(err.Error(), "mode upsert needs key") {
		t.Errorf("New: err = %v", err)
	}
	// an unknown mode is the enum's message
	spec.Fragments[0].Nodes[1].Cfg["mode"] = "merge"
	spec.Fragments[0].Nodes[1].Cfg["key"] = "id"
	if ds = Check(spec, CheckOptions{}); len(ds) != 1 || !strings.Contains(ds[0].Msg, `"merge" is not one of insert, upsert`) {
		t.Errorf("Check, mode merge = %v", ds)
	}
}

// A Postgres sql.read straight into a Postgres sql.write runs as one
// direct COPY — unless the sink upserts: a COPY into the table cannot
// merge, so the sink declines the direct path and the rows take the
// Writer's staging table.
func TestSQLWriteUpsertNotDirect(t *testing.T) {
	p, _ := Lookup("sql.write")
	build := func(cfg Config) directSink {
		t.Helper()
		n, err := p.New(p.Defaults(cfg))
		if err != nil {
			t.Fatal(err)
		}
		return n.(directSink)
	}
	if _, _, _, _, _, ok := build(Config{"conn": "pg", "table": "t"}).directSink(); !ok {
		t.Error("a plain insert declined the direct path")
	}
	if _, _, _, _, _, ok := build(Config{"conn": "pg", "table": "t", "mode": "upsert", "key": "id"}).directSink(); ok {
		t.Error("an upsert would run as a direct COPY")
	}
}
