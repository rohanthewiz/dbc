package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/sdb/sdbapi"
)

// scriptEnv is a test server whose scripts_dir is a fresh, not yet created
// directory: the first save makes it, as for a new install.
func scriptEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scripts")
	return newTestEnv(t, func(c *config.Config, _ *Options) { c.ScriptsDir = dir }), dir
}

// putScript saves text as name from base, through the route, and returns
// the answer.
func (e *testEnv) putScript(name, text, base string, want int) scriptSaved {
	e.t.Helper()
	b, _ := json.Marshal(scriptSave{Text: text, Base: base})
	env := e.api("PUT", "/api/v1/scripts/"+name+"?win=w1", string(b), want)
	if want != 200 {
		return scriptSaved{}
	}
	return decodeData[scriptSaved](e.t, env)
}

// awaitScripts reads the window stream up to the next "scripts" event.
func awaitScripts(t *testing.T, s *stream) scriptsEvent {
	t.Helper()
	ev, _ := s.await(t, "scripts")
	var se scriptsEvent
	if err := json.Unmarshal(ev.Data, &se); err != nil {
		t.Fatal(err)
	}
	return se
}

const okScript = `// Count the cats.
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	s.Print("hi")
	return nil
}
`

// A save names its base revision. A create (base "") never overwrites; a
// stale base is a conflict that carries the file's text and leaves the
// file alone; a file deleted since is a conflict with rev "". Every save
// that lands is told to the windows.
func TestScriptSaveRevisions(t *testing.T) {
	e, dir := scriptEnv(t)
	_, s := e.open()

	e.api("GET", "/api/v1/scripts/cats.go", "", 404)
	r1 := e.putScript("cats.go", okScript, "", 200)
	if r1.Conflict || r1.Rev == "" {
		t.Fatalf("create = %+v", r1)
	}
	if ev := awaitScripts(t, s); ev.Op != "saved" || ev.Name != "cats.go" || ev.Rev != r1.Rev || ev.Win != "w1" {
		t.Errorf("save event = %+v", ev)
	}
	got := decodeData[scriptText](t, e.api("GET", "/api/v1/scripts/cats.go", "", 200))
	if got.Text != okScript || got.Rev != r1.Rev {
		t.Fatalf("read back = %+v", got)
	}
	list := decodeData[scriptsList](t, e.api("GET", "/api/v1/scripts", "", 200))
	if len(list.Scripts) != 1 || list.Scripts[0].Desc != "Count the cats." || list.Scripts[0].Rev != r1.Rev {
		t.Errorf("list = %+v", list.Scripts)
	}

	// a second create of the name is refused, not merged
	e.putScript("cats.go", "package main", "", 409)

	r2 := e.putScript("cats.go", okScript+"// v2\n", r1.Rev, 200)
	if r2.Conflict || r2.Rev == r1.Rev {
		t.Fatalf("save from the current rev = %+v", r2)
	}
	// a window still on r1: refused, told what the file holds
	stale := e.putScript("cats.go", "// mine\n", r1.Rev, 200)
	if !stale.Conflict || stale.Rev != r2.Rev || stale.Text != okScript+"// v2\n" {
		t.Fatalf("stale save = %+v", stale)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cats.go")); string(b) != okScript+"// v2\n" {
		t.Fatalf("a stale save wrote the file: %q", b)
	}

	// deleted behind the page's back (vim, rm): a conflict with rev ""
	if err := os.Remove(filepath.Join(dir, "cats.go")); err != nil {
		t.Fatal(err)
	}
	gone := e.putScript("cats.go", "// mine\n", r2.Rev, 200)
	if !gone.Conflict || gone.Rev != "" || gone.Text != "" {
		t.Fatalf("save over a deleted file = %+v", gone)
	}
	// and base "" puts it back
	if back := e.putScript("cats.go", "// mine\n", "", 200); back.Conflict {
		t.Fatalf("re-create = %+v", back)
	}
}

// Nothing from the browser reaches the disk but a script name in
// scripts_dir: a traversal, a hidden name or a missing .go is a 400 before
// any file is touched.
func TestScriptNamesChecked(t *testing.T) {
	e, dir := scriptEnv(t)
	for _, n := range []string{"..%2Fx.go", ".hidden.go", "cats", "a%2Fb.go", "a..go"} {
		e.api("GET", "/api/v1/scripts/"+n, "", 400)
		e.api("PUT", "/api/v1/scripts/"+n, `{"text":"x"}`, 400)
		e.api("DELETE", "/api/v1/scripts/"+n, "", 400)
		e.api("POST", "/api/v1/scripts/"+n+"/rename", `{"to":"ok.go"}`, 400)
	}
	e.putScript("ok.go", okScript, "", 200)
	e.api("POST", "/api/v1/scripts/ok.go/rename", `{"to":"../up.go"}`, 400)
	e.api("POST", "/api/v1/scripts/ok.go/rename", `{"to":"up"}`, 400)
	if _, err := os.Stat(filepath.Join(dir, "..", "x.go")); !os.IsNotExist(err) {
		t.Errorf("a file appeared outside scripts_dir: %v", err)
	}
	// too big to be a script
	e.putScript("big.go", strings.Repeat("x", 1<<20+1), "", 400)
}

// No route shadows a script: a name that shares its first letters with a
// route word (the router does not backtrack; see scripts.go) is a script
// like any other, on every route.
func TestScriptNamesBesideRouteWords(t *testing.T) {
	e, _ := scriptEnv(t)
	for _, n := range []string{"check.go", "api.go", "trash.go", "examples.go", "templates.go", "export_report.go", "script-check.go", "rename.go"} {
		r := e.putScript(n, okScript, "", 200)
		if got := decodeData[scriptText](t, e.api("GET", "/api/v1/scripts/"+n, "", 200)); got.Rev != r.Rev {
			t.Errorf("GET %s = %+v", n, got)
		}
		e.api("POST", "/api/v1/scripts/"+n+"/rename", `{"to":"x_`+n+`"}`, 200)
		e.api("DELETE", "/api/v1/scripts/x_"+n, "", 200)
	}
	// an example's name is only a name: the user's own copy is a script,
	// and no copy is not found (examples have their own route)
	ex := scripts.Examples()[0].Name
	e.api("GET", "/api/v1/scripts/"+ex, "", 404)
	e.putScript(ex, okScript, "", 200)
	if got := decodeData[scriptText](t, e.api("GET", "/api/v1/scripts/"+ex, "", 200)); got.Text != okScript {
		t.Errorf("the user's %s = %q", ex, got.Text)
	}
}

// Rename refuses a taken name; trash moves the file aside and lists it;
// restore puts it back, under another name when the old one is taken.
// Each is told to the windows.
func TestScriptRenameTrashRestore(t *testing.T) {
	e, dir := scriptEnv(t)
	_, s := e.open()
	e.putScript("a.go", okScript, "", 200)
	e.putScript("b.go", okScript, "", 200)
	awaitScripts(t, s)
	awaitScripts(t, s)

	e.api("POST", "/api/v1/scripts/a.go/rename", `{"to":"b.go"}`, 409)
	e.api("POST", "/api/v1/scripts/nope.go/rename", `{"to":"c.go"}`, 404)
	e.api("POST", "/api/v1/scripts/a.go/rename?win=w2", `{"to":" c.go "}`, 200)
	if ev := awaitScripts(t, s); ev.Op != "renamed" || ev.Name != "a.go" || ev.To != "c.go" || ev.Win != "w2" {
		t.Errorf("rename event = %+v", ev)
	}

	e.api("DELETE", "/api/v1/scripts/nope.go", "", 404)
	id := decodeData[map[string]string](t, e.api("DELETE", "/api/v1/scripts/c.go", "", 200))["id"]
	if ev := awaitScripts(t, s); ev.Op != "trashed" || ev.Name != "c.go" || ev.ID != id {
		t.Errorf("trash event = %+v", ev)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.go")); !os.IsNotExist(err) {
		t.Fatal("trashed script still in place")
	}
	list := decodeData[scriptsList](t, e.api("GET", "/api/v1/scripts", "", 200))
	if len(list.Scripts) != 1 || len(list.Trash) != 1 || list.Trash[0].ID != id || list.Trash[0].Name != "c.go" {
		t.Fatalf("after trash: scripts %+v, trash %+v", list.Scripts, list.Trash)
	}

	// someone made a new c.go meanwhile: restore asks for another name
	e.putScript("c.go", "// new\n", "", 200)
	e.api("POST", "/api/v1/script-trash/"+id+"/restore", `{}`, 409)
	e.api("POST", "/api/v1/script-trash/"+id+"/restore", `{"to":"../c.go"}`, 400)
	e.api("POST", "/api/v1/script-trash/nope.1.go/restore", `{}`, 404)
	e.api("POST", "/api/v1/script-trash/..%2Fb.1.go/restore", `{}`, 404)
	got := decodeData[map[string]string](t, e.api("POST", "/api/v1/script-trash/"+id+"/restore", `{"to":"c_old.go"}`, 200))
	if got["name"] != "c_old.go" {
		t.Errorf("restored as %q", got["name"])
	}
	awaitScripts(t, s) // c.go's save
	if ev := awaitScripts(t, s); ev.Op != "restored" || ev.Name != "c_old.go" || ev.ID != id {
		t.Errorf("restore event = %+v", ev)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "c_old.go")); string(b) != okScript {
		t.Errorf("restored text = %q", b)
	}
}

// Check takes the editor's text, saved or not, and never runs it or writes
// anything: diags with positions for a broken script, [] for a clean one.
func TestScriptCheckRoute(t *testing.T) {
	e, dir := scriptEnv(t)
	check := func(text string) []script.Diag {
		b, _ := json.Marshal(scriptCheck{Name: "x.go", Text: text})
		return decodeData[struct {
			Diags []script.Diag `json:"diags"`
		}](t, e.api("POST", "/api/v1/script-check", string(b), 200)).Diags
	}
	if d := check(okScript); d == nil || len(d) != 0 {
		t.Errorf("clean script diags = %#v, want []", d)
	}
	bad := strings.Replace(okScript, `s.Print("hi")`, `undefinedThing()`, 1)
	d := check(bad)
	if !script.HasError(d) || d[0].Line != 9 {
		t.Errorf("undefined name diags = %+v", d)
	}
	if d := check("package main\nfunc Run("); !script.HasError(d) {
		t.Errorf("syntax error diags = %+v", d)
	}
	// init() would write a file if it ran
	marker := filepath.Join(t.TempDir(), "ran")
	initer := "package main\n\nimport (\n\t\"os\"\n\n\t\"github.com/rohanthewiz/dbc/sdb\"\n)\n\n" +
		"func init() { _ = os.WriteFile(" + strconvQuote(marker) + ", nil, 0o600) }\n\nfunc Run(s *sdb.S) error { return nil }\n"
	if d := check(initer); script.HasError(d) {
		t.Fatalf("init script diags = %+v", d)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("check ran the script's init()")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("check created the scripts dir")
	}
	b, _ := json.Marshal(scriptCheck{Text: strings.Repeat("x", 1<<20+1)})
	e.api("POST", "/api/v1/script-check", string(b), 400)
}

// script-symbol answers in UTF-16 units both ways: with a non-BMP rune
// (two units, four bytes) in a string before the name, the caret Monaco
// sends and the ranges it gets back are units, not bytes.
func TestScriptSymbol(t *testing.T) {
	e, _ := scriptEnv(t)
	text := "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\n" +
		"func Run(s *sdb.S) error {\n\tmsg := \"🐈 \"\n\ts.Print(msg + msg)\n\treturn nil\n}\n"
	units := func(s string) int { return len(utf16.Encode([]rune(s))) }
	decl := units(text[:strings.Index(text, "msg :=")])
	last := units(text[:strings.LastIndex(text, "msg)")])
	b, _ := json.Marshal(scriptSymbolReq{Text: text, Caret: last + 1})
	sym := decodeData[script.Symbol](t, e.api("POST", "/api/v1/script-symbol", string(b), 200))
	if sym.Kind != "var" || sym.Name != "msg" || sym.Def == nil || *sym.Def != (script.Span{From: decl, To: decl + 3}) {
		t.Fatalf("symbol = %+v, want var msg declared at %d", sym, decl)
	}
	if len(sym.Uses) != 3 || sym.Uses[2] != (script.Span{From: last, To: last + 3}) || sym.At != sym.Uses[2] {
		t.Errorf("uses = %+v, at = %+v; want 3, the last at %d", sym.Uses, sym.At, last)
	}
	// nothing under the caret is an answer, not an error
	b, _ = json.Marshal(scriptSymbolReq{Text: text, Caret: 1})
	if sym := decodeData[script.Symbol](t, e.api("POST", "/api/v1/script-symbol", string(b), 200)); sym.Kind != "" || sym.Uses == nil {
		t.Errorf("caret on a keyword = %+v", sym)
	}
}

// script-symbol-rename answers edits in UTF-16 units, as script-symbol does,
// and a refusal is a 400 carrying script.Rename's sentence for the rename box.
func TestScriptSymbolRename(t *testing.T) {
	e, _ := scriptEnv(t)
	text := "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\n" +
		"func Run(s *sdb.S) error {\n\tmsg := \"🐈 \"\n\ts.Print(msg + msg)\n\treturn nil\n}\n"
	units := func(s string) int { return len(utf16.Encode([]rune(s))) }
	last := units(text[:strings.LastIndex(text, "msg)")])
	b, _ := json.Marshal(scriptSymbolReq{Text: text, Caret: last, Name: "note"})
	got := decodeData[struct{ Edits []script.Edit }](t, e.api("POST", "/api/v1/script-symbol-rename", string(b), 200))
	if len(got.Edits) != 3 || got.Edits[2] != (script.Edit{From: last, To: last + 3, Text: "note"}) {
		t.Fatalf("edits = %+v, want 3, the last at %d", got.Edits, last)
	}

	b, _ = json.Marshal(scriptSymbolReq{Text: text, Caret: last, Name: "s"})
	if env := e.api("POST", "/api/v1/script-symbol-rename", string(b), 400); !strings.Contains(env.Error, "s is already declared") {
		t.Errorf("rename to s: error = %q", env.Error)
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// The list carries the built-in examples and the templates; an example's
// text and a filled template come by name, and a filled template is a
// clean script that names the tab's connection.
func TestScriptExamplesAndTemplates(t *testing.T) {
	e, _ := scriptEnv(t)
	list := decodeData[scriptsList](t, e.api("GET", "/api/v1/scripts", "", 200))
	if len(list.Scripts) != 0 || list.Scripts == nil || list.Trash == nil {
		t.Errorf("an empty dir lists %+v / %+v, want [] and []", list.Scripts, list.Trash)
	}
	if len(list.Examples) != len(scripts.Examples()) || list.Examples[0].Desc == "" {
		t.Errorf("examples = %+v", list.Examples)
	}
	if len(list.Templates) == 0 || list.Templates[0].Name != "blank" || list.Templates[0].Title == "" {
		t.Errorf("templates = %+v", list.Templates)
	}

	ex := list.Examples[0]
	got := decodeData[map[string]string](t, e.api("GET", "/api/v1/script-examples/"+ex.Name, "", 200))
	if want, _ := scripts.ExampleByName(ex.Name); got["text"] != want.Text || got["text"] == "" {
		t.Errorf("example %s text differs", ex.Name)
	}
	e.api("GET", "/api/v1/script-examples/nope.go", "", 404)
	e.api("GET", "/api/v1/script-examples/..%2Fembed.go", "", 404)

	e.api("GET", "/api/v1/script-templates/nope", "", 404)
	for _, tm := range list.Templates {
		text := decodeData[map[string]string](t, e.api("GET", "/api/v1/script-templates/"+tm.Name+"?conn=demo-sqlite", "", 200))["text"]
		if strings.Contains(text, "{{conn") {
			t.Errorf("template %s left a placeholder", tm.Name)
		}
		if d := script.Check(tm.Name+".go", text); len(d) != 0 {
			t.Errorf("template %s: %v", tm.Name, d)
		}
	}
	cp := decodeData[map[string]string](t, e.api("GET", "/api/v1/script-templates/copy?conn=demo-sqlite", "", 200))["text"]
	if !strings.Contains(cp, `"demo-sqlite"`) {
		t.Errorf("the copy template does not name the connection:\n%s", cp)
	}
}

// templateConns puts the tab's connection first, then the default, then
// the rest, each once; an unknown first is ignored.
func TestTemplateConns(t *testing.T) {
	e := newTestEnv(t, func(c *config.Config, _ *Options) {
		c.Connections = append(c.Connections,
			config.Connection{Name: "b", Driver: "sqlite", DSN: "file:b?mode=memory"},
			config.Connection{Name: "c", Driver: "sqlite", DSN: "file:c?mode=memory"})
	})
	for first, want := range map[string][]string{
		"":     {"demo-sqlite", "b", "c"},
		"c":    {"c", "demo-sqlite", "b"},
		"nope": {"demo-sqlite", "b", "c"},
	} {
		if got := e.srv.templateConns(first); !slices.Equal(got, want) {
			t.Errorf("templateConns(%q) = %v, want %v", first, got, want)
		}
	}
}

// The sdb API is served as the embedded description, as data.
func TestScriptAPIRoute(t *testing.T) {
	e, _ := scriptEnv(t)
	api := decodeData[sdbapi.API](t, e.api("GET", "/api/v1/script-api", "", 200))
	if api.Package != "sdb" || !slices.ContainsFunc(api.Types, func(ty sdbapi.Type) bool { return ty.Name == "S" && len(ty.Methods) > 0 }) {
		t.Errorf("api = %+v", api.Package)
	}
}

// showsScript shows three results, so the results bar's switcher has
// something to switch between.
const showsScript = `//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	for i := 1; i <= 3; i++ {
		r, err := s.Query("demo-sqlite", "SELECT ? AS i", i)
		if err != nil {
			return err
		}
		s.Show(r)
	}
	return nil
}
`

// A script tab's workspace never connects, and still runs a script (the
// script opens the connections it names). Each s.Show's "result" event
// counts the results so far; the outcome and the state carry them; and
// show-result puts an earlier one back on the grid, with its own status.
// An index out of range is a 400; a query run's own result ends the list.
func TestScriptResultSets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shows.go"), []byte(showsScript), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newTestEnv(t, func(c *config.Config, _ *Options) { c.ScriptsDir = dir })
	id, s := e.open() // no connect: a script tab's workspace
	e.api("POST", "/api/v1/ws/"+id+"/script", `{"name":"shows.go"}`, 200)

	var counts []string
	for range 3 {
		ev, _ := s.await(t, "result")
		r := decodeData[resultEvent](t, testEnvelope{Data: ev.Data})
		if r.Sets == nil {
			counts = append(counts, "-")
		} else {
			counts = append(counts, fmt.Sprintf("%d@%d", r.Sets.N, r.Sets.At))
		}
	}
	if strings.Join(counts, ",") != "-,2@1,3@2" {
		t.Errorf("result events' sets = %v, want none for the first, then 2@1, 3@2", counts)
	}
	ev, _ := s.await(t, "run")
	run := decodeData[runEvent](t, testEnvelope{Data: ev.Data})
	if !run.OK || run.Sets == nil || run.Sets.N != 3 || run.Sets.At != 2 {
		t.Fatalf("run = %+v (sets %+v)", run, run.Sets)
	}
	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if st.Sets == nil || st.Sets.N != 3 || st.Connected {
		t.Fatalf("state = %+v", st)
	}

	type picked struct {
		Sets   *resultSets `json:"sets"`
		Status string      `json:"status"`
	}
	p := decodeData[picked](t, e.api("POST", "/api/v1/ws/"+id+"/show-result", `{"i":0}`, 200))
	if p.Sets == nil || p.Sets.At != 0 || !strings.Contains(p.Status, "1 row") {
		t.Errorf("show-result = %+v", p)
	}
	pg := decodeData[resultPage](t, e.api("GET", "/api/v1/ws/"+id+"/result", "", 200))
	if len(pg.Cells) != 1 || *pg.Cells[0][0] != "1" {
		t.Errorf("the grid after picking result 1: %+v", pg.Cells)
	}
	e.api("POST", "/api/v1/ws/"+id+"/show-result", `{"i":3}`, 400)

	// a run with a result of its own: the script's list is over
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"demo-sqlite"}`, 200)
	s.await(t, "conn")
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 1", 0, false), 200)
	ev, _ = s.await(t, "run")
	if run = decodeData[runEvent](t, testEnvelope{Data: ev.Data}); run.Sets != nil {
		t.Errorf("a query's run still carries the script's sets: %+v", run.Sets)
	}
	if st = decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200)); st.Sets != nil {
		t.Errorf("state after a query still has sets: %+v", st.Sets)
	}
}

// A saved tab may name a script; one that is not a script name is refused,
// so a saved tab can never point the page outside scripts_dir. A script
// tab boots without a console database.
func TestSavedScriptTab(t *testing.T) {
	e := newTestEnv(t)
	win, _ := e.openWin()
	e.api("PUT", "/api/v1/tabs/k1?win="+win, `{"title":"copy.go","conn":"demo-sqlite","script":"copy.go"}`, 200)
	e.api("PUT", "/api/v1/tabs/k2?win="+win, `{"title":"x","script":"../x.go"}`, 400)
	tabs := decodeData[[]savedTab](t, e.api("GET", "/api/v1/tabs", "", 200))
	if len(tabs) != 1 || tabs[0].Script != "copy.go" || tabs[0].ConsoleDB != nil {
		t.Fatalf("saved tabs = %+v", tabs)
	}
}
