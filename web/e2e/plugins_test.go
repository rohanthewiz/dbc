package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// pluginFiles is Phase 6's user plugins in the browser — a Go file in
// plugins_dir becoming a node the canvas can place:
//
//	Ctrl+O               ─► the Plugins section: none yet, four examples
//	⧉ Copy mask_email.go ─► the name, a plugin tab (◈): a script tab named
//	                        "plugin:mask_email.go", on the file in plugins_dir
//	break it             ─► the plugin check marks the descriptor's line
//	Ctrl+S               ─► saved, the log says it did not load; the pipeline
//	                        tab's palette lists it under Yours, ⚠, not draggable
//	fix it, Ctrl+S       ─► "loaded: ƒ mask.email (transform)"; the palette's
//	                        Yours has it, and a drag onto the lane places it,
//	                        its inspector drawn from the file's Fields
//	Ctrl+O               ─► the row says what it loaded as
//
// It runs after the pipeline step, whose e2e_pipe pipeline it places the
// plugin in; the drag is left unsaved, so nothing after it sees a change.
func pluginFiles(t *testing.T, e *env, p *rod.Page) {
	dir := filepath.Join(e.home, ".config", "dbc", "plugins")
	ctrl := func(k input.Key) {
		t.Helper()
		if err := p.Keyboard.Press(input.ControlLeft); err != nil {
			t.Fatal(err)
		}
		p.Keyboard.MustType(k)
		if err := p.Keyboard.Release(input.ControlLeft); err != nil {
			t.Fatal(err)
		}
	}
	logHas := func(what, re string) {
		t.Helper()
		waitFor(t, p, what, `(re) => [...document.querySelectorAll("#log > div")].some((d) => new RegExp(re).test(d.textContent))`, re)
	}
	// the tabs are reached through the browser's ✎ (which shows the tab
	// already open on a file): after the earlier steps the strip is full,
	// and a tab clicked by its title may be scrolled out of reach
	openFrom := func(kind, name, header string) {
		t.Helper()
		ctrl(input.KeyO)
		waitFor(t, p, "the browser's "+name, `(k, n) => !!document.querySelector('.slist .srow.' + k + '[data-name="' + n + '"]')`, kind, name)
		eval(t, p, `(k, n) => [...document.querySelectorAll('.slist .srow.' + k + '[data-name="' + n + '"] .sact button')]
		  .find((b) => b.textContent === "✎").click()`, kind, name)
		waitFor(t, p, "the tab on "+name, `(h) => document.getElementById("active-conn").textContent.startsWith(h)`, header)
	}
	pipeTab := func() { t.Helper(); openFrom("pipeline", "e2e_pipe.json", "⛓ e2e_pipe.json") }
	plugTab := func() { t.Helper(); openFrom("plugin", "mask_email.go", "◈ mask_email.go") }

	// ── the browser's Plugins section ─────────────────────────────────────
	clickSel(t, p, "#scripts-btn", proto.InputMouseButtonLeft)
	waitFor(t, p, "the plugin examples and the empty note", `() =>
	  document.querySelectorAll(".slist .srow.plexample").length === 4 &&
	  [...document.querySelectorAll(".slist .snote")].some((n) => /New ▾ → Plugin/.test(n.textContent))`)

	// ── ⧉ Copy an example: a plugin tab on the new file ───────────────────
	eval(t, p, `() => {
	  const row = [...document.querySelectorAll(".slist .srow.plexample")].find((r) => r.dataset.name === "mask_email.go");
	  row.querySelector(".sact button").click();
	}`)
	waitFor(t, p, "the name prompt", `() => { const i = document.querySelector(".modal input.hfilter"); return !!i && i.value === "mask_email.go"; }`)
	eval(t, p, `() => document.querySelector(".modal button.primary").click()`)
	waitFor(t, p, "the plugin tab", `() => {
	  const tab = document.querySelector("#qtabs .qtab.script.on");
	  return !!tab && tab.querySelector(".qt").textContent === "mask_email.go" && tab.querySelector(".qgo").textContent === "◈" &&
	    document.getElementById("active-conn").textContent === "◈ mask_email.go" &&
	    monaco.editor.getEditors()[0].getModel().getLanguageId() === "go";
	}`)
	src, err := os.ReadFile(filepath.Join(dir, "mask_email.go"))
	if err != nil || !strings.Contains(string(src), `Name:  "mask.email"`) {
		t.Fatalf("the copy in plugins_dir: %v\n%s", err, src)
	}

	// ── broken: marked as typed, said on save, ⚠ in the palette ──────────
	broken := strings.Replace(string(src), "func Apply(", "func Apply2(", 1)
	eval(t, p, `(s) => { dbc.editor.setText(s); monaco.editor.getEditors()[0].focus(); }`, broken)
	waitFor(t, p, "the plugin check's marker on var Plugin", `() => {
	  const m = monaco.editor.getModelMarkers({ owner: "dbc" });
	  return m.length === 1 && /no func Apply/.test(m[0].message) && m[0].severity === monaco.MarkerSeverity.Error;
	}`)
	ctrl(input.KeyS)
	logHas("the failed load in the log", `mask_email\.go did not load as mask\.email`)
	pipeTab()
	waitFor(t, p, "the broken file in Yours", `() => {
	  const heads = [...document.querySelectorAll(".plist .phead")].map((h) => h.textContent);
	  const b = document.querySelector(".plist .pitem.broken");
	  return heads[0] === "Yours" && !!b && /mask\.email/.test(b.textContent) && !b.dataset.plugin &&
	    !document.querySelector('.pitem[data-plugin="mask.email"]');
	}`)

	// ── fixed: loaded, in the palette, placed on the lane ─────────────────
	plugTab()
	eval(t, p, `(s) => { dbc.editor.setText(s); monaco.editor.getEditors()[0].focus(); }`, string(src))
	waitFor(t, p, "the markers cleared", `() => monaco.editor.getModelMarkers({ owner: "dbc" }).length === 0`)
	ctrl(input.KeyS)
	logHas("the load in the log", `mask_email\.go loaded: ƒ mask\.email \(transform\)`)
	pipeTab()
	waitFor(t, p, "mask.email in Yours", `() => !!document.querySelector('.plist .pitem[data-plugin="mask.email"]') &&
	  !document.querySelector(".plist .pitem.broken")`)
	// onto the first lane, whatever the pipeline step left it called
	dragTo(t, p, `.pitem[data-plugin="mask.email"]`, `.plane .lbody`, 0, 150)
	waitFor(t, p, "the card, and its form from the file's Fields", `() => {
	  const card = [...document.querySelectorAll(".plane .pcard")].find((c) => c.querySelector(".cp").textContent === "mask.email");
	  const names = [...document.querySelectorAll(".pinsp .ifield .iname")].map((n) => n.textContent);
	  return !!card && names.some((n) => n.startsWith("column")) && names.some((n) => n.startsWith("salt"));
	}`)

	// ── the browser's row says what the file loaded as ────────────────────
	ctrl(input.KeyO)
	waitFor(t, p, "the plugin row", `() => {
	  const r = [...document.querySelectorAll(".slist .srow.plugin")].find((x) => x.dataset.name === "mask_email.go");
	  return !!r && /ƒ mask\.email/.test(r.querySelector(".sdesc").textContent) && !r.classList.contains("bad");
	}`)
	chord(t, p, 0, "Escape", "Escape", 27)
	waitFor(t, p, "the browser closed", `() => !document.querySelector(".modal.scripts")`)
}
