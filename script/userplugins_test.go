package script

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
)

// pluginDir writes files (name → source) into a fresh directory, loads it
// as the plugins directory, and unloads every user plugin when the test
// ends — the registry is process-wide, and the next test must not see
// this one's plugins.
func pluginDir(t *testing.T, files map[string]string) (string, []pipeline.PluginProblem) {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { LoadPlugins("") })
	return dir, LoadPlugins(dir)
}

// tagPlugin counts the rows it has seen in a package-level var and writes
// the count into a column named by its config: two nodes of it in one
// fragment must each count from 1, which they do only if each node has an
// interpreter (and so globals) of its own.
const tagPlugin = `//go:build ignore

// Tags each row with how many rows this node has seen.
package main

import "github.com/rohanthewiz/dbc/sdb"

var Plugin = sdb.Plugin{
	Name: "test.tag", Kind: sdb.KindTransform, Label: "Tag rows",
	Fields: []sdb.Field{
		{Name: "column", Type: sdb.FieldString, Required: true, Doc: "Where the count goes."},
		{Name: "step", Type: sdb.FieldInt, Default: "1", Doc: "Added per row."},
	},
}

var seen int

func Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error) {
	step, err := e.Cfg.Int("step", 1)
	if err != nil {
		return nil, err
	}
	col := e.Cfg.Str("column", "n")
	b.AddCol(col, "INT8", nil)
	j := b.Col(col)
	for i := range b.Rows {
		seen += step
		b.Rows[i][j] = int64(seen)
	}
	return b, nil
}
`

// A transform plugin from a file: listed with its file and the doc from
// its comment, placed twice in one fragment, each node with its own
// globals and its own settings.
func TestUserPluginTransform(t *testing.T) {
	dir, probs := pluginDir(t, map[string]string{"tag.go": tagPlugin})
	if len(probs) > 0 {
		t.Fatalf("problems: %+v", probs)
	}
	p, ok := pipeline.Lookup("test.tag")
	if !ok || p.File != filepath.Join(dir, "tag.go") || p.Kind != pipeline.KindTransform {
		t.Fatalf("lookup: %+v %v", p, ok)
	}
	if p.Doc != "Tags each row with how many rows this node has seen." {
		t.Errorf("doc from the comment: %q", p.Doc)
	}
	mgr, printed, err := runScript(t, `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	p := sdb.NewPipeline("tags")
	f := p.Fragment("cats").Batch(3)
	f.Node("sql.read", sdb.Cfg{"conn": "a", "query": "SELECT id FROM cats ORDER BY id"})
	f.Then("test.tag", sdb.Cfg{"column": "one"})
	f.Then("test.tag", sdb.Cfg{"column": "two", "step": "10"})
	f.Then("sql.write", sdb.Cfg{"conn": "b", "table": "tagged", "create": "true"})
	_, err := s.RunPipeline(p, sdb.PipelineOpts{})
	return err
}
`)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(printed, "\n"))
	}
	got := query(t, mgr, "b", "SELECT id, one, two FROM tagged ORDER BY id")
	if len(got) != 8 || got[0][1] != "1" || got[0][2] != "10" || got[7][1] != "8" || got[7][2] != "80" {
		t.Errorf("each node counts on its own: %v", got)
	}
}

// One plugin of every other kind: a source with Open and Close(ok), a
// sink whose Commit reports its own stats, an action publishing a value
// the next fragment reads.
func TestUserPluginKinds(t *testing.T) {
	_, probs := pluginDir(t, map[string]string{
		"count.go": `package main

import "github.com/rohanthewiz/dbc/sdb"

var Plugin = sdb.Plugin{Name: "test.count", Kind: sdb.KindSource, Label: "Count",
	Fields: []sdb.Field{{Name: "to", Type: sdb.FieldInt, Required: true, Doc: "The last number."}}}

var n, to int

func Open(e *sdb.Env) error {
	var err error
	to, err = e.Cfg.Int("to", 0)
	return err
}

func Next(e *sdb.Env) (*sdb.Batch, error) {
	if n >= to {
		return nil, nil
	}
	b := sdb.NewBatch(sdb.ColsOf([]string{"n"}, []string{"INT8"}), nil)
	for len(b.Rows) < e.Batch && n < to {
		n++
		b.Rows = append(b.Rows, []any{int64(n)})
	}
	return b, nil
}

func Close(ok bool) error {
	if !ok {
		panic("a clean run closes with ok")
	}
	return nil
}
`,
		"sum.go": `package main

import (
	"fmt"

	"github.com/rohanthewiz/dbc/sdb"
)

var Plugin = sdb.Plugin{Name: "test.sum", Kind: sdb.KindSink, Doc: "Adds the n column up."}

var total int64

func Write(e *sdb.Env, b *sdb.Batch) error {
	for i := range b.Rows {
		total += b.Get(i, "n").(int64)
	}
	return nil
}

func Commit(e *sdb.Env) (sdb.Stats, error) {
	return sdb.Stats{Rows: total, Note: fmt.Sprintf("sum %d", total), Vars: map[string]string{"sum": fmt.Sprint(total)}}, nil
}
`,
		"say.go": `package main

import "github.com/rohanthewiz/dbc/sdb"

var Plugin = sdb.Plugin{Name: "test.say", Kind: sdb.KindAction, Doc: "Prints its text.",
	Fields: []sdb.Field{{Name: "text", Type: sdb.FieldString, Doc: "What to say."}}}

func Run(e *sdb.Env) (sdb.Stats, error) {
	e.S.Print("say: %s", e.Cfg.Str("text", ""))
	return sdb.Stats{Vars: map[string]string{"said": e.Cfg.Str("text", "")}}, nil
}
`,
	})
	if len(probs) > 0 {
		t.Fatalf("problems: %+v", probs)
	}
	_, printed, err := runScript(t, `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	p := sdb.NewPipeline("kinds")
	f := p.Fragment("numbers").Batch(4)
	f.Node("test.count", sdb.Cfg{"to": "10"})
	f.Then("test.sum", nil)
	p.Fragment("tell").Node("test.say", sdb.Cfg{"text": "sum was ${frag.numbers.sum}"})
	st, err := s.RunPipeline(p, sdb.PipelineOpts{})
	if err != nil {
		return err
	}
	s.Print("rows=%d said=%s", st.Fragments[0].Rows, st.Fragments[1].Vars["said"])
	return nil
}
`)
	joined := strings.Join(printed, "\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, joined)
	}
	if !strings.Contains(joined, "say: sum was 55") || !strings.Contains(joined, "rows=55 said=sum was 55") {
		t.Errorf("output:\n%s", joined)
	}
}

// Files that cannot be plugins are listed with why, and cannot be placed:
// a check of a spec naming one says the file did not load.
func TestUserPluginProblems(t *testing.T) {
	dir, probs := pluginDir(t, map[string]string{
		"syntax.go":   "package main\n\nfunc (\n",
		"novar.go":    "package main\n\nfunc Apply() {}\n",
		"kind.go":     `package main` + "\n\n" + `import "github.com/rohanthewiz/dbc/sdb"` + "\n\n" + `var Plugin = sdb.Plugin{Name: "test.k", Kind: "sideways"}` + "\n",
		"noapply.go":  `package main` + "\n\n" + `import "github.com/rohanthewiz/dbc/sdb"` + "\n\n" + `var Plugin = sdb.Plugin{Name: "test.noapply", Kind: sdb.KindTransform}` + "\n",
		"badopen.go":  `package main` + "\n\n" + `import "github.com/rohanthewiz/dbc/sdb"` + "\n\n" + `var Plugin = sdb.Plugin{Name: "test.badopen", Kind: sdb.KindTransform}` + "\n\nfunc Apply(b *sdb.Batch) (*sdb.Batch, error) { return b, nil }\n\nfunc Open() error { return nil }\n",
		"builtin.go":  `package main` + "\n\n" + `import "github.com/rohanthewiz/dbc/sdb"` + "\n\n" + `var Plugin = sdb.Plugin{Name: "sql.read", Kind: sdb.KindAction}` + "\n\nfunc Run(e *sdb.Env) error { return nil }\n",
		"a-twice.go":  `package main` + "\n\n" + `import "github.com/rohanthewiz/dbc/sdb"` + "\n\n" + `var Plugin = sdb.Plugin{Name: "test.twice", Kind: sdb.KindAction}` + "\n\nfunc Run(e *sdb.Env) error { return nil }\n",
		"b-twice.go":  `package main` + "\n\n" + `import "github.com/rohanthewiz/dbc/sdb"` + "\n\n" + `var Plugin = sdb.Plugin{Name: "test.twice", Kind: sdb.KindAction}` + "\n\nfunc Run(e *sdb.Env) error { return nil }\n",
		"badfield.go": `package main` + "\n\n" + `import "github.com/rohanthewiz/dbc/sdb"` + "\n\n" + `var Plugin = sdb.Plugin{Name: "test.f", Kind: sdb.KindAction, Fields: []sdb.Field{{Name: "x", Type: "colour"}}}` + "\n\nfunc Run(e *sdb.Env) error { return nil }\n",
	})
	want := map[string]string{
		"syntax.go":   "expected",
		"novar.go":    "no var Plugin",
		"kind.go":     `Plugin.Kind "sideways"`,
		"noapply.go":  "no func Apply",
		"badopen.go":  "func Open has the wrong signature",
		"builtin.go":  "a plugin built into dbc is called sql.read",
		"b-twice.go":  "test.twice is already the plugin of " + filepath.Join(dir, "a-twice.go"),
		"badfield.go": `Type "colour"`,
	}
	got := map[string]string{}
	for _, p := range probs {
		got[filepath.Base(p.File)] = p.Err
	}
	for file, w := range want {
		if !strings.Contains(got[file], w) {
			t.Errorf("%s: want %q in %q", file, w, got[file])
		}
	}
	if _, ok := pipeline.Lookup("test.twice"); !ok {
		t.Error("the first of two files with one name still loads")
	}
	if len(got) != len(want) {
		t.Errorf("problems: %v", got)
	}
	spec, err := pipeline.Parse(`{"name": "p", "fragments": [{"name": "f", "nodes": [{"id": "x", "plugin": "test.noapply"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	ds := pipeline.Check(spec, pipeline.CheckOptions{})
	if len(ds) == 0 || !strings.Contains(ds[0].Msg, `plugin "test.noapply" did not load from `+filepath.Join(dir, "noapply.go")) {
		t.Errorf("check: %v", ds)
	}
}

// SyncPlugins reloads only when the directory changed, and a reload
// swaps the set: a removed file's plugin is gone, an edited one's new
// version is in.
func TestSyncPlugins(t *testing.T) {
	dir, _ := pluginDir(t, map[string]string{"tag.go": tagPlugin})
	if SyncPlugins(dir) {
		t.Error("nothing changed, yet it reloaded")
	}
	edited := strings.Replace(tagPlugin, `Label: "Tag rows"`, `Label: "Tag the rows"`, 1)
	if err := os.WriteFile(filepath.Join(dir, "tag.go"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if !SyncPlugins(dir) {
		t.Fatal("an edited file was not reloaded")
	}
	if p, _ := pipeline.Lookup("test.tag"); p.Label != "Tag the rows" {
		t.Errorf("label after the edit: %q", p.Label)
	}
	if err := os.Remove(filepath.Join(dir, "tag.go")); err != nil {
		t.Fatal(err)
	}
	if !SyncPlugins(dir) {
		t.Fatal("a removed file was not noticed")
	}
	if _, ok := pipeline.Lookup("test.tag"); ok {
		t.Error("a removed file's plugin is still registered")
	}
	if _, ok := pipeline.Lookup("sql.read"); !ok {
		t.Error("the built-ins went with it")
	}
}

// A plugin's own Check runs in pipeline.Check, on the node's config.
func TestUserPluginCheckFunc(t *testing.T) {
	_, probs := pluginDir(t, map[string]string{"pick.go": `package main

import (
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

var Plugin = sdb.Plugin{Name: "test.pick", Kind: sdb.KindTransform, Doc: "Picks.",
	Fields: []sdb.Field{{Name: "mode", Type: sdb.FieldString, Doc: "a or b"}}}

func Check(cfg sdb.Cfg) []string {
	if m := cfg.Str("mode", "a"); m != "a" && m != "b" {
		return []string{"mode: " + strings.ToUpper(m) + " is neither a nor b"}
	}
	return nil
}

func Apply(b *sdb.Batch) (*sdb.Batch, error) { return b, nil }
`})
	if len(probs) > 0 {
		t.Fatalf("problems: %+v", probs)
	}
	spec, err := pipeline.Parse(`{"name": "p", "fragments": [{"name": "f", "nodes": [
		{"id": "s", "plugin": "sql.read", "cfg": {"conn": "a", "query": "SELECT 1"}},
		{"id": "x", "plugin": "test.pick", "cfg": {"mode": "c"}}, {"id": "d", "plugin": "preview"}],
		"edges": [["s", "x"], ["x", "d"]]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	ds := pipeline.Check(spec, pipeline.CheckOptions{})
	if len(ds) != 1 || ds[0].Where != "f/x" || ds[0].Msg != "mode: C is neither a nor b" {
		t.Errorf("check: %v", ds)
	}
}

// CheckPlugin: the editor's check, without running the file.
func TestCheckPlugin(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"clean", tagPlugin, ""},
		{"package", "package tools\n", "a plugin file is package main"},
		{"no var", "package main\n\nfunc Apply() {}\n", "no var Plugin"},
		{"no apply", "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nvar Plugin = sdb.Plugin{Name: \"x.y\", Kind: sdb.KindTransform}\n",
			"5:5: no func Apply"},
		{"apply sig", "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nvar Plugin = sdb.Plugin{Name: \"x.y\", Kind: sdb.KindTransform}\n\nfunc Apply(rows []any) error { return nil }\n",
			"7:6: Apply must be func(*sdb.Batch) (*sdb.Batch, error) or func(*sdb.Env, *sdb.Batch) (*sdb.Batch, error), not func([]any) error"},
		{"renamed import", "package main\n\nimport db \"github.com/rohanthewiz/dbc/sdb\"\n\nvar Plugin = db.Plugin{Name: \"x.y\", Kind: db.KindSink}\n\nfunc Write(e *db.Env, b *db.Batch) error { return nil }\n", ""},
		{"builtin name", "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nvar Plugin = sdb.Plugin{Name: \"preview\", Kind: sdb.KindAction}\n\nfunc Run(e *sdb.Env) error { return nil }\n",
			"5:31: a plugin built into dbc is called preview"},
		{"compile", "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nvar Plugin = sdb.Plugin{Name: \"x.y\", Kind: sdb.KindAction}\n\nfunc Run(e *sdb.Env) error { return nope }\n",
			"undefined: nope"},
	}
	for _, c := range cases {
		ds := CheckPlugin("p.go", c.src)
		var got []string
		for _, d := range ds {
			got = append(got, d.String())
		}
		joined := strings.Join(got, "\n")
		if c.want == "" && len(ds) > 0 || c.want != "" && !strings.Contains(joined, c.want) {
			t.Errorf("%s: want %q, got:\n%s", c.name, c.want, joined)
		}
	}
}

// go.sink: Write per batch, the default stats without a Commit.
func TestGoSink(t *testing.T) {
	_, printed, err := runScript(t, `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	p := sdb.NewPipeline("gosink")
	f := p.Fragment("cats").Batch(3)
	f.Node("sql.read", sdb.Cfg{"conn": "a", "query": "SELECT name FROM cats ORDER BY id"})
	f.Then("go.sink", sdb.Cfg{"code": "var names []string\n\nfunc Write(e *sdb.Env, b *sdb.Batch) error {\n\tfor i := range b.Rows {\n\t\tnames = append(names, b.Get(i, \"name\").(string))\n\t}\n\te.S.Print(\"got %d\", len(names))\n\treturn nil\n}\n"})
	st, err := s.RunPipeline(p, sdb.PipelineOpts{})
	if err != nil {
		return err
	}
	s.Print("rows=%d", st.Rows())
	return nil
}
`)
	joined := strings.Join(printed, "\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, joined)
	}
	if !strings.Contains(joined, "got 8") || !strings.Contains(joined, "rows=8") {
		t.Errorf("output:\n%s", joined)
	}
}

// The example plugins (scripts/plugins, one per kind) pass the editor's
// check, load, and work together: a series of numbers, an address added
// and masked, the rows posted to a webhook, then a gate on a file.
func TestPluginExamples(t *testing.T) {
	files := map[string]string{}
	for _, ex := range scripts.PluginExamples() {
		files[ex.Name] = ex.Text
		if ds := CheckPlugin(ex.Name, ex.Text); len(ds) > 0 {
			t.Errorf("%s: %v", ex.Name, ds)
		}
	}
	if len(files) != 4 {
		t.Fatalf("examples: %d", len(files))
	}
	_, probs := pluginDir(t, files)
	if len(probs) > 0 {
		t.Fatalf("problems: %+v", probs)
	}
	if p, _ := pipeline.Lookup("mask.email"); !strings.HasPrefix(p.Doc, "Masks an e-mail column: the part before the @") ||
		strings.Contains(p.Doc, "plugins_dir") {
		t.Errorf("doc is the comment's first paragraph: %q", p.Doc)
	}
	var posted struct {
		Fragment string           `json:"fragment"`
		Rows     []map[string]any `json:"rows"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(bs, &posted); err != nil || r.Header.Get("X-Token") != "s3cret" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "done")
	if err := os.WriteFile(marker, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, printed, err := runScript(t, `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	p := sdb.NewPipeline("examples")
	f := p.Fragment("post").Batch(7)
	f.Node("gen.series", sdb.Cfg{"to": "20", "column": "id"})
	f.Then("cols.add", sdb.Cfg{"columns": "email = 'Some.One@Example.COM'"})
	f.Then("mask.email", sdb.Cfg{"column": "email", "salt": "pepper"})
	f.Then("webhook.post", sdb.Cfg{"url": "`+srv.URL+`", "headers": "X-Token: s3cret"})
	p.Fragment("gate").Node("wait.file", sdb.Cfg{"path": "`+marker+`", "every": "10ms"})
	st, err := s.RunPipeline(p, sdb.PipelineOpts{})
	if err != nil {
		return err
	}
	for _, n := range st.Fragments[0].Nodes { s.Print("node %s in=%d out=%d err=%s", n.ID, n.In, n.Out, n.Error) }
	s.Print("rows=%d size=%s", st.Fragments[0].Rows, st.Fragments[1].Vars["size"])
	return nil
}
`)
	joined := strings.Join(printed, "\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, joined)
	}
	if !strings.Contains(joined, "rows=20 size=2") || len(posted.Rows) != 20 || posted.Fragment != "post" {
		t.Fatalf("output:\n%s\nposted %d rows", joined, len(posted.Rows))
	}
	email, _ := posted.Rows[0]["email"].(string)
	if !strings.HasSuffix(email, "@example.com") || len(email) != len("0123456789@example.com") || email == posted.Rows[0]["id"] {
		t.Errorf("masked: %q", email)
	}
}

// The interpreter bug lintStoreComputed warns about, pinned: an
// operator's result stored straight into a row the interpreter was handed
// clobbers the batch, so Apply returns nil and the rows are dropped — and
// the runner says so once. If a yaegi upgrade fixes it, this test fails:
// then the lint, and the workaround in the docs, can go.
func TestYaegiStoreComputedBug(t *testing.T) {
	_, printed, err := runScript(t, `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	p := sdb.NewPipeline("bug")
	f := p.Fragment("cats").Batch(3)
	f.Node("sql.read", sdb.Cfg{"conn": "a", "query": "SELECT id, name FROM cats ORDER BY id"})
	f.Then("go.transform", sdb.Cfg{"code": "func Apply(b *sdb.Batch) (*sdb.Batch, error) {\n\tfor i := range b.Rows {\n\t\tb.Rows[i][1] = b.Rows[i][1].(string) + \"!\"\n\t}\n\treturn b, nil\n}\n"})
	f.Then("discard", nil)
	st, err := s.RunPipeline(p, sdb.PipelineOpts{})
	if err != nil {
		return err
	}
	s.Print("out=%d", st.Fragments[0].Nodes[1].Out)
	return nil
}
`)
	joined := strings.Join(printed, "\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, joined)
	}
	if !strings.Contains(joined, "out=0") {
		t.Errorf("the interpreter no longer drops the batch — lintStoreComputed may be obsolete:\n%s", joined)
	}
	if n := strings.Count(joined, "Apply returned no batch"); n != 1 {
		t.Errorf("the drop is said once per node, not %d times:\n%s", n, joined)
	}
}

// lintStoreComputed: the row shapes with an operator's result, and not a
// comparison, a call, a variable, or a slice of the code's own.
func TestLintStoreComputed(t *testing.T) {
	src := `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	r, _ := s.Query("a", "SELECT 1, 'x'")
	n, v := int64(1), "y"
	r.Raw[0][0] = n + 1
	r.Raw[0][1] = -n
	r.Raw[0][1] = n == 1
	r.Raw[0][1] = v
	r.Raw[0][1] = any(v + "!")
	mine := []int64{1}
	mine[0] = n + 1
	return nil
}
`
	var lines []int
	for _, d := range Check("s.go", src) {
		if strings.Contains(d.Msg, "mis-stores") {
			lines = append(lines, d.Line)
		}
	}
	if len(lines) != 2 || lines[0] != 8 || lines[1] != 9 {
		t.Errorf("flagged lines %v, want [8 9]", lines)
	}
	if ds := checkSnippet("func Apply(b *sdb.Batch) (*sdb.Batch, error) {\n\tb.Rows[0][0] = 1 + 1\n\treturn b, nil\n}\n", "Apply"); len(ds) != 1 ||
		!strings.HasPrefix(ds[0], "code:2:2: warning: the script interpreter mis-stores") {
		t.Errorf("snippet: %v", ds)
	}
}
