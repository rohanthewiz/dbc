package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The plugins added with the plugin SDK: the file sources and sinks,
// cols.cast, cols.add, text.clean, rows.dedupe, discard, lookup and
// pipeline.check.

// newNode builds a plugin's node as the runner does: defaults, then New.
func newNode(t *testing.T, plugin string, cfg Config) any {
	t.Helper()
	p, ok := Lookup(plugin)
	if !ok {
		t.Fatalf("no plugin %s", plugin)
	}
	cfg = p.Defaults(cfg)
	if msgs := p.Validate(cfg); len(msgs) > 0 {
		t.Fatalf("%s: %v", plugin, msgs)
	}
	n, err := p.New(cfg)
	if err != nil {
		t.Fatalf("%s: %v", plugin, err)
	}
	return n
}

// logEnv is an Env whose Logf lines are kept.
func logEnv(lines *[]string) *Env {
	return &Env{Ctx: context.Background(), Logf: func(f string, a ...any) { *lines = append(*lines, fmt.Sprintf(f, a...)) }}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	bs, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(bs)
}

// cats → CSV → back, typed by cols.cast, into a created table: the CSV is
// what a spreadsheet expects, the path is published for the next
// fragment, and the cast columns land as integers, not text.
func TestCSVRoundTrip(t *testing.T) {
	h := newTestHost(t)
	path := filepath.Join(t.TempDir(), "sub", "cats.csv") // sub/ does not exist yet
	spec := mustParse(t, fmt.Sprintf(`{"name": "rt", "fragments": [
	  {"name": "out", "batch": 7, "nodes": [
	    {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id, name, breed, age FROM cats ORDER BY id"}},
	    {"id": "csv", "plugin": "csv.write", "cfg": {"path": %q}}
	  ], "edges": [["src", "csv"]]},
	  {"name": "in", "batch": 4, "nodes": [
	    {"id": "csv", "plugin": "csv.read", "cfg": {"path": "${frag.out.path}"}},
	    {"id": "cast", "plugin": "cols.cast", "cfg": {"casts": "id int\nage int"}},
	    {"id": "dst", "plugin": "sql.write", "cfg": {"conn": "b", "table": "out", "create": "true"}}
	  ], "edges": [["csv", "cast"], ["cast", "dst"]]}
	]}`, path))
	st, err := Run(context.Background(), h, spec, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Fragments[0].Vars["path"]; got != path {
		t.Errorf("published path %q, want %q", got, path)
	}
	text := readFile(t, path)
	if lines := strings.Split(text, "\n"); lines[0] != "id,name,breed,age" || lines[1] != "1,cat01,Siamese,1" || len(lines) != 27 {
		t.Errorf("csv: %q…", lines[:3])
	}
	if st.Fragments[1].Rows != 25 {
		t.Errorf("loaded %d rows", st.Fragments[1].Rows)
	}
	got := h.rows(t, "b", "SELECT typeof(id), typeof(age), typeof(name), sum(age) FROM out")
	if want := h.rows(t, "a", "SELECT sum(age) FROM cats")[0][0]; got[0][0] != "integer" || got[0][1] != "integer" ||
		got[0][2] != "text" || got[0][3] != want {
		t.Errorf("loaded types/sum: %v (want sum %s)", got, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("mode: %v %v", fi.Mode(), err)
	}
}

// csv.read's options: no header with named columns, a tab delimiter, a
// BOM, empty fields as NULL or as text; a ragged record is an error with
// its line; a header-only file still creates the table downstream.
func TestCSVReadOptions(t *testing.T) {
	dir := t.TempDir()
	tsv := filepath.Join(dir, "a.tsv")
	if err := os.WriteFile(tsv, []byte("\uFEFF1\tTom\t\n2\t\tx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := func(cfg Config) ([][]any, []Col, error) {
		src := newNode(t, "csv.read", cfg).(Source)
		e := &Env{Ctx: context.Background(), Batch: 1}
		if err := src.Open(e); err != nil {
			return nil, nil, err
		}
		defer src.Close(true)
		var rows [][]any
		var cols []Col
		for {
			b, err := src.Next(e)
			if err != nil || b == nil {
				return rows, cols, err
			}
			rows, cols = append(rows, b.Rows...), b.Cols
		}
	}
	rows, cols, err := read(Config{"path": tsv, "header": "false", "columns": "id, name, note", "delimiter": `\t`})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || cols[2].Name != "note" || rows[0][0] != "1" || rows[0][2] != nil || rows[1][1] != nil || rows[1][2] != "x" {
		t.Errorf("tsv: %v %v", cols, rows)
	}
	rows, cols, _ = read(Config{"path": tsv, "header": "false", "delimiter": "tab", "empty_null": "false"})
	if cols[0].Name != "c1" || rows[0][2] != "" {
		t.Errorf("unnamed / empty kept: %v %v", cols, rows)
	}
	dup := filepath.Join(dir, "dup.csv")
	_ = os.WriteFile(dup, []byte("a,a,,b\n1,2,3,4\n"), 0o644)
	if _, cols, _ = read(Config{"path": dup}); fmt.Sprint(ColsNames(cols)) != "[a a_2 c3 b]" {
		t.Errorf("names: %v", ColsNames(cols))
	}
	ragged := filepath.Join(dir, "r.csv")
	_ = os.WriteFile(ragged, []byte("a,b\n1,2\n3\n"), 0o644)
	if _, _, err = read(Config{"path": ragged}); err == nil || !strings.Contains(err.Error(), "3") ||
		!strings.Contains(err.Error(), ragged) {
		t.Errorf("ragged: %v", err)
	}

	h := newTestHost(t)
	head := filepath.Join(dir, "head.csv")
	_ = os.WriteFile(head, []byte("id,name\n"), 0o644)
	spec := mustParse(t, fmt.Sprintf(`{"name": "h", "fragments": [{"name": "f", "nodes": [
	  {"id": "src", "plugin": "csv.read", "cfg": {"path": %q}},
	  {"id": "dst", "plugin": "sql.write", "cfg": {"conn": "b", "table": "empty", "create": "true"}}
	], "edges": [["src", "dst"]]}]}`, head))
	if _, err := Run(context.Background(), h, spec, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := h.rows(t, "b", "SELECT count(*) FROM empty"); got[0][0] != "0" {
		t.Errorf("header-only: %v", got)
	}
}

// ColsNames lists a column list's names (a test helper).
func ColsNames(cols []Col) []string {
	return (&Batch{Cols: cols}).Names()
}

// jsonl.read keeps the first object's key order, types numbers as
// integers or floats, keeps nested values as JSON text, keeps an
// out-of-range integer exact, and says once which keys it dropped;
// jsonl.write writes keys in column order.
func TestJSONLRoundTrip(t *testing.T) {
	h := newTestHost(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	_ = os.WriteFile(in, []byte(`{"id": 1, "name": "a", "price": 1.5, "tags": ["x", "y"], "meta": {"k": 1}}

{"name": "b", "id": 2, "extra": true}
{"id": 9223372036854775808, "price": 2, "ok": false, "name": null}
`), 0o644)
	spec := mustParse(t, fmt.Sprintf(`{"name": "j", "fragments": [{"name": "f", "batch": 2, "nodes": [
	  {"id": "src", "plugin": "jsonl.read", "cfg": {"path": %q}},
	  {"id": "peek", "plugin": "preview", "cfg": {"rows": "10"}}
	], "edges": [["src", "peek"]]}]}`, in))
	if _, err := Run(context.Background(), h, spec, Options{}); err != nil {
		t.Fatal(err)
	}
	r := h.shown[0]
	if strings.Join(r.Columns, ",") != "id,name,price,tags,meta" {
		t.Fatalf("columns %v", r.Columns)
	}
	raw := r.Raw
	if raw[0][0] != int64(1) || raw[0][2] != 1.5 || raw[0][3] != `["x","y"]` || raw[0][4] != `{"k":1}` {
		t.Errorf("row 1: %#v", raw[0])
	}
	if raw[1][0] != int64(2) || raw[1][1] != "b" || raw[1][2] != nil {
		t.Errorf("row 2: %#v", raw[1])
	}
	if raw[2][0] != "9223372036854775808" || raw[2][2] != int64(2) || raw[2][1] != nil {
		t.Errorf("row 3: %#v", raw[2])
	}
	if log := strings.Join(h.lines, "\n"); !strings.Contains(log, "2 rows had keys outside the columns (extra, ok)") {
		t.Errorf("no extra-keys line: %s", log)
	}

	out := filepath.Join(dir, "cats.jsonl")
	spec = mustParse(t, fmt.Sprintf(`{"name": "w", "fragments": [{"name": "f", "nodes": [
	  {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT name, id, age * 1.5 AS f FROM cats WHERE id <= 2 ORDER BY id"}},
	  {"id": "dst", "plugin": "jsonl.write", "cfg": {"path": %q}}
	], "edges": [["src", "dst"]]}]}`, out))
	st, err := Run(context.Background(), h, spec, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, out); got != `{"name":"cat01","id":1,"f":1.5}`+"\n"+`{"name":"cat02","id":2,"f":3}`+"\n" {
		t.Errorf("jsonl: %q", got)
	}
	if st.Fragments[0].Rows != 2 || st.Fragments[0].Vars["rows"] != "2" {
		t.Errorf("stats: %+v", st.Fragments[0])
	}

	bad := filepath.Join(dir, "bad.jsonl")
	_ = os.WriteFile(bad, []byte("{\"a\": 1}\n[1, 2]\n"), 0o644)
	spec = mustParse(t, fmt.Sprintf(`{"name": "b", "fragments": [{"name": "f", "nodes": [
	  {"id": "src", "plugin": "jsonl.read", "cfg": {"path": %q}}, {"id": "d", "plugin": "discard"}
	], "edges": [["src", "d"]]}]}`, bad))
	if _, err = Run(context.Background(), h, spec, Options{}); err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Errorf("not an object: %v", err)
	}
}

// A fragment failing after csv.write has rows leaves an existing file as
// it was, and no temporary file behind.
func TestFileWriteFailureLeavesFile(t *testing.T) {
	h := newTestHost(t)
	dir := t.TempDir()
	for _, plugin := range []string{"csv.write", "jsonl.write"} {
		path := filepath.Join(dir, "out."+plugin)
		_ = os.WriteFile(path, []byte("old\n"), 0o644)
		p := New("w")
		f := p.Fragment("f").Batch(5)
		f.Node("sql.read", Config{"conn": "a", "query": "SELECT id, name FROM cats ORDER BY id"})
		f.ThenFunc(func(b *Batch) (*Batch, error) {
			if v, _ := b.Get(0, "id").(int64); v > 8 {
				return nil, errors.New("boom")
			}
			return b, nil
		})
		f.Then(plugin, Config{"path": path})
		if _, err := Run(context.Background(), h, p.Spec(), Options{}); err == nil {
			t.Fatalf("%s: no error", plugin)
		}
		if got := readFile(t, path); got != "old\n" {
			t.Errorf("%s: file changed: %q", plugin, got)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("left behind: %v", names)
	}
}

// Path: ~ the home directory, an absolute path as written, a relative one
// joined to files_dir — or, with none (a host that sets no FilesDir, a
// nil Env), left relative to the working directory as before.
func TestEnvPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	files := filepath.Join(t.TempDir(), "files")
	abs := filepath.Join(t.TempDir(), "x.csv")
	e := &Env{filesDir: files}
	for _, c := range []struct {
		env      *Env
		in, want string
	}{
		{e, "out.csv", filepath.Join(files, "out.csv")},
		{e, "exports/out.csv", filepath.Join(files, "exports", "out.csv")},
		{e, "../up.csv", filepath.Join(filepath.Dir(files), "up.csv")}, // cleaned, as Join does
		{e, abs, abs},
		{e, "~/x.csv", filepath.Join(home, "x.csv")},
		{e, "~", home},
		{e, "", ""}, // required, so refused before; not turned into files_dir itself
		{&Env{}, "out.csv", "out.csv"},
		{nil, "out.csv", "out.csv"},
		{nil, "~/x.csv", filepath.Join(home, "x.csv")},
	} {
		if got, err := c.env.Path(c.in); err != nil || got != c.want {
			t.Errorf("Path(%q) with files_dir %v = %q, %v; want %q", c.in, c.env != nil && c.env.filesDir != "", got, err, c.want)
		}
	}
}

// A run's relative paths are in Options.FilesDir, for all four file
// plugins: csv.write's file lands there (missing directories made, the
// path published absolute), csv.read and jsonl.read find theirs there, and
// nothing appears in the working directory. A relative file that is not
// there fails saying where it was looked for, and why there.
func TestFilesDirRun(t *testing.T) {
	h := newTestHost(t)
	files := t.TempDir()
	spec := mustParse(t, `{"name": "fd", "fragments": [
	  {"name": "out", "nodes": [
	    {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id, name FROM cats ORDER BY id"}},
	    {"id": "csv", "plugin": "csv.write", "cfg": {"path": "fd-exports/cats.csv"}}
	  ], "edges": [["src", "csv"]]},
	  {"name": "again", "nodes": [
	    {"id": "csv", "plugin": "csv.read", "cfg": {"path": "fd-exports/cats.csv"}},
	    {"id": "js", "plugin": "jsonl.write", "cfg": {"path": "fd-exports/cats.jsonl"}}
	  ], "edges": [["csv", "js"]]},
	  {"name": "back", "nodes": [
	    {"id": "js", "plugin": "jsonl.read", "cfg": {"path": "fd-exports/cats.jsonl"}},
	    {"id": "d", "plugin": "discard"}
	  ], "edges": [["js", "d"]]}
	]}`)
	st, err := Run(context.Background(), h, spec, Options{FilesDir: files})
	if err != nil {
		t.Fatal(err)
	}
	csvPath := filepath.Join(files, "fd-exports", "cats.csv")
	if got := st.Fragments[0].Vars["path"]; got != csvPath {
		t.Errorf("published path %q, want %q", got, csvPath)
	}
	if got := st.Fragments[1].Vars["path"]; got != filepath.Join(files, "fd-exports", "cats.jsonl") {
		t.Errorf("jsonl path %q", got)
	}
	if lines := strings.Count(readFile(t, csvPath), "\n"); lines != 26 {
		t.Errorf("csv has %d lines, want a header and 25 rows", lines)
	}
	if st.Fragments[1].Rows != 25 || st.Fragments[2].Nodes[0].Out != 25 {
		t.Errorf("rows: again %d, back read %d", st.Fragments[1].Rows, st.Fragments[2].Nodes[0].Out)
	}
	if _, err := os.Stat("fd-exports"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the working directory got fd-exports: %v", err)
	}

	missing := mustParse(t, `{"name": "m", "fragments": [{"name": "f", "nodes": [
	  {"id": "src", "plugin": "csv.read", "cfg": {"path": "nope.csv"}}, {"id": "d", "plugin": "discard"}
	], "edges": [["src", "d"]]}]}`)
	st, err = Run(context.Background(), h, missing, Options{FilesDir: files})
	want := "(a relative path is in files_dir, " + files + ")"
	if err == nil || !strings.Contains(err.Error(), filepath.Join(files, "nope.csv")) || !strings.Contains(err.Error(), want) {
		t.Errorf("missing: %v", err)
	}
	// the node's own error, which the run monitor shows, says it too
	if got := st.Fragments[0].Nodes[0].Error; !strings.Contains(got, want) {
		t.Errorf("node error %q", got)
	}
	// an absolute path that is not there is not about files_dir
	gone := filepath.Join(files, "gone.csv")
	spec = mustParse(t, fmt.Sprintf(`{"name": "a", "fragments": [{"name": "f", "nodes": [
	  {"id": "src", "plugin": "csv.read", "cfg": {"path": %q}}, {"id": "d", "plugin": "discard"}
	], "edges": [["src", "d"]]}]}`, gone))
	if _, err = Run(context.Background(), h, spec, Options{FilesDir: files}); err == nil || strings.Contains(err.Error(), "files_dir") {
		t.Errorf("absolute missing: %v", err)
	}
}

func TestCSVText(t *testing.T) {
	for v, want := range map[any]string{
		1000000.0: "1000000", 0.5: "0.5", 1e-7: "1e-07", 1e21: "1e+21", 0.0: "0", int64(-3): "-3",
		true: "true", "x,y": "x,y",
	} {
		if got := csvText(v, ""); got != want {
			t.Errorf("csvText(%v) = %q, want %q", v, got, want)
		}
	}
	if csvText(nil, `\N`) != `\N` || csvText([]int{1, 2}, "") != "[1,2]" || csvText([]byte{1, 255}, "") != `\x01ff` {
		t.Error("null / slice / bytes")
	}
	if got := csvText(time.Date(2024, 3, 1, 10, 0, 0, 5, time.UTC), ""); got != "2024-03-01T10:00:00.000000005Z" {
		t.Errorf("time: %s", got)
	}
}

// cols.cast: every type, blank strings to NULL, the DBType carried on,
// and on_error fail vs null.
func TestColsCast(t *testing.T) {
	cast := func(casts, onErr string, vals ...any) (*Batch, []string, error) {
		var lines []string
		tr := newNode(t, "cols.cast", Config{"casts": casts, "on_error": onErr}).(Transform)
		e := logEnv(&lines)
		_ = tr.Open(e)
		rows := make([][]any, len(vals))
		for i, v := range vals {
			rows[i] = []any{v}
		}
		b, err := tr.Apply(e, NewBatch(ColsOf([]string{"v"}, nil), rows))
		_ = tr.Close()
		return b, lines, err
	}
	col := func(b *Batch) []any {
		var out []any
		for _, r := range b.Rows {
			out = append(out, r[0])
		}
		return out
	}
	day := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		casts  string
		in     []any
		want   []any
		dbType string
	}{
		{"v int", []any{"42", " 7 ", "", "  ", nil, 2.0, true, int64(5)}, []any{int64(42), int64(7), nil, nil, nil, int64(2), int64(1), int64(5)}, "INT8"},
		{"v float", []any{"2.50", int64(3), false}, []any{2.5, 3.0, 0.0}, "FLOAT8"},
		{"v bool", []any{"Yes", "off", int64(1), "T", "0"}, []any{true, false, true, true, false}, "BOOL"},
		{"v text", []any{int64(5), "", nil}, []any{"5", "", nil}, "TEXT"},
		{"v time", []any{"2024-03-01T10:00:00Z", "2024-03-01 10:00:00"}, []any{day.Add(10 * time.Hour), day.Add(10 * time.Hour)}, "TIMESTAMPTZ"},
		{"v time 02/01/2006 15:04", []any{"01/03/2024 10:00"}, []any{day.Add(10 * time.Hour)}, "TIMESTAMPTZ"},
		{"v date", []any{"2024-03-01", "2024-03-01T23:30:00-05:00", day.Add(13 * time.Hour)}, []any{day, day, day}, "DATE"},
	} {
		b, _, err := cast(c.casts, "fail", c.in...)
		if err != nil {
			t.Errorf("%s: %v", c.casts, err)
			continue
		}
		got := col(b)
		for i := range c.want {
			if w, ok := c.want[i].(time.Time); ok {
				if g, ok := got[i].(time.Time); !ok || !g.Equal(w) {
					t.Errorf("%s [%d]: %v, want %v", c.casts, i, got[i], w)
				}
			} else if got[i] != c.want[i] {
				t.Errorf("%s [%d]: %#v, want %#v", c.casts, i, got[i], c.want[i])
			}
		}
		if b.Cols[0].DBType != c.dbType {
			t.Errorf("%s: DBType %q", c.casts, b.Cols[0].DBType)
		}
	}
	if _, _, err := cast("v int", "fail", "1", "x"); err == nil || !strings.Contains(err.Error(), "not an integer") ||
		!strings.Contains(err.Error(), "row") {
		t.Errorf("fail: %v", err)
	}
	if _, _, err := cast("v int", "fail", 2.5); err == nil {
		t.Error("2.5 cast to an int")
	}
	b, lines, err := cast("v int\nv bool", "null", "1", "x", "maybe")
	if err != nil || fmt.Sprint(col(b)) != "[true <nil> <nil>]" || len(lines) != 1 || !strings.HasPrefix(lines[0], "2 values became NULL") {
		t.Errorf("null: %v %v %v", col(b), lines, err)
	}
	if _, _, err = cast("nope int", "fail", "1"); err == nil || !strings.Contains(err.Error(), "no column nope (the columns: v)") {
		t.Errorf("missing column: %v", err)
	}
}

// cols.add: each kind of value, row() counting across batches, now() the
// same instant for every row.
func TestColsAdd(t *testing.T) {
	tr := newNode(t, "cols.add", Config{"columns": "loaded = now()\nuid = uuid()\nn = row()\nz = null\n" +
		"i = 42\nf = 1.5\nb = TRUE\nq = 'it''s'\nt = hello world\nzip = '007'\nid = 0"}).(Transform)
	_ = tr.Open(&Env{})
	mk := func() *Batch { return NewBatch(ColsOf([]string{"id"}, nil), [][]any{{int64(1)}, {int64(2)}}) }
	b1, _ := tr.Apply(&Env{}, mk())
	b2, _ := tr.Apply(&Env{}, mk())
	want := "[id loaded uid n z i f b q t zip]"
	if fmt.Sprint(b1.Names()) != want {
		t.Fatalf("names %v", b1.Names())
	}
	r := b1.Rows[0]
	if r[0] != int64(0) || r[3] != int64(1) || r[4] != nil || r[5] != int64(42) || r[6] != 1.5 || r[7] != true ||
		r[8] != "it's" || r[9] != "hello world" || r[10] != "007" {
		t.Errorf("row: %#v", r)
	}
	if b2.Rows[1][3] != int64(4) {
		t.Errorf("row() across batches: %v", b2.Rows[1][3])
	}
	if b1.Rows[0][1] != b2.Rows[1][1] {
		t.Error("now() moved between rows")
	}
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if u, _ := b1.Rows[0][2].(string); !uuidRe.MatchString(u) || u == b1.Rows[1][2] {
		t.Errorf("uuid: %v / %v", b1.Rows[0][2], b1.Rows[1][2])
	}
	if got := fmt.Sprint(b1.DBTypes()); got != "[INT8 TIMESTAMPTZ UUID INT8  INT8 FLOAT8 BOOL TEXT TEXT TEXT]" {
		t.Errorf("types %s", got)
	}
}

func TestTextClean(t *testing.T) {
	tr := newNode(t, "text.clean", Config{"collapse": "true", "case": "lower", "empty_null": "true"}).(Transform)
	b, _ := tr.Apply(&Env{}, NewBatch(ColsOf([]string{"a", "b"}, nil),
		[][]any{{"  Hello \t  World  ", int64(5)}, {"   ", nil}}))
	if b.Rows[0][0] != "hello world" || b.Rows[0][1] != int64(5) || b.Rows[1][0] != nil {
		t.Errorf("clean: %#v", b.Rows)
	}
	tr = newNode(t, "text.clean", Config{"columns": "b", "case": "upper"}).(Transform)
	b, _ = tr.Apply(&Env{}, NewBatch(ColsOf([]string{"a", "b"}, nil), [][]any{{" x ", " y "}}))
	if b.Rows[0][0] != " x " || b.Rows[0][1] != "Y" {
		t.Errorf("one column: %#v", b.Rows)
	}
	if _, err := tr.Apply(&Env{}, NewBatch(ColsOf([]string{"a"}, nil), [][]any{{"x"}})); err == nil {
		t.Error("a missing column passed")
	}
}

// rows.dedupe keeps the first of each key across batches (of 3 here);
// over whole rows, NULL and the string "NULL" are different keys.
func TestRowsDedupe(t *testing.T) {
	h := newTestHost(t)
	spec := mustParse(t, `{"name": "d", "fragments": [{"name": "f", "batch": 3, "nodes": [
	  {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id, breed FROM cats ORDER BY id"}},
	  {"id": "dd", "plugin": "rows.dedupe", "cfg": {"key": "breed"}},
	  {"id": "peek", "plugin": "preview"}
	], "edges": [["src", "dd"], ["dd", "peek"]]}]}`)
	if _, err := Run(context.Background(), h, spec, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(h.shown[0].Rows); got != "[[1 Siamese] [2 Maine Coon] [3 Tabby]]" {
		t.Errorf("deduped: %s", got)
	}
	if log := strings.Join(h.lines, "\n"); !strings.Contains(log, "dropped 22 duplicate rows") {
		t.Errorf("log: %s", log)
	}
	tr := newNode(t, "rows.dedupe", Config{}).(Transform)
	b, _ := tr.Apply(&Env{}, NewBatch(ColsOf([]string{"a", "b"}, nil),
		[][]any{{nil, "x"}, {"NULL", "x"}, {nil, "x"}, {"ab", "c"}, {"a", "bc"}}))
	if b.Len() != 4 {
		t.Errorf("whole-row keys: %v", b.Rows)
	}
}

// lookup: matches as text across types, the first of a repeated side key
// wins, and a row with no match gets NULLs, is dropped, or fails.
func TestLookup(t *testing.T) {
	h := newTestHost(t)
	h.exec(t, "b", `CREATE TABLE breeds (name TEXT, origin TEXT); INSERT INTO breeds VALUES ('Tabby', 'UK'), ('Siamese', 'Thailand'), ('Siamese', 'dup')`)
	h.exec(t, "b", `CREATE TABLE notes (cat_id TEXT, note TEXT); INSERT INTO notes VALUES ('1', 'first'), ('2', 'second')`)
	run := func(cfg string) ([][]any, error) {
		h.shown, h.lines = nil, nil
		spec := mustParse(t, `{"name": "l", "fragments": [{"name": "f", "nodes": [
		  {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id, breed FROM cats ORDER BY id"}},
		  {"id": "lk", "plugin": "lookup", "cfg": `+cfg+`},
		  {"id": "peek", "plugin": "preview", "cfg": {"rows": "100"}}
		], "edges": [["src", "lk"], ["lk", "peek"]]}]}`)
		_, err := Run(context.Background(), h, spec, Options{})
		if err != nil {
			return nil, err
		}
		return h.shown[0].Raw, nil
	}
	rows, err := run(`{"conn": "b", "query": "SELECT name, origin FROM breeds", "key": "breed", "match": "name"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 25 || rows[0][2] != "Thailand" || rows[1][2] != nil || rows[2][2] != "UK" {
		t.Errorf("null: %d rows, %v %v %v", len(rows), rows[0], rows[1], rows[2])
	}
	log := strings.Join(h.lines, "\n")
	if !strings.Contains(log, "1 side rows repeated a key") || !strings.Contains(log, "8 rows had no match and got NULLs") {
		t.Errorf("log: %s", log)
	}
	rows, err = run(`{"conn": "b", "query": "SELECT cat_id, note FROM notes", "key": "id", "match": "cat_id", "missing": "drop", "prefix": "n_"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1][2] != "second" || h.shown[0].Columns[2] != "n_note" {
		t.Errorf("drop: %v %v", h.shown[0].Columns, rows)
	}
	if _, err = run(`{"conn": "b", "query": "SELECT name, origin FROM breeds", "key": "breed", "match": "name", "missing": "fail"}`); err == nil ||
		!strings.Contains(err.Error(), "no match") {
		t.Errorf("fail: %v", err)
	}
	if _, err = run(`{"conn": "b", "query": "SELECT name AS breed, origin AS id FROM breeds", "key": "breed"}`); err == nil ||
		!strings.Contains(err.Error(), "prefix") {
		t.Errorf("clash: %v", err)
	}
	if _, err = run(`{"conn": "b", "query": "SELECT name, origin FROM breeds", "key": "breed", "match": "name", "max_rows": "2"}`); err == nil ||
		!strings.Contains(err.Error(), "max_rows") {
		t.Errorf("max_rows: %v", err)
	}
}

// discard counts what reached it; its count is the fragment's only when
// nothing was written beside it.
func TestDiscard(t *testing.T) {
	h := newTestHost(t)
	for sinks, want := range map[string]int64{
		`{"id": "d", "plugin": "discard"}`: 25,
		`{"id": "d", "plugin": "discard"}, {"id": "w", "plugin": "sql.write", "cfg": {"conn": "b", "table": "out", "create": "true", "truncate": "true"}}`: 25,
	} {
		edges := `["src", "d"]`
		if strings.Contains(sinks, `"w"`) {
			edges += `, ["src", "w"]`
		}
		spec := mustParse(t, `{"name": "x", "fragments": [{"name": "f", "nodes": [
		  {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id FROM cats"}}, `+sinks+`
		], "edges": [`+edges+`]}]}`)
		st, err := Run(context.Background(), h, spec, Options{})
		if err != nil {
			t.Fatal(err)
		}
		fs := st.Fragments[0]
		if fs.Rows != want || fs.Nodes[1].Out != 25 {
			t.Errorf("%s: rows %d, discard out %d", sinks, fs.Rows, fs.Nodes[1].Out)
		}
	}
}

// pipeline.check gates the fragments after it: a passing rule publishes
// the value; a failing one stops the pipeline with the spec's message.
func TestPipelineCheck(t *testing.T) {
	h := newTestHost(t)
	spec := func(rule string) *Spec {
		return mustParse(t, `{"name": "g", "fragments": [
		  {"name": "gate", "nodes": [{"id": "c", "plugin": "pipeline.check", "cfg": {"conn": "a",
		    "query": "SELECT count(*) FROM cats WHERE age >= ?", "args": "0", "rule": "`+rule+`", "message": "too few cats"}}]},
		  {"name": "after", "nodes": [{"id": "x", "plugin": "sql.exec", "cfg": {"conn": "b",
		    "sql": "CREATE TABLE log (n INTEGER); INSERT INTO log VALUES (${frag.gate.value})"}}]}
		]}`)
	}
	st, err := Run(context.Background(), h, spec(">= 20"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Fragments[0].Vars["value"] != "25" {
		t.Errorf("vars: %v", st.Fragments[0].Vars)
	}
	if got := h.rows(t, "b", "SELECT n FROM log"); got[0][0] != "25" {
		t.Errorf("log: %v", got)
	}
	h.exec(t, "b", "DROP TABLE log")
	_, err = Run(context.Background(), h, spec("> 100"), Options{})
	if err == nil || !strings.Contains(err.Error(), "too few cats") || !strings.Contains(err.Error(), "> 100") {
		t.Fatalf("fail: %v", err)
	}
	if got := h.rows(t, "b", "SELECT name FROM sqlite_master WHERE name = 'log'"); len(got) != 0 {
		t.Error("the fragment after a failed check ran")
	}
	// no rows is a failure too, in the default words
	empty := mustParse(t, `{"name": "e", "fragments": [{"name": "gate", "nodes": [{"id": "c", "plugin": "pipeline.check",
	  "cfg": {"conn": "a", "query": "SELECT id FROM cats WHERE id < 0", "rule": "notnull"}}]}]}`)
	if _, err = Run(context.Background(), h, empty, Options{}); err == nil || !strings.Contains(err.Error(), "no rows") {
		t.Errorf("no rows: %v", err)
	}
}

// Check reports the new plugins' config mistakes before a run.
func TestCheckNewPlugins(t *testing.T) {
	for cfg, want := range map[string]string{
		`"plugin": "cols.cast", "cfg": {"casts": "age integer"}`:                              "unknown type",
		`"plugin": "cols.cast", "cfg": {"casts": "age"}`:                                      "a cast is",
		`"plugin": "cols.cast", "cfg": {"casts": "age int 2006"}`:                             "only time and date take a layout",
		`"plugin": "cols.add", "cfg": {"columns": "x"}`:                                       "name = value",
		`"plugin": "cols.add", "cfg": {"columns": "x = foo()"}`:                               "unknown function",
		`"plugin": "cols.add", "cfg": {"columns": "x = 'abc"}`:                                "closing",
		`"plugin": "cols.add", "cfg": {"columns": " = 1"}`:                                    "needs a name",
		`"plugin": "lookup", "cfg": {"conn": "a", "query": "q", "key": "a", "match": "x, y"}`: "match: 2 columns",
		`"plugin": "csv.read", "cfg": {"path": "x", "delimiter": ";;"}`:                       "one character",
	} {
		// a transform between a source and a discard; a source straight
		// into the discard
		nodes := `{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "x"}}, {"id": "n", ` + cfg + `}, {"id": "z", "plugin": "discard"}`
		edges := `[["s", "n"], ["n", "z"]]`
		if strings.Contains(cfg, "csv.read") {
			nodes, edges = `{"id": "n", `+cfg+`}, {"id": "z", "plugin": "discard"}`, `[["n", "z"]]`
		}
		spec := mustParse(t, `{"name": "c", "fragments": [{"name": "f", "nodes": [`+nodes+`], "edges": `+edges+`}]}`)
		var msgs []string
		for _, d := range Check(spec, CheckOptions{}) {
			msgs = append(msgs, d.String())
		}
		if got := strings.Join(msgs, "\n"); !strings.Contains(got, want) {
			t.Errorf("%s: want %q in\n%s", cfg, want, got)
		}
	}
	for rule, want := range map[string]string{"~ 3": "unknown operator", "": "required", ">=": "needs a value"} {
		spec := mustParse(t, `{"name": "c", "fragments": [{"name": "g", "nodes": [{"id": "c", "plugin": "pipeline.check",
		  "cfg": {"conn": "a", "query": "q", "rule": "`+rule+`"}}]}]}`)
		var msgs []string
		for _, d := range Check(spec, CheckOptions{}) {
			msgs = append(msgs, d.String())
		}
		if got := strings.Join(msgs, "\n"); !strings.Contains(got, want) {
			t.Errorf("rule %q: want %q in\n%s", rule, want, got)
		}
	}
}
