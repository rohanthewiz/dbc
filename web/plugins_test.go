package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/scripts"
)

// pluginEnv is a test server whose plugins_dir is a fresh directory. The
// registry is the process's, so the user plugins are unloaded once the
// server has shut down (cleanups run last-in first-out).
func pluginEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plugins")
	t.Cleanup(func() { script.LoadPlugins("") })
	return newTestEnv(t, func(c *config.Config, _ *Options) { c.PluginsDir = dir }), dir
}

func (e *testEnv) putPlugin(name, text, base string, want int) pluginSaved {
	e.t.Helper()
	b, _ := json.Marshal(scriptSave{Text: text, Base: base})
	env := e.api("PUT", "/api/v1/plugin-files/"+name+"?win=w1", string(b), want)
	if want != 200 {
		return pluginSaved{}
	}
	return decodeData[pluginSaved](e.t, env)
}

// A copy saved with fit (N-191): the example's Name, mask.email, is the
// first copy's, so the second is renamed after its file and loads beside
// it; a fit on a free Name, and a plain save of a taken one, write the
// text as it is.
func TestPluginSaveFit(t *testing.T) {
	e, dir := pluginEnv(t)
	ex, _ := scripts.PluginExampleByName("mask_email.go")
	put := func(name, text string, fit bool) pluginSaved {
		t.Helper()
		b, _ := json.Marshal(struct {
			scriptSave
			Fit bool `json:"fit"`
		}{scriptSave{Text: text}, fit})
		return decodeData[pluginSaved](t, e.api("PUT", "/api/v1/plugin-files/"+name+"?win=w1", string(b), 200))
	}
	if r := put("mask_email.go", ex.Text, true); r.Renamed != nil || r.Load.Plugin != "mask.email" {
		t.Fatalf("the first copy = %+v", r)
	}
	r := put("mask_email-2.go", ex.Text, true)
	if r.Renamed == nil || *r.Renamed != (pluginRenamed{From: "mask.email", To: "mask.email-2"}) ||
		r.Load.Plugin != "mask.email-2" || r.Load.Error != "" {
		t.Fatalf("the second copy = %+v %+v", r, r.Renamed)
	}
	if bs, _ := os.ReadFile(filepath.Join(dir, "mask_email-2.go")); !strings.Contains(string(bs), `"mask.email-2"`) {
		t.Errorf("the second copy on disk:\n%s", bs)
	}
	// without fit the text is the caller's: the third does not load
	if r := put("mask_three.go", ex.Text, false); r.Renamed != nil || !strings.Contains(r.Load.Error, "already the plugin of") {
		t.Errorf("a plain save = %+v", r)
	}
}

// The plugin files' life through the API: listed with the examples, made
// from one, loaded at once (the save says as what, the windows hear
// "scripts" under the tab's name and "plugins"), placed in the registry
// the palette reads; a broken file says why it did not load, there and in
// the list; renamed, trashed (gone from the palette) and restored.
func TestPluginFilesLifecycle(t *testing.T) {
	e, dir := pluginEnv(t)
	_, s := e.open()

	got := decodeData[pluginFilesList](t, e.api("GET", "/api/v1/plugin-files", "", 200))
	if got.Dir != dir || len(got.Plugins) != 0 || len(got.Examples) != 4 {
		t.Fatalf("empty list = %+v", got)
	}
	ex, _ := scripts.PluginExampleByName("mask_email.go")
	r := e.putPlugin("mask.go", ex.Text, "", 200)
	if r.Rev == "" || r.Load.Plugin != "mask.email" || r.Load.Kind != pipeline.KindTransform || r.Load.Error != "" {
		t.Fatalf("save = %+v", r)
	}
	if ev := awaitScripts(t, s); ev.Op != "saved" || ev.Name != "plugin:mask.go" {
		t.Errorf("scripts event = %+v", ev)
	}
	s.await(t, "plugins")
	reg := decodeData[struct {
		Plugins  []pipeline.Plugin        `json:"plugins"`
		Problems []pipeline.PluginProblem `json:"problems"`
	}](t, e.api("GET", "/api/v1/plugins", "", 200))
	mine := 0
	for _, p := range reg.Plugins {
		if p.File != "" {
			mine++
			if p.Name != "mask.email" || p.File != filepath.Join(dir, "mask.go") {
				t.Errorf("user plugin = %+v", p)
			}
		}
	}
	if mine != 1 || len(reg.Problems) != 0 {
		t.Errorf("registry: %d of yours, problems %+v", mine, reg.Problems)
	}

	// a file that compiles but lacks its kind's func
	bad := "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nvar Plugin = sdb.Plugin{Name: \"bad.one\", Kind: sdb.KindSink}\n"
	if r := e.putPlugin("bad.go", bad, "", 200); !strings.Contains(r.Load.Error, "no func Write") || r.Load.Plugin != "bad.one" {
		t.Errorf("broken save = %+v", r)
	}
	awaitScripts(t, s)
	got = decodeData[pluginFilesList](t, e.api("GET", "/api/v1/plugin-files", "", 200))
	if len(got.Plugins) != 2 || got.Plugins[0].Name != "bad.go" || !strings.Contains(got.Plugins[0].Error, "no func Write") ||
		got.Plugins[1].Plugin != "mask.email" || got.Plugins[1].Kind != pipeline.KindTransform {
		t.Errorf("list = %+v", got.Plugins)
	}
	diags := decodeData[map[string][]script.Diag](t, e.api("POST", "/api/v1/plugin-check",
		`{"name": "plugin:bad.go", "text": `+jsonString(bad)+`}`, 200))["diags"]
	if len(diags) != 1 || diags[0].Line != 5 || !strings.Contains(diags[0].Msg, "no func Write") {
		t.Errorf("check = %+v", diags)
	}

	// rename: events under the tab names; the loader follows the file
	e.api("POST", "/api/v1/plugin-files/mask.go/rename?win=w1", `{"to": "plugin:masking.go"}`, 200)
	if ev := awaitScripts(t, s); ev.Op != "renamed" || ev.Name != "plugin:mask.go" || ev.To != "plugin:masking.go" {
		t.Errorf("rename event = %+v", ev)
	}
	if p, _ := pipeline.Lookup("mask.email"); p.File != filepath.Join(dir, "masking.go") {
		t.Errorf("after the rename the plugin is from %s", p.File)
	}
	// trash: out of the palette; restore: back
	id := decodeData[map[string]string](t, e.api("DELETE", "/api/v1/plugin-files/masking.go?win=w1", "", 200))["id"]
	if _, ok := pipeline.Lookup("mask.email"); ok {
		t.Error("a trashed file's plugin is still registered")
	}
	got = decodeData[pluginFilesList](t, e.api("GET", "/api/v1/plugin-files", "", 200))
	if len(got.Trash) != 1 || got.Trash[0].ID != id {
		t.Fatalf("trash = %+v", got.Trash)
	}
	if name := decodeData[map[string]string](t, e.api("POST", "/api/v1/plugin-trash/"+id+"/restore?win=w1", `{}`, 200))["name"]; name != "plugin:masking.go" {
		t.Errorf("restored as %q", name)
	}
	if _, ok := pipeline.Lookup("mask.email"); !ok {
		t.Error("a restored file's plugin is not registered")
	}

	// the names the store refuses, the example route, a 404
	e.api("GET", "/api/v1/plugin-files/..%2Fx.go", "", 400)
	e.api("GET", "/api/v1/plugin-files/nope.go", "", 404)
	if text := decodeData[map[string]string](t, e.api("GET", "/api/v1/plugin-examples/wait_file", "", 200))["text"]; !strings.Contains(text, `"wait.file"`) {
		t.Errorf("example text = %.80q", text)
	}
}

// A plugin file's tab is a script tab named "plugin:<file>": saved like
// any tab, and a name outside the store's alphabet refused as a script's.
func TestPluginTabNames(t *testing.T) {
	e, _ := pluginEnv(t)
	win, _ := e.open()
	e.api("PUT", "/api/v1/tabs/k1?win="+win, `{"title":"mask.go","script":"plugin:mask.go"}`, 200)
	e.api("PUT", "/api/v1/tabs/k2?win="+win, `{"title":"x","script":"plugin:../x.go"}`, 400)
	e.api("PUT", "/api/v1/tabs/k3?win="+win, `{"title":"x","script":"plug:x.go"}`, 400)
}

// A file written behind the server's back (an editor, `cp`) is noticed
// by the watcher, loaded, and told to the windows.
func TestPluginWatch(t *testing.T) {
	was := pluginWatchEvery
	pluginWatchEvery = 20 * time.Millisecond
	t.Cleanup(func() { pluginWatchEvery = was })
	e, dir := pluginEnv(t)
	_, s := e.open()
	ex, _ := scripts.PluginExampleByName("gen_series.go")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "series.go"), []byte(ex.Text), 0o644); err != nil {
		t.Fatal(err)
	}
	s.await(t, "plugins")
	if p, ok := pipeline.Lookup("gen.series"); !ok || p.Kind != pipeline.KindSource {
		t.Errorf("gen.series = %+v %v", p, ok)
	}
}
