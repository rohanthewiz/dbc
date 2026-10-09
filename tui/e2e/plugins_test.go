package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// e2eCountPlugin is a source plugin: the numbers 1 to its "to".
const e2eCountPlugin = `//go:build ignore

// Counts from 1 to a number.
package main

import "github.com/rohanthewiz/dbc/sdb"

var Plugin = sdb.Plugin{Name: "e2e.count", Kind: sdb.KindSource, Label: "Count",
	Fields: []sdb.Field{{Name: "to", Type: sdb.FieldInt, Required: true, Doc: "The last number."}}}

var n int64

func Next(e *sdb.Env) (*sdb.Batch, error) {
	to, err := e.Cfg.Int("to", 0)
	if err != nil || n >= int64(to) {
		return nil, err
	}
	b := sdb.NewBatch(sdb.ColsOf([]string{"n"}, []string{"INT8"}), nil)
	for n < int64(to) && len(b.Rows) < e.Batch {
		n++
		b.Rows = append(b.Rows, []any{n})
	}
	return b, nil
}
`

// userPlugins drives a plugin file through the TUI (Pipelines Phase 6):
//
//	Ctrl+O, the Plugins section ─► a plugin example copied into plugins_dir
//	                                (Enter, Enter), $EDITOR (the stand-in
//	                                writes e2e.count), checked and loaded:
//	                                the log says as what, the row says so
//	again, broken ─► the check's path:line:col, "did not load", the row ⚠
//	again, fixed ─► loaded; a pipeline placing e2e.count runs from Ctrl+J
//	dbc plugins (another process) ─► lists it, from the file
func userPlugins(t *testing.T, e *env, u *term) {
	broken := strings.Replace(e2eCountPlugin, "func Next(", "func Next2(", 1)
	pipes := filepath.Join(e.home, ".config", "dbc", "pipelines")
	if err := os.MkdirAll(pipes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pipes, "e2e_plug.json"), []byte(`{"name": "e2e_plug", "fragments": [{"name": "count",
	  "nodes": [{"id": "src", "plugin": "e2e.count", "cfg": {"to": "4"}}, {"id": "peek", "plugin": "preview"}],
	  "edges": [["src", "peek"]]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// ── copy an example; the "edit" writes e2e.count ──────────────────────
	e.nextEdit(t, e2eCountPlugin)
	u.ctrl('o')
	u.waitFor("Plugins · ~/.config/dbc/plugins", "Plugin examples · Enter makes your own copy")
	u.key('/')
	u.typeText("gen_series")
	u.key(uv.KeyEscape) // back to the list, the example under the cursor
	u.waitFor("Enter run · e edit")
	u.key(uv.KeyEnter)
	u.waitFor("Copy the example plugin gen_series.go")
	u.key(uv.KeyEnter) // the offered name
	u.waitFor("gen_series.go saved — checked, no problems", "gen_series.go loaded: ⇥ e2e.count (source)",
		"⇥ e2e.count — Counts from 1 to a number.", "Enter/e edit")

	// ── broken: the check's line, the load's verdict, ⚠ ───────────────────
	e.nextEdit(t, broken)
	u.key(uv.KeyEnter) // Enter edits a plugin: there is nothing to run
	u.waitFor("gen_series.go saved — the check found 1 error(s)", "gen_series.go:8:5: no func Next",
		"gen_series.go did not load: ", "⚠")

	// ── fixed, and placed in a pipeline ──────────────────────────────────
	e.nextEdit(t, e2eCountPlugin)
	u.key(uv.KeyEnter)
	u.waitFor("gen_series.go loaded: ⇥ e2e.count (source)")
	u.key(uv.KeyEscape)
	u.waitGone("Plugins · ~/.config/dbc/plugins")
	u.ctrl('j')
	u.waitFor("Pipelines & jobs", "e2e_plug.json")
	u.key('/')
	u.typeText("e2e_plug")
	u.key(uv.KeyEnter)
	u.waitFor("Run 2026", "pipeline e2e_plug", "✓ succeeded", "src  e2e.count", "0 → 4")
	u.key(uv.KeyEscape)
	u.waitGone("Run 2026")
	u.waitFor("✓ pipeline e2e_plug succeeded", "preview count/peek")

	// ── another process sees the file too ────────────────────────────────
	if out := e.cli(t, "plugins"); !strings.Contains(out, "e2e.count") || !strings.Contains(out, "~/.config/dbc/plugins/gen_series.go") {
		t.Fatalf("dbc plugins:\n%s", out)
	}
	if err := os.Remove(filepath.Join(e.home, "next-edit.go")); err != nil {
		t.Fatal(err)
	}
}
