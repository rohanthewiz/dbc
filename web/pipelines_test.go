package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
)

// pipelineEnv is a test server whose pipelines_dir is a fresh, not yet
// created directory, as for a new install. The test connection is called
// demo-sqlite, as the examples' is, so they run as they ship.
func pipelineEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pipelines")
	return newTestEnv(t, func(c *config.Config, _ *Options) { c.PipelinesDir = dir }), dir
}

func (e *testEnv) putPipeline(name, text, base string, want int) scriptSaved {
	e.t.Helper()
	b, _ := json.Marshal(scriptSave{Text: text, Base: base})
	env := e.api("PUT", "/api/v1/pipelines/"+name+"?win=w1", string(b), want)
	if want != 200 {
		return scriptSaved{}
	}
	return decodeData[scriptSaved](e.t, env)
}

func exampleText(t *testing.T, name string) string {
	t.Helper()
	ex, ok := scripts.PipelineByName(name)
	if !ok {
		t.Fatalf("no example %s", name)
	}
	return ex.Text
}

// awaitJob reads the stream up to the next window event of type typ (a
// "job.*") and decodes its data.
func awaitJob[T any](t *testing.T, s *stream, typ string) T {
	t.Helper()
	ev, _ := s.await(t, typ)
	var v T
	if err := json.Unmarshal(ev.Data, &v); err != nil {
		t.Fatalf("%s: %v: %s", typ, err, ev.Data)
	}
	return v
}

// awaitPipelines reads the stream up to the next "pipelines" event.
func awaitPipelines(t *testing.T, s *stream) scriptsEvent {
	t.Helper()
	return awaitJob[scriptsEvent](t, s, "pipelines")
}

type runEnvelope struct {
	Run jobs.Run `json:"run"`
}

// The store is the scripts protocol for .json files: create never
// overwrites, a stale base is a conflict carrying the file's text, rename,
// trash and restore, each told to the windows as "pipelines". What is not
// a JSON object is not saved.
func TestPipelineStore(t *testing.T) {
	e, dir := pipelineEnv(t)
	_, s := e.open()
	text := exampleText(t, "cats-report.json")

	e.api("GET", "/api/v1/pipelines/report.json", "", 404)
	r1 := e.putPipeline("report.json", text, "", 200)
	if ev := awaitPipelines(t, s); ev.Op != "saved" || ev.Name != "report.json" || ev.Rev != r1.Rev {
		t.Errorf("save event = %+v", ev)
	}
	e.putPipeline("report.json", text, "", 409) // a create onto a name taken
	if c := e.putPipeline("report.json", text+" ", "stale", 200); !c.Conflict || c.Text != text {
		t.Errorf("stale base = %+v", c)
	}
	e.putPipeline("x.json", "not json", "", 400)
	e.putPipeline("x.json", "[1, 2]", "", 400)
	e.api("PUT", "/api/v1/pipelines/..%2Fescape.json", `{"text":"{}"}`, 400)

	list := decodeData[pipelinesList](t, e.api("GET", "/api/v1/pipelines", "", 200))
	if len(list.Pipelines) != 1 || list.Pipelines[0].Name != "report.json" || list.Pipelines[0].Fragments != 1 ||
		len(list.Examples) < 3 || list.Dir != dir {
		t.Errorf("list = %+v", list)
	}

	e.api("POST", "/api/v1/pipelines/report.json/rename?win=w1", `{"to":"daily.json"}`, 200)
	if ev := awaitPipelines(t, s); ev.Op != "renamed" || ev.To != "daily.json" {
		t.Errorf("rename event = %+v", ev)
	}
	trashed := decodeData[map[string]string](t, e.api("DELETE", "/api/v1/pipelines/daily.json?win=w1", "", 200))
	if _, err := os.Stat(filepath.Join(dir, "daily.json")); !os.IsNotExist(err) {
		t.Error("the trashed file is still there")
	}
	awaitPipelines(t, s)
	e.api("POST", "/api/v1/pipeline-trash/"+trashed["id"]+"/restore", `{}`, 200)
	if got := decodeData[scriptText](t, e.api("GET", "/api/v1/pipelines/daily.json", "", 200)); got.Text != text {
		t.Errorf("restored text = %q", got.Text)
	}
	e.api("POST", "/api/v1/pipeline-trash/nope.1.json/restore", `{}`, 404)

	ex := decodeData[map[string]string](t, e.api("GET", "/api/v1/pipeline-examples/clean-and-load.json", "", 200))
	if !strings.Contains(ex["text"], `"go.transform"`) {
		t.Errorf("example = %v", ex)
	}
}

// The check takes unsaved text: a parse error is placed at its offset, an
// unknown key at the key, and Check's findings at the node or field they
// name — for the JSON view's markers.
func TestPipelineCheckRoute(t *testing.T) {
	e, _ := pipelineEnv(t)
	check := func(text string) []pipeDiag {
		b, _ := json.Marshal(map[string]string{"text": text})
		return decodeData[struct {
			Diags []pipeDiag `json:"diags"`
		}](t, e.api("POST", "/api/v1/pipeline-check", string(b), 200)).Diags
	}
	if d := check(exampleText(t, "clean-and-load.json")); len(d) != 0 {
		t.Errorf("the example has diags: %+v", d)
	}
	if d := check("{\n  \"name\": \"x\",\n  \"fragments\": [\n}"); len(d) != 1 || d[0].Line != 4 {
		t.Errorf("syntax error = %+v", d)
	}
	if d := check("{\n  \"name\": \"x\",\n  \"fragmnts\": []\n}"); len(d) != 1 || d[0].Line != 3 || !strings.Contains(d[0].Msg, "fragmnts") {
		t.Errorf("unknown key = %+v", d)
	}
	bad := strings.Replace(exampleText(t, "clean-and-load.json"), `"table": "cats_clean"`, `"table": ""`, 1)
	bad = strings.Replace(bad, `"conn": "demo-sqlite",`+"\n"+`            "query"`, `"conn": "nowhere",`+"\n"+`            "query"`, 1)
	d := check(bad)
	byWhere := map[string]pipeDiag{}
	for _, x := range d {
		byWhere[x.Where] = x
	}
	if x, ok := byWhere["clean/dst"]; !ok || !strings.Contains(x.Msg, "table: required") || x.Line == 0 {
		t.Errorf("required field: %+v", d)
	}
	lines := strings.Split(bad, "\n")
	if x, ok := byWhere["clean/src.conn"]; !ok || !strings.Contains(x.Msg, `no connection called "nowhere"`) ||
		!strings.Contains(lines[x.Line-1], `"conn": "nowhere"`) {
		t.Errorf("unknown connection: %+v", d)
	}
}

// A preview from a tab: the run starts at once, its rows land in that
// tab's grid (a "result" event, then the result route), every window hears
// job.run … job.preview … job.done, and nothing is written.
func TestPipelinePreviewLandsInTheTab(t *testing.T) {
	e, _ := pipelineEnv(t)
	id, s := e.open()
	b, _ := json.Marshal(pipelinePreviewReq{WS: id, Text: exampleText(t, "clean-and-load.json"), Rows: 4})
	run := decodeData[runEnvelope](t, e.api("POST", "/api/v1/pipeline-preview", string(b), 200)).Run
	if run.Preview != 4 || run.Origin != id || run.Name != "clean-and-load" {
		t.Fatalf("run = %+v", run)
	}
	started := awaitJob[runEnvelope](t, s, "job.run")
	if started.Run.ID != run.ID {
		t.Errorf("job.run = %+v", started.Run)
	}
	pv := awaitJob[jobPreview](t, s, "job.preview")
	if pv.Origin != id || pv.Rows != 4 || pv.Title != "preview clean/dst" || !slices.Contains(pv.Cols, "label") {
		t.Errorf("job.preview = %+v", pv)
	}
	ev, _ := s.await(t, "result")
	if ev.WS != id {
		t.Errorf("result went to %q", ev.WS)
	}
	done := awaitJob[runEnvelope](t, s, "job.done")
	if done.Run.Status != pipeline.Succeeded || done.Run.Pipelines[0].Fragments[1].Status != pipeline.Skipped {
		t.Errorf("job.done = %+v", done.Run)
	}
	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if !st.HasResult || st.Sets == nil || st.Sets.N != 2 {
		t.Errorf("the tab's state after the preview = %+v", st)
	}
	r, err := e.srv.mgr.Run("demo-sqlite", "SELECT count(*) FROM sqlite_master WHERE name = 'cats_clean'")
	if err != nil || r.Rows[0][0] != "0" {
		t.Errorf("a preview wrote: %v %v", r, err)
	}
	// the record keeps the log
	rec := decodeData[jobs.Run](t, e.api("GET", "/api/v1/runs/"+run.ID, "", 200))
	if len(rec.Log) == 0 {
		t.Error("no log in the record")
	}
	runs := decodeData[struct {
		Running []jobs.Run `json:"running"`
		Recent  []jobs.Run `json:"recent"`
	}](t, e.api("GET", "/api/v1/runs", "", 200))
	if len(runs.Running) != 0 || len(runs.Recent) != 1 || runs.Recent[0].ID != run.ID {
		t.Errorf("runs = %+v", runs)
	}

	e.api("POST", "/api/v1/pipeline-preview", `{"ws":"nope","text":"{}"}`, 404)
	b, _ = json.Marshal(pipelinePreviewReq{WS: id, Text: `{"name": "x"`})
	e.api("POST", "/api/v1/pipeline-preview", string(b), 400)
}

// waiting is a pipeline whose Go source waits until the run is stopped.
const waiting = `{"name": "waiting", "fragments": [{"name": "w", "nodes": [
  {"id": "src", "plugin": "go.source", "cfg": {"code": "func Next(e *sdb.Env) (*sdb.Batch, error) {\n\t<-e.Ctx.Done()\n\treturn nil, e.Ctx.Err()\n}\n"}},
  {"id": "dst", "plugin": "preview", "cfg": {}}
], "edges": [["src", "dst"]]}]}`

// A run runs the saved file (or an example by name): it writes what it
// loads. One run of a pipeline at a time is a 409; the cancel route stops
// one, which ends canceled.
func TestPipelineRunAndCancel(t *testing.T) {
	e, _ := pipelineEnv(t)
	id, s := e.open()
	e.putPipeline("clean.json", exampleText(t, "clean-and-load.json"), "", 200)
	run := decodeData[runEnvelope](t, e.api("POST", "/api/v1/pipeline-run", `{"ws":"`+id+`","name":"clean.json"}`, 200)).Run
	for {
		done := awaitJob[runEnvelope](t, s, "job.done")
		if done.Run.ID == run.ID {
			if done.Run.Status != pipeline.Succeeded {
				t.Fatalf("run = %+v", done.Run)
			}
			break
		}
	}
	if r, _ := e.srv.mgr.Run("demo-sqlite", "SELECT count(*) FROM cats_clean"); r == nil || r.Rows[0][0] == "0" {
		t.Errorf("the run wrote nothing: %v", r)
	}
	// an example runs by name
	ex := decodeData[runEnvelope](t, e.api("POST", "/api/v1/pipeline-run", `{"name":"cats-report.json"}`, 200)).Run
	if ex.Name != "cats-report" {
		t.Errorf("example run = %+v", ex)
	}
	e.api("POST", "/api/v1/pipeline-run", `{"name":"nope.json"}`, 404)

	e.putPipeline("waiting.json", waiting, "", 200)
	slow := decodeData[runEnvelope](t, e.api("POST", "/api/v1/pipeline-run", `{"name":"waiting.json"}`, 200)).Run
	e.api("POST", "/api/v1/pipeline-run", `{"name":"waiting.json"}`, 409)
	e.api("POST", "/api/v1/runs/"+slow.ID+"/cancel", "", 200)
	for {
		done := awaitJob[runEnvelope](t, s, "job.done")
		if done.Run.ID == slow.ID {
			if done.Run.Status != pipeline.Canceled {
				t.Errorf("canceled run = %+v", done.Run)
			}
			break
		}
	}
	e.api("POST", "/api/v1/runs/nope/cancel", "", 404)
	e.api("GET", "/api/v1/runs/nope", "", 404)
}

// Export is the builder form of a saved pipeline or an example; the
// plugins route lists the registry, Go plugins included, with fields.
func TestPipelineExportAndPlugins(t *testing.T) {
	e, _ := pipelineEnv(t)
	got := decodeData[map[string]string](t, e.api("GET", "/api/v1/pipeline-export/clean-and-load.json", "", 200))
	if !strings.Contains(got["text"], "sdb.NewPipeline(") || !strings.Contains(got["text"], "func Run(s *sdb.S) error") {
		t.Errorf("export = %s", got["text"])
	}
	e.api("GET", "/api/v1/pipeline-export/nope.json", "", 404)

	pl := decodeData[struct {
		Plugins []pipeline.Plugin `json:"plugins"`
		Conns   []string          `json:"conns"`
	}](t, e.api("GET", "/api/v1/plugins", "", 200))
	byName := map[string]pipeline.Plugin{}
	for _, p := range pl.Plugins {
		byName[p.Name] = p
	}
	if p, ok := byName["sql.read"]; !ok || p.Kind != pipeline.KindSource || len(p.Fields) == 0 {
		t.Errorf("sql.read = %+v", p)
	}
	if _, ok := byName["go.transform"]; !ok {
		t.Error("the Go plugins are missing")
	}
	if !slices.Equal(pl.Conns, []string{"demo-sqlite"}) {
		t.Errorf("conns = %v", pl.Conns)
	}
}

// A saved tab may name a pipeline — one the store would accept, and never
// beside a script.
func TestPipelineTabSaved(t *testing.T) {
	e, _ := pipelineEnv(t)
	e.api("PUT", "/api/v1/tabs/p1", `{"title":"x","pipeline":"../x.json"}`, 400)
	e.api("PUT", "/api/v1/tabs/p1", `{"title":"x","pipeline":"a.json","script":"a.go"}`, 400)
	e.api("PUT", "/api/v1/tabs/p1", `{"title":"a.json","pipeline":"a.json","conn":"demo-sqlite"}`, 200)
	tabs := decodeData[[]savedTab](t, e.api("GET", "/api/v1/tabs", "", 200))
	if len(tabs) != 1 || tabs[0].Pipeline != "a.json" || tabs[0].ConsoleDB != nil {
		t.Errorf("tabs = %+v", tabs)
	}
}
