package script

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/sdb"
)

// The clean-and-load example, from a script, on the test manager ("a"
// has the demo cats): a go.transform snippet without a package clause,
// a filter, two sinks, and a second fragment reading the first's rows.
func TestPipelineExampleFromScript(t *testing.T) {
	ex, ok := scripts.PipelineByName("clean-and-load.json")
	if !ok {
		t.Fatal("no example clean-and-load.json")
	}
	spec := strings.ReplaceAll(ex.Text, "demo-sqlite", "a")
	mgr, printed, err := runScript(t, `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	spec, err := sdb.ParsePipeline(`+"`"+spec+"`"+`)
	if err != nil {
		return err
	}
	st, err := s.RunPipelineSpec(spec, sdb.PipelineOpts{Params: sdb.Params{"min_age": "3"}})
	if err != nil {
		return err
	}
	s.Print("rows=%d status=%s direct=%v", st.Rows(), st.Status, st.Fragments[0].Direct)
	return nil
}
`)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(printed, "\n"))
	}
	joined := strings.Join(printed, "\n")
	// 5 cats are 3 or older; the transform upper-cased the breed and
	// added a label; the stamp fragment read ${frag.clean.rows}
	if !strings.Contains(joined, "rows=6 status=succeeded direct=false") {
		t.Errorf("summary: %s", joined)
	}
	if got := query(t, mgr, "a", "SELECT breed, label FROM cats_clean ORDER BY id"); len(got) != 5 ||
		got[0][0] != "TABBY" || got[0][1] != "Whiskers (3)" {
		t.Errorf("cats_clean: %v", got)
	}
	if got := query(t, mgr, "a", "SELECT rows FROM pipeline_log"); len(got) != 1 || got[0][0] != "5" {
		t.Errorf("pipeline_log: %v", got)
	}
	if !strings.Contains(joined, `DDL a: CREATE TABLE IF NOT EXISTS "cats_clean"`) {
		t.Errorf("the created table was not logged as DDL:\n%s", joined)
	}
}

// The Builder from a script: a script's own func as a transform, Then
// wiring, and a go.source whose rows come from Go.
func TestPipelineBuilderFromScript(t *testing.T) {
	mgr, printed, err := runScript(t, `package main

import (
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	p := sdb.NewPipeline("adhoc")
	f := p.Fragment("cats").Batch(4)
	f.Node("sql.read", sdb.Cfg{"conn": "a", "query": "SELECT id, name FROM cats WHERE id <= 6 ORDER BY id"})
	f.ThenFunc(func(b *sdb.Batch) (*sdb.Batch, error) {
		c := b.Col("name")
		for i := range b.Rows {
			if n, ok := b.Rows[i][c].(string); ok {
				b.Rows[i][c] = strings.ToUpper(n)
			}
		}
		b.AddCol("len", "", func(row int) any { return len(b.Get(row, "name").(string)) })
		return b, nil
	})
	f.Then("sql.write", sdb.Cfg{"conn": "b", "table": "up", "create": "true"})

	g := p.Fragment("gen").Batch(3)
	n := 0
	g.Func(func(e *sdb.Env) (*sdb.Batch, error) {
		if n >= 7 {
			return nil, nil
		}
		b := sdb.NewBatch(sdb.ColsOf([]string{"i", "sq"}, nil), nil)
		for len(b.Rows) < e.Batch && n < 7 {
			b.Rows = append(b.Rows, []any{n, n * n})
			n++
		}
		return b, nil
	})
	g.Then("sql.write", sdb.Cfg{"conn": "b", "table": "squares", "create": "true"})

	st, err := s.RunPipeline(p, sdb.PipelineOpts{})
	if err != nil {
		return err
	}
	s.Print("%s", st)
	return nil
}
`)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(printed, "\n"))
	}
	if got := query(t, mgr, "b", "SELECT name, len FROM up ORDER BY id"); len(got) != 6 || got[0][0] != "WHISKERS" || got[0][1] != "8" {
		t.Errorf("up: %v", got)
	}
	if got := query(t, mgr, "b", "SELECT i, sq FROM squares ORDER BY i"); len(got) != 7 || got[6][1] != "36" {
		t.Errorf("squares: %v", got)
	}
	if last := printed[len(printed)-1]; !strings.Contains(last, "pipeline adhoc: 2 fragments, 13 rows") {
		t.Errorf("summary: %s", last)
	}
}

// Gen's script runs and does what the spec does: the same tables, the
// same rows.
func TestGenRoundTrip(t *testing.T) {
	spec, err := pipeline.Parse(`{"name": "rt", "params": {"max": {"default": "4"}}, "fragments": [
	  {"name": "f", "batch": 2, "nodes": [
	    {"id": "src", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT id, name, breed FROM cats WHERE id <= ${max}\nORDER BY id"}},
	    {"id": "sel", "plugin": "cols.select", "cfg": {"drop": "breed", "rename": "name=cat"}},
	    {"id": "dst", "plugin": "sql.write", "cfg": {"conn": "b", "table": "rt", "create": "true"}}
	  ], "edges": [["src", "sel"], ["sel", "dst"]]},
	  {"name": "g", "nodes": [{"id": "x", "plugin": "sql.exec", "cfg": {"conn": "b", "sql": "INSERT INTO rt VALUES (99, 'from ${frag.f.rows}')"}}]}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	src, err := pipeline.Gen(spec)
	if err != nil {
		t.Fatal(err)
	}
	if diags := Check("gen.go", src); HasError(diags) {
		t.Fatalf("generated script does not check: %v\n%s", diags, src)
	}
	mgr, printed, err := runScript(t, src)
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, strings.Join(printed, "\n"), src)
	}
	got := query(t, mgr, "b", "SELECT id, cat FROM rt ORDER BY id")
	if len(got) != 5 || got[0][1] != "Whiskers" || got[4][1] != "from 4" {
		t.Errorf("rt: %v", got)
	}
}

// go.* Checks: a missing entry point, a compile error on the snippet's
// own line, a wrong signature at New, and a panic in Apply as an error.
func TestGoNodeChecks(t *testing.T) {
	p, _ := pipeline.Lookup("go.transform")
	if msgs := p.Check(pipeline.Config{"code": "func Nope() {}"}); len(msgs) == 0 || !strings.Contains(msgs[0], "no func Apply") {
		t.Errorf("missing Apply: %v", msgs)
	}
	msgs := p.Check(pipeline.Config{"code": "func Apply(b *sdb.Batch) (*sdb.Batch, error) {\n\treturn nope, nil\n}"})
	if len(msgs) == 0 || !strings.Contains(msgs[0], "code:2:") || !strings.Contains(msgs[0], "nope") {
		t.Errorf("compile error on the snippet's line: %v", msgs)
	}
	if msgs := p.Check(pipeline.Config{"code": "func Apply(b *sdb.Batch) (*sdb.Batch, error) { return b, nil }"}); len(msgs) != 0 {
		t.Errorf("clean snippet: %v", msgs)
	}
	if _, err := p.New(pipeline.Config{"code": "func Apply(x int) int { return x }"}); err == nil || !strings.Contains(err.Error(), "wrong signature") {
		t.Errorf("wrong signature: %v", err)
	}
	node, err := p.New(pipeline.Config{"code": "func Apply(b *sdb.Batch) (*sdb.Batch, error) { panic(\"oops\") }"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.(pipeline.Transform).Apply(&pipeline.Env{}, pipeline.NewBatch(nil, nil)); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("panic not an error: %v", err)
	}
	// a whole file is taken as it is
	src, header := WrapSnippet("package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Apply(b *sdb.Batch) (*sdb.Batch, error) { return b, nil }")
	if header != 0 || !strings.HasPrefix(src, "package main") {
		t.Errorf("whole file rewrapped: %d\n%s", header, src)
	}
	src, header = WrapSnippet("func Apply(b *sdb.Batch) (*sdb.Batch, error) { return b, strconv.ErrSyntax }")
	if header == 0 || !strings.Contains(src, `"strconv"`) || strings.Contains(src, `"regexp"`) {
		t.Errorf("imports: %d\n%s", header, src)
	}
}

// script.run runs a saved script as a fragment, by name from the
// session's scripts directory.
func TestScriptRunNode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mk.go"), []byte(`package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	_, err := s.Exec("b", "CREATE TABLE made (x INTEGER)")
	return err
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, _, err := runScriptPaths(t, sdb.Paths{ScriptsDir: dir}, `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	p := sdb.NewPipeline("sr")
	p.Fragment("mk").Node("script.run", sdb.Cfg{"name": "mk"})
	_, err := s.RunPipeline(p, sdb.PipelineOpts{})
	return err
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := query(t, mgr, "b", "SELECT name FROM sqlite_master WHERE name = 'made'"); len(got) != 1 {
		t.Errorf("made: %v", got)
	}
}

// A script's own relative paths are in files_dir, from each place a script
// runs: the script itself through s.Path, s.Export (which goes through it),
// a go.action node and a saved script run by a script.run node — each of
// the last two getting the run's session, and so its Paths. Nothing lands
// in the working directory, which is where each of them wrote before.
// files_dir does not exist yet, as a config's "data" does not before its
// first write: the export makes it (and exports/ under it), as csv.write
// would, and the writes after it find it there.
func TestScriptFilesDir(t *testing.T) {
	files, scriptsDir := filepath.Join(t.TempDir(), "data"), t.TempDir()
	if err := os.WriteFile(filepath.Join(scriptsDir, "saved.go"), []byte(`package main

import (
	"os"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	return os.WriteFile(s.Path("fd-saved.txt"), []byte("saved"), 0o644)
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// the go.action's code is a snippet: wrapped, os imported for it
	_, printed, err := runScriptPaths(t, sdb.Paths{FilesDir: files, ScriptsDir: scriptsDir}, `package main

import (
	"os"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	r, err := s.Query("a", "SELECT id, name FROM cats ORDER BY id LIMIT 2")
	if err != nil {
		return err
	}
	if err = s.Export(r, "csv", "exports/fd-export.csv"); err != nil {
		return err
	}
	if err := os.WriteFile(s.Path("fd-own.txt"), []byte("own"), 0o644); err != nil {
		return err
	}
	p := sdb.NewPipeline("fd")
	p.Fragment("act").Node("go.action", sdb.Cfg{"code": "func Run(s *sdb.S) error {\n" +
		"\treturn os.WriteFile(s.Path(\"fd-action.txt\"), []byte(\"action\"), 0o644)\n}"})
	p.Fragment("run").Node("script.run", sdb.Cfg{"name": "saved"})
	if _, err = s.RunPipeline(p, sdb.PipelineOpts{}); err != nil {
		return err
	}
	s.Print("%s", s.Path("fd-own.txt"))
	return nil
}
`)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(printed, "\n"))
	}
	for name, want := range map[string]string{
		"fd-own.txt":            "own",
		"exports/fd-export.csv": "id,name\n1,Whiskers\n",
		"fd-action.txt":         "action",
		"fd-saved.txt":          "saved",
	} {
		got, err := os.ReadFile(filepath.Join(files, name))
		if err != nil {
			t.Errorf("%s not in files_dir: %v", name, err)
		} else if !strings.HasPrefix(string(got), want) {
			t.Errorf("%s = %q, want it to start %q", name, got, want)
		}
		// the old behaviour's file, in the test's working directory
		if _, err := os.Stat(name); err == nil {
			os.Remove(name)
			t.Errorf("%s was written to the working directory", name)
		}
	}
	if want := filepath.Join(files, "fd-own.txt"); !slices.Contains(printed, want) {
		t.Errorf("s.Path printed %q, want %q among them", printed, want)
	}
}

// A script that runs a pipeline with a files_dir of its own
// (PipelineOpts.FilesDir) gets every file of the run there: the file
// nodes' and, through the run's view of the session (sdb.S.ForFiles), a
// go.action's and a script.run's own s.Path (N-200). The view is still the
// session: DDL a node runs is in the host's CatalogChanged.
func TestScriptPipelineOwnFilesDir(t *testing.T) {
	files, other, scriptsDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(scriptsDir, "saved.go"), []byte(`package main

import (
	"os"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	return os.WriteFile(s.Path("own-saved.txt"), []byte("saved"), 0o644)
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, printed, err := runScriptPaths(t, sdb.Paths{FilesDir: files, ScriptsDir: scriptsDir}, `package main

import (
	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	p := sdb.NewPipeline("own")
	p.Fragment("act").Node("go.action", sdb.Cfg{"code": "func Run(s *sdb.S) error {\n" +
		"\tif _, err := s.Exec(\"a\", \"CREATE TABLE own_ddl (x INT)\"); err != nil {\n\t\treturn err\n\t}\n" +
		"\treturn os.WriteFile(s.Path(\"own-action.txt\"), []byte(\"action\"), 0o644)\n}"})
	p.Fragment("run").Node("script.run", sdb.Cfg{"name": "saved"})
	f := p.Fragment("rows")
	f.Node("sql.read", sdb.Cfg{"conn": "a", "query": "SELECT id FROM cats ORDER BY id LIMIT 1"})
	f.Then("csv.write", sdb.Cfg{"path": "own-rows.csv"})
	if _, err := s.RunPipeline(p, sdb.PipelineOpts{FilesDir: "`+other+`"}); err != nil {
		return err
	}
	s.Print("%s %v", s.Path("x"), s.CatalogChanged("a"))
	return nil
}
`)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(printed, "\n"))
	}
	for _, name := range []string{"own-action.txt", "own-saved.txt", "own-rows.csv"} {
		if _, err := os.Stat(filepath.Join(other, name)); err != nil {
			t.Errorf("%s not in the run's files_dir: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(files, name)); err == nil {
			t.Errorf("%s was written to the host's files_dir", name)
		}
	}
	// the script's own session kept the host's files_dir; the node's DDL
	// is on the host's record
	if want := filepath.Join(files, "x") + " true"; !slices.Contains(printed, want) {
		t.Errorf("printed %q, want %q among them", printed, want)
	}
}

// runScriptPaths is runScript with the session's Paths set.
func runScriptPaths(t *testing.T, paths sdb.Paths, src string) (*db.Manager, []string, error) {
	t.Helper()
	dir := t.TempDir()
	mgr := db.NewManager(&config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "a", Driver: "sqlite", DSN: filepath.Join(dir, "a.db")},
		{Name: "b", Driver: "sqlite", DSN: filepath.Join(dir, "b.db")},
	}})
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, "a"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	var printed []string
	s := sdb.New(mgr, func(*model.Result) {}, func(line string) { printed = append(printed, line) }).WithPaths(paths)
	return mgr, printed, Run(path, s)
}
