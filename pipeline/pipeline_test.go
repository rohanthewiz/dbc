package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	_ "modernc.org/sqlite"

	"github.com/rohanthewiz/dbc/etl"
	"github.com/rohanthewiz/dbc/model"
)

// testHost is a Host over bare *sql.DBs: no db.Manager, no config, so
// the runner is tested on its own. Print lines and shown results are
// kept for the assertions.
type testHost struct {
	ctx    context.Context
	conns  map[string]etl.Conn
	lines  []string
	shown  []*model.Result
	traced []string
}

func newTestHost(t *testing.T) *testHost {
	t.Helper()
	h := &testHost{ctx: context.Background(), conns: map[string]etl.Conn{}}
	open := func(name, drv string, e etl.Engine) {
		dbh, err := sql.Open(drv, filepath.Join(t.TempDir(), name+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dbh.Close() })
		c := etl.Conn{Name: name, DB: dbh, Engine: e, Trace: func(stmt string) { h.traced = append(h.traced, stmt) }}
		h.conns[name] = c
	}
	open("a", "sqlite", etl.SQLite)
	open("b", "sqlite", etl.SQLite)
	open("k", bytdbdrv.DriverName, etl.Bytdb)
	h.exec(t, "a", `CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT, breed TEXT, age INTEGER)`)
	for i := 1; i <= 25; i++ {
		h.exec(t, "a", fmt.Sprintf(`INSERT INTO cats VALUES (%d, 'cat%02d', '%s', %d)`, i, i,
			[]string{"Tabby", "Siamese", "Maine Coon"}[i%3], i%7))
	}
	return h
}

func (h *testHost) exec(t *testing.T, conn, stmt string) {
	t.Helper()
	if _, err := h.conns[conn].DB.Exec(stmt); err != nil {
		t.Fatalf("%s: %s: %v", conn, stmt, err)
	}
}

func (h *testHost) rows(t *testing.T, conn, q string) [][]string {
	t.Helper()
	rd, err := etl.Read(context.Background(), h.conns[conn], q)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	var out [][]string
	for rd.Next() {
		row := make([]string, len(rd.Row()))
		for i, v := range rd.Row() {
			row[i] = FormatValue(v)
		}
		out = append(out, row)
	}
	if err := rd.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (h *testHost) Ctx() context.Context { return h.ctx }
func (h *testHost) Conns() []string      { return []string{"a", "b", "k"} }
func (h *testHost) Query(conn, query string, args ...any) (*model.Result, error) {
	rd, err := etl.Read(h.ctx, h.conns[conn], query, args...)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	b := &Batch{Cols: ColsOf(rd.Columns(), rd.DBTypes())}
	for rd.Next() {
		b.Rows = append(b.Rows, rd.Row())
	}
	return b.Result(conn, query), rd.Err()
}
func (h *testHost) Exec(conn, stmt string, args ...any) (int64, error) {
	c, ok := h.conns[conn]
	if !ok {
		return 0, errors.New("unknown connection " + conn)
	}
	res, err := c.DB.ExecContext(h.ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
func (h *testHost) Print(format string, args ...any) {
	h.lines = append(h.lines, fmt.Sprintf(format, args...))
}
func (h *testHost) Show(r *model.Result)            { h.shown = append(h.shown, r) }
func (h *testHost) DB(conn string) (*sql.DB, error) { return h.conns[conn].DB, nil }
func (h *testHost) ETLConn(name string) (etl.Conn, error) {
	c, ok := h.conns[name]
	if !ok {
		return etl.Conn{}, errors.New("unknown connection " + name)
	}
	return c, nil
}

// mustParse is Parse that fails the test.
func mustParse(t *testing.T, text string) *Spec {
	t.Helper()
	s, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A linear fragment into two engines, batch by batch: rows arrive
// whole, the created table has every column, and a second fragment reads
// the first's published row count.
func TestRunLoadAndVars(t *testing.T) {
	h := newTestHost(t)
	spec := mustParse(t, `{
	  "name": "load", "params": {"min": {"default": "3"}},
	  "fragments": [
	    {"name": "cats", "batch": 4, "nodes": [
	      {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id, name, breed, age FROM cats WHERE age >= ${min} ORDER BY id"}},
	      {"id": "cols", "plugin": "cols.select", "cfg": {"keep": "id, name, age", "rename": "name=cat"}},
	      {"id": "lite", "plugin": "sql.write", "cfg": {"conn": "b", "table": "out", "create": "true", "truncate": "true"}},
	      {"id": "byt", "plugin": "sql.write", "cfg": {"conn": "k", "table": "out", "create": "true", "key": "id"}}
	    ], "edges": [["src", "cols"], ["cols", "lite"], ["cols", "byt"]]},
	    {"name": "stamp", "nodes": [
	      {"id": "x", "plugin": "sql.exec", "cfg": {"conn": "b", "sql": "CREATE TABLE log (n INTEGER); INSERT INTO log VALUES (${frag.cats.rows})"}}
	    ]}
	  ]}`)
	st, err := Run(context.Background(), h, spec, Options{Params: map[string]string{"min": "4"}})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != Succeeded || len(st.Fragments) != 2 {
		t.Fatalf("stats: %+v", st)
	}
	// age >= 4 of i%7: i in {4,5,6,11,12,13,18,19,20,25} → 10 rows, into two sinks
	want := 10
	for _, conn := range []string{"b", "k"} {
		got := h.rows(t, conn, "SELECT id, cat, age FROM out ORDER BY id")
		if len(got) != want || got[0][1] != "cat04" {
			t.Errorf("%s: got %d rows, first %v", conn, len(got), got[0])
		}
	}
	if fs := st.Fragments[0]; fs.Rows != int64(2*want) || fs.Vars["rows"] != "20" {
		t.Errorf("fragment rows %d vars %v", fs.Rows, fs.Vars)
	}
	if got := h.rows(t, "b", "SELECT n FROM log"); len(got) != 1 || got[0][0] != "20" {
		t.Errorf("log: %v", got)
	}
	// batches of 4 over 10 rows: 3 batches at every node
	for _, n := range st.Fragments[0].Nodes {
		if n.Batches != 3 {
			t.Errorf("node %s: %d batches", n.ID, n.Batches)
		}
	}
	if st.Fragments[1].Vars["affected"] != "1" {
		t.Errorf("affected: %v", st.Fragments[1].Vars)
	}
	if !strings.Contains(strings.Join(h.lines, "\n"), "pipeline load: 2 fragments, 21 rows") {
		t.Errorf("summary line missing: %q", h.lines)
	}
}

// A transform failing mid-stream rolls every sink back: the table that
// was truncated keeps its old rows, and the one being created on bytdb
// (DDL outside the transaction) is empty.
func TestRunFailureRollsBackEverySink(t *testing.T) {
	h := newTestHost(t)
	h.exec(t, "b", `CREATE TABLE out (id INTEGER, name TEXT); INSERT INTO out VALUES (99, 'old')`)
	bad := func(_ *Env, b *Batch) (*Batch, error) {
		if v, _ := b.Get(0, "id").(int64); v > 8 {
			return nil, errors.New("boom at " + fmt.Sprint(v))
		}
		return b, nil
	}
	p := New("fail")
	f := p.Fragment("f").Batch(5)
	f.Node("sql.read", Config{"conn": "a", "query": "SELECT id, name FROM cats ORDER BY id"})
	f.ThenFunc(bad)
	f.Then("sql.write", Config{"conn": "b", "table": "out", "truncate": "true"})
	f.Wire(f.frag().Nodes[1].ID, f.Named("k", "sql.write", Config{"conn": "k", "table": "out", "create": "true", "key": "id"}))
	st, err := Run(context.Background(), h, p.Spec(), Options{})
	if err == nil || !strings.Contains(err.Error(), "boom at 11") {
		t.Fatalf("err = %v", err)
	}
	if st.Status != Failed || st.Fragments[0].Status != Failed || st.Fragments[0].Nodes[1].Error == "" {
		t.Errorf("stats: %+v", st.Fragments[0])
	}
	if got := h.rows(t, "b", "SELECT id, name FROM out"); len(got) != 1 || got[0][1] != "old" {
		t.Errorf("b.out after rollback: %v", got)
	}
	if got := h.rows(t, "k", "SELECT count(*) FROM out"); got[0][0] != "0" {
		t.Errorf("k.out after rollback: %v", got)
	}
}

// Cancelling mid-run stops the fragment, rolls back, and marks the run
// canceled rather than failed.
func TestRunCancel(t *testing.T) {
	h := newTestHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	n := 0
	p := New("cancel")
	f := p.Fragment("f").Batch(5)
	f.Node("sql.read", Config{"conn": "a", "query": "SELECT id FROM cats"})
	f.ThenFunc(func(b *Batch) (*Batch, error) {
		if n++; n == 2 {
			cancel()
		}
		return b, nil
	})
	f.Then("sql.write", Config{"conn": "b", "table": "out", "create": "true"})
	st, err := Run(ctx, h, p.Spec(), Options{})
	if err == nil || st.Status != Canceled || st.Fragments[0].Status != Canceled {
		t.Fatalf("err %v status %s / %s", err, st.Status, st.Fragments[0].Status)
	}
	if got := h.rows(t, "b", "SELECT name FROM sqlite_master WHERE name = 'out'"); len(got) != 0 {
		t.Errorf("out was created despite the rollback: %v", got)
	}
}

// A preview writes nothing: the sinks become previews, the source stops
// after N rows, actions are skipped, and the rows reach Show.
func TestRunPreview(t *testing.T) {
	h := newTestHost(t)
	spec := mustParse(t, `{"name": "p", "fragments": [
	  {"name": "f", "batch": 3, "nodes": [
	    {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id, name FROM cats ORDER BY id"}},
	    {"id": "up", "plugin": "rows.filter", "cfg": {"rules": "id > 2"}},
	    {"id": "dst", "plugin": "sql.write", "cfg": {"conn": "b", "table": "out", "create": "true"}}
	  ], "edges": [["src", "up"], ["up", "dst"]]},
	  {"name": "act", "nodes": [{"id": "x", "plugin": "sql.exec", "cfg": {"conn": "b", "sql": "CREATE TABLE nope (x INTEGER)"}}]}
	]}`)
	st, err := Run(context.Background(), h, spec, Options{PreviewRows: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Preview || st.Fragments[1].Status != Skipped {
		t.Errorf("stats: %+v", st)
	}
	if got := h.rows(t, "b", "SELECT name FROM sqlite_master"); len(got) != 0 {
		t.Errorf("a preview wrote: %v", got)
	}
	if len(h.shown) != 1 || len(h.shown[0].Rows) != 5 || h.shown[0].Rows[0][0] != "3" {
		t.Fatalf("shown: %+v", h.shown)
	}
	// batches of 3: the source read 9 rows in 3 batches before the limit
	// (spliced after it) said stop, and let 7 through
	nodes := map[string]NodeStats{}
	for _, n := range st.Fragments[0].Nodes {
		nodes[n.ID] = n
	}
	if src := nodes["src"]; src.Out != 9 || src.Batches != 3 {
		t.Errorf("source read %d rows in %d batches, want 9 in 3", src.Out, src.Batches)
	}
	if lim := nodes["limit.preview"]; lim.In != 9 || lim.Out != 7 {
		t.Errorf("limit %d→%d, want 9→7", lim.In, lim.Out)
	}
}

// A sink no rows reached, with nothing but the source above it, is still
// opened: its truncate empties the table (a reload from an empty source
// is an empty table, not a stale one).
func TestRunEmptySourceStillTruncates(t *testing.T) {
	h := newTestHost(t)
	h.exec(t, "b", `CREATE TABLE out (id INTEGER, name TEXT, breed TEXT, age INTEGER); INSERT INTO out VALUES (1, 'x', 'y', 2)`)
	spec := mustParse(t, `{"name": "e", "fragments": [{"name": "f", "nodes": [
	  {"id": "src", "plugin": "sql.table", "cfg": {"conn": "a", "table": "cats", "where": "age > 100"}},
	  {"id": "dst", "plugin": "sql.write", "cfg": {"conn": "b", "table": "out", "truncate": "true"}}
	], "edges": [["src", "dst"]]}]}`)
	if _, err := Run(context.Background(), h, spec, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := h.rows(t, "b", "SELECT count(*) FROM out"); got[0][0] != "0" {
		t.Errorf("out not truncated: %v", got)
	}
}

// rows.filter's operators, on a batch in memory.
func TestRowsFilter(t *testing.T) {
	cols := ColsOf([]string{"n", "s", "x"}, nil)
	mk := func() *Batch {
		return NewBatch(cols, [][]any{{int64(1), "Alpha", nil}, {int64(10), "beta", "v"}, {float64(2.5), "Gamma", "w"}})
	}
	for rules, want := range map[string]int{
		"n > 2":            2,
		"n >= 10":          1,
		"n < 2":            1,
		"s like %a":        3,
		"s like alp%":      1,
		"s in Alpha,Gamma": 2,
		"x null":           1,
		"x notnull":        2,
		"s != beta":        2,
		"n = 2.5":          1,
		"n > 2\nx notnull": 2,
	} {
		p, _ := Lookup("rows.filter")
		node, err := p.New(Config{"rules": rules})
		if err != nil {
			t.Fatalf("%q: %v", rules, err)
		}
		b, err := node.(Transform).Apply(&Env{}, mk())
		if err != nil {
			t.Fatalf("%q: %v", rules, err)
		}
		if b.Len() != want {
			t.Errorf("%q: kept %d rows, want %d", rules, b.Len(), want)
		}
	}
	if _, err := parseRule("n ~ 3"); err == nil {
		t.Error("a bad operator passed")
	}
}

// Check's rules, one by one.
func TestCheck(t *testing.T) {
	for name, c := range map[string]struct {
		spec string
		want string // a substring of one diag; "" for clean
	}{
		"clean": {`{"name": "ok", "fragments": [{"name": "f", "nodes": [
			{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT 1"}},
			{"id": "d", "plugin": "sql.write", "cfg": {"conn": "b", "table": "t"}}], "edges": [["s", "d"]]}]}`, ""},
		"no fragments": {`{"name": "x", "fragments": []}`, "at least one fragment"},
		"bad name":     {`{"name": "x y", "fragments": [{"name": "f", "nodes": [{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT 1"}}]}]}`, "not a pipeline name"},
		"no plugin":    {`{"name": "x", "fragments": [{"name": "f", "nodes": [{"id": "s", "plugin": "sql.nope"}]}]}`, `no plugin "sql.nope"`},
		"required":     {`{"name": "x", "fragments": [{"name": "f", "nodes": [{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a"}}]}]}`, "query: required"},
		"unknown field": {`{"name": "x", "fragments": [{"name": "f", "nodes": [{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x", "nope": "1"}}]}]}`,
			"nope: no such field"},
		"bad int": {`{"name": "x", "fragments": [{"name": "f", "nodes": [{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}},
			{"id": "l", "plugin": "rows.limit", "cfg": {"rows": "ten"}}], "edges": [["s", "l"]]}]}`, "rows: not an integer"},
		"unknown ref": {`{"name": "x", "fragments": [{"name": "f", "nodes": [{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT ${dasy}"}}]}]}`,
			"${dasy} is not a parameter"},
		"later fragment ref": {`{"name": "x", "fragments": [
			{"name": "f", "nodes": [{"id": "s", "plugin": "sql.exec", "cfg": {"conn": "a", "sql": "SELECT ${frag.g.rows}"}}]},
			{"name": "g", "nodes": [{"id": "s", "plugin": "sql.exec", "cfg": {"conn": "a", "sql": "SELECT 1"}}]}]}`, "${frag.g.rows} is not"},
		"unknown conn": {`{"name": "x", "fragments": [{"name": "f", "nodes": [{"id": "s", "plugin": "sql.exec", "cfg": {"conn": "zz", "sql": "SELECT 1"}}]}]}`, `no connection called "zz"`},
		"two sources": {`{"name": "x", "fragments": [{"name": "f", "nodes": [
			{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}}, {"id": "t", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}}]}]}`, "one source per fragment"},
		"action with company": {`{"name": "x", "fragments": [{"name": "f", "nodes": [
			{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}}, {"id": "x", "plugin": "sql.exec", "cfg": {"conn": "a", "sql": "x"}}]}]}`, "a fragment of its own"},
		"two parents": {`{"name": "x", "fragments": [{"name": "f", "nodes": [
			{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}}, {"id": "l", "plugin": "rows.limit", "cfg": {"rows": "1"}},
			{"id": "d", "plugin": "preview"}], "edges": [["s", "l"], ["s", "d"], ["l", "d"]]}]}`, "one input, not 2"},
		"sink output": {`{"name": "x", "fragments": [{"name": "f", "nodes": [
			{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}}, {"id": "d", "plugin": "preview"}, {"id": "l", "plugin": "rows.limit", "cfg": {"rows": "1"}}],
			"edges": [["s", "d"], ["d", "l"]]}]}`, "a sink has no output"},
		"unreachable": {`{"name": "x", "fragments": [{"name": "f", "nodes": [
			{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}}, {"id": "d", "plugin": "preview"}]}]}`, "not connected to the source"},
		"edge to nowhere": {`{"name": "x", "fragments": [{"name": "f", "nodes": [
			{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}}], "edges": [["s", "zz"]]}]}`, "names a node the fragment does not have"},
		"no sink warns": {`{"name": "x", "fragments": [{"name": "f", "nodes": [
			{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}}]}]}`, "warning: no sink"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := mustParse(t, c.spec)
			diags := Check(spec, CheckOptions{Conns: []string{"a", "b"}})
			var all []string
			for _, d := range diags {
				all = append(all, d.String())
			}
			joined := strings.Join(all, "\n")
			if c.want == "" {
				if len(diags) != 0 {
					t.Errorf("want clean, got %s", joined)
				}
				return
			}
			if !strings.Contains(joined, c.want) {
				t.Errorf("want a diag containing %q, got:\n%s", c.want, joined)
			}
		})
	}
}

// A Parse refuses a key the spec does not have; JSON round-trips, edges
// as pairs.
func TestParseAndJSON(t *testing.T) {
	if _, err := Parse(`{"name": "x", "fragment": []}`); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("unknown key passed: %v", err)
	}
	spec := mustParse(t, `{"name":"x","desc":"d","params":{"p":{"default":"1","doc":"D"}},"fragments":[{"name":"f","batch":7,"nodes":[{"id":"s","plugin":"sql.read","cfg":{"conn":"a","query":"SELECT 1 WHERE 1 < 2"}},{"id":"d","plugin":"preview"}],"edges":[["s","d"]],"ui":{"s":[1,2]}}]}`)
	text, err := spec.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `["s", "d"]`) && !strings.Contains(text, "[\n        \"s\",\n        \"d\"\n      ]") {
		t.Errorf("edges not written as a pair:\n%s", text)
	}
	if strings.Contains(text, `\u003c`) {
		t.Errorf("< was escaped:\n%s", text)
	}
	again := mustParse(t, text)
	if again.Fragments[0].Batch != 7 || again.Fragments[0].Edges[0] != (Edge{"s", "d"}) || again.Params["p"].Doc != "D" {
		t.Errorf("round trip lost something: %+v", again)
	}
	if refs := Refs("a ${x} b ${frag.f.rows} ${x}"); strings.Join(refs, ",") != "x,frag.f.rows" {
		t.Errorf("Refs = %v", refs)
	}
}

// Batch helpers.
func TestBatch(t *testing.T) {
	b := NewBatch(ColsOf([]string{"id", "Name"}, []string{"INT4", "TEXT"}), [][]any{{int64(1), "a"}, {int64(2), "b"}})
	if b.Col("name") != 1 || b.Col("zz") != -1 || b.Get(1, "id") != int64(2) {
		t.Error("lookup")
	}
	b.AddCol("n2", "", func(r int) any { return r * 10 })
	b.Rename("Name", "nm")
	b.Keep("n2", "nm")
	if b.Names()[0] != "n2" || len(b.Rows[0]) != 2 || b.Rows[1][0] != 10 {
		t.Errorf("after keep: %+v", b)
	}
	c := b.Clone()
	c.Set(0, "nm", "z")
	if b.Get(0, "nm") != "a" {
		t.Error("clone shares rows")
	}
	b.Filter(func(r int) bool { return r == 1 })
	if b.Len() != 1 || b.Get(0, "nm") != "b" {
		t.Errorf("filter: %+v", b.Rows)
	}
	r := b.Result("c", "q")
	if r.Rows[0][1] != "b" || r.Raw[0][0] != 10 {
		t.Errorf("result: %+v", r)
	}
}

// Builder → Spec: ids, Then wiring, params; Gen writes a script that
// names the same nodes.
func TestBuilderAndGen(t *testing.T) {
	p := New("b").Desc("D").Param("days", "1", "how far back")
	f := p.Fragment("f").Batch(10)
	src := f.Node("sql.read", Config{"conn": "a", "query": "SELECT 1\nFROM t"})
	f.Then("cols.select", Config{"keep": "x"})
	f.Then("sql.write", Config{"conn": "b", "table": "t"})
	f.Wire(f.frag().Nodes[1].ID, f.Named("peek", "preview", nil))
	p.Fragment("g").Node("sql.exec", Config{"conn": "a", "sql": "SELECT 1"})
	spec := p.Spec()
	if src != "read" || spec.Fragments[0].Nodes[1].ID != "select" || len(spec.Fragments[0].Edges) != 3 {
		t.Errorf("spec: %+v", spec.Fragments[0])
	}
	if ds := Check(spec, CheckOptions{}); len(ds) != 0 {
		t.Errorf("built spec does not check out: %v", ds)
	}
	src2, err := Gen(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`p := sdb.NewPipeline("b").Desc("D")`, `p.Param("days", "1", "how far back")`,
		`f := p.Fragment("f").Batch(10)`, `read := f.Named("read", "sql.read", sdb.Cfg{`,
		"\"query\": `SELECT 1\nFROM t`,", `f.Wire(read, select_)`, `g.Named("exec", "sql.exec"`,
		`st, err := s.RunPipeline(p, sdb.PipelineOpts{})`,
	} {
		if !strings.Contains(src2, want) {
			t.Errorf("generated script lacks %q:\n%s", want, src2)
		}
	}
	f.ThenFunc(func(b *Batch) (*Batch, error) { return b, nil })
	if _, err := Gen(p.Spec()); err == nil {
		t.Error("a func node exported")
	}
}

// Every built-in plugin has a kind, a label, a doc, and fields that
// validate their own defaults.
func TestPluginsDescribed(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Plugins() {
		if p.Label == "" || p.Doc == "" {
			t.Errorf("%s: no label or doc", p.Name)
		}
		for _, f := range p.Fields {
			if f.Doc == "" || f.Type == "" {
				t.Errorf("%s.%s: no doc or type", p.Name, f.Name)
			}
			if f.Type == FieldEnum && len(f.Enum) == 0 {
				t.Errorf("%s.%s: enum without values", p.Name, f.Name)
			}
		}
		if msgs := p.Validate(p.Defaults(Config{})); len(msgs) > 0 {
			// only required fields may be missing from an empty config
			for _, m := range msgs {
				if !strings.HasSuffix(m, ": required") {
					t.Errorf("%s: defaults do not validate: %s", p.Name, m)
				}
			}
		}
		seen[p.Name] = true
	}
	for _, want := range []string{"sql.read", "sql.table", "sql.write", "sql.exec", "cols.select", "rows.filter", "rows.limit", "preview"} {
		if !seen[want] {
			t.Errorf("no plugin %s", want)
		}
	}
}

// Progress tells a host every fragment's life: its start (running, the
// nodes listed with nothing counted), a snapshot per batch, and its end
// with the final status — skipped included, for a fragment Options.Fragment
// leaves out — so a host can draw states from the one callback.
func TestRunProgressStartBatchesEnd(t *testing.T) {
	h := newTestHost(t)
	spec := mustParse(t, `{
	  "name": "prog",
	  "fragments": [
	    {"name": "one", "batch": 10, "nodes": [
	      {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id FROM cats ORDER BY id"}},
	      {"id": "dst", "plugin": "sql.write", "cfg": {"conn": "b", "table": "o", "create": "true"}}
	    ], "edges": [["src", "dst"]]},
	    {"name": "two", "nodes": [{"id": "x", "plugin": "sql.exec", "cfg": {"conn": "b", "sql": "SELECT 1"}}]}
	  ]}`)
	var seen []FragmentStats
	_, err := Run(context.Background(), h, spec, Options{Progress: func(f FragmentStats) { seen = append(seen, f) }})
	if err != nil {
		t.Fatal(err)
	}
	var one []FragmentStats
	for _, f := range seen {
		if f.Name == "one" {
			one = append(one, f)
		}
	}
	// 25 rows in batches of 10: a start, three batches, the stream's own
	// last report, the end
	if len(one) < 3 {
		t.Fatalf("one: %d reports", len(one))
	}
	first, last := one[0], one[len(one)-1]
	if first.Status != Running || len(first.Nodes) != 2 || first.Nodes[0].Out != 0 {
		t.Errorf("start = %+v", first)
	}
	if last.Status != Succeeded || last.Rows != 25 || last.Ended.IsZero() {
		t.Errorf("end = %+v", last)
	}
	// a snapshot is the host's: the runner counting on does not move it
	if first.Nodes[0].Out != 0 {
		t.Errorf("start snapshot moved: %+v", first.Nodes[0])
	}
	if end := seen[len(seen)-1]; end.Name != "two" || end.Status != Succeeded {
		t.Errorf("last report = %+v", end)
	}

	// a fragment left out is reported skipped
	seen = nil
	if _, err = Run(context.Background(), h, spec, Options{Fragment: "two",
		Progress: func(f FragmentStats) { seen = append(seen, f) }}); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 || seen[0].Name != "one" || seen[0].Status != Skipped {
		t.Errorf("skipped report = %+v", seen)
	}
}

// ${run.…} values: the clock ones default from the run's start, the host's
// override them, and a name that is not a run value is a check error
// rather than a failure mid-run.
func TestRunValues(t *testing.T) {
	started := time.Date(2026, 10, 9, 2, 0, 0, 0, time.Local)
	v := runValues(map[string]string{"id": "20261009-020000-7f3a", "trigger": "schedule"}, started)
	for k, want := range map[string]string{
		"run.date": "2026-10-09", "run.time": "02:00:00", "run.id": "20261009-020000-7f3a",
		"run.trigger": "schedule", "run.job": "",
	} {
		if got, ok := v[k]; !ok || got != want {
			t.Errorf("%s = %q (%v), want %q", k, got, ok, want)
		}
	}
	spec := &Spec{Name: "p", Fragments: []Fragment{{Name: "f", Nodes: []Node{
		{ID: "x", Plugin: "sql.exec", Cfg: Config{"conn": "a", "sql": "SELECT '${run.date}', '${run.dat}'"}},
	}}}}
	diags := Check(spec, CheckOptions{})
	if len(diags) != 1 || !strings.Contains(diags[0].Msg, "${run.dat} is not a run value") {
		t.Fatalf("diags = %v", diags)
	}
}

// A Progress snapshot is the host's to keep: neither its nodes nor its
// published values are the runner's own, which it goes on writing.
func TestProgressSnapshotsAreCopies(t *testing.T) {
	h := newTestHost(t)
	var snaps []FragmentStats
	spec := &Spec{Name: "p", Fragments: []Fragment{{Name: "f", Nodes: []Node{
		{ID: "x", Plugin: "sql.exec", Cfg: Config{"conn": "a", "sql": "SELECT 1"}}}}}}
	st, err := Run(context.Background(), h, spec, Options{Progress: func(f FragmentStats) { snaps = append(snaps, f) }})
	if err != nil {
		t.Fatal(err)
	}
	final := st.Fragments[0]
	for _, s := range snaps {
		if s.Vars != nil && final.Vars != nil && fmt.Sprintf("%p", s.Vars) == fmt.Sprintf("%p", final.Vars) {
			t.Fatal("a snapshot shares the runner's Vars map")
		}
	}
}
