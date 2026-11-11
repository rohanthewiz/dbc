package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
)

// saveTrouble: a save dbc web does not take is said, marked and retried,
// and the console's text is kept in this browser until one lands (N-265).
// Before, app.js swallowed the failure: dbc web stopped or restarting
// while the user typed lost everything since the last good save on the
// next reload. And the page's goodbye was one keepalive request carrying
// the console's text twice, refused outright past the Fetch spec's 64 KiB
// — so a big console's last edits were lost, and the release with them:
// its tabs stayed claimed, and a browser tab opened in the grace was told
// they were open in another one.
//
// It runs in browser windows of its own, so the shared page and its tabs
// are left as they were. A failing dbc web is stood in for by a shim over
// the page's fetch that refuses the tab and console PUTs as a dead server
// would (a rejected promise — what fetch gives when nothing answers); the
// shim is the page's, so a reload drops it, as a restarted dbc web would
// answer again.
func saveTrouble(t *testing.T, e *env, _ *rod.Page) {
	q := e.page(t, "lite")
	defer q.MustClose()
	noJSErrors := func(p *rod.Page) {
		t.Helper()
		if errs := jsErrors(t, p); len(errs) > 0 {
			t.Fatalf("the page logged JavaScript errors:\n  %s", strings.Join(errs, "\n  "))
		}
	}

	// a console of its own: the shared page's tabs show lite's others.
	// Waited for by name: the fresh tab's first console may be empty too,
	// and typing before the switch lands would go into that one
	waitFor(t, q, "the fresh tab's console", `() => !!dbc.state.tab.console`)
	before := evalStr(t, q, `() => dbc.state.tab.console`)
	chord(t, q, modAlt, "n", "KeyN", 78)
	waitFor(t, q, "a new console", `(b) => !!dbc.state.tab.console && dbc.state.tab.console !== b &&
	  dbc.editor.docKey().endsWith("/" + dbc.state.tab.console) && dbc.editor.text() === ""`, before)
	key := evalStr(t, q, `() => dbc.state.tab.key`)
	waitFor(t, q, "the tab saved", `async (k) => {
	  const r = await (await fetch("/api/v1/tabs")).json();
	  return (r.data || []).some((t) => t.id === k && !!t.console);
	}`, key)

	// fileIs waits until the console's text as dbc web holds it is want;
	// draftIs until the draft this browser keeps of it is want ("" for
	// none). want is a JS expression, so a big text is built in the page
	// rather than sent over.
	fileIs := func(what, want string) {
		t.Helper()
		waitFor(t, q, what, `async () => {
		  const c = dbc.state.tab.cdb, name = dbc.state.tab.console;
		  const r = await (await fetch("/api/v1/consoles/" + encodeURIComponent(c.host) + "/" +
		    encodeURIComponent(c.database) + "/" + encodeURIComponent(name))).json();
		  return (r.data || {}).text === `+want+`;
		}`)
	}
	draftIs := func(what, want string) {
		t.Helper()
		waitFor(t, q, what, `() => {
		  const c = dbc.state.tab.cdb, tail = ":" + c.host + "/" + c.database + "/" + dbc.state.tab.console;
		  const k = Object.keys(localStorage).find((k) => k.startsWith("dbc.console.draft.") && k.endsWith(tail));
		  return (k ? JSON.parse(localStorage.getItem(k)).text : "") === `+want+`;
		}`)
	}
	failSaves := func(on bool) {
		t.Helper()
		eval(t, q, `(on) => {
		  if (!window.__realFetch) {
		    window.__realFetch = window.fetch;
		    window.fetch = function (input, init) {
		      const url = typeof input === "string" ? input : input.url;
		      if (window.__failSaves && init && init.method === "PUT" && /\/api\/v1\/(consoles|tabs)\//.test(url)) {
		        return Promise.reject(new TypeError("Failed to fetch"));
		      }
		      return window.__realFetch.apply(this, arguments);
		    };
		  }
		  window.__failSaves = on;
		}`, on)
	}
	marked := func(on bool) {
		t.Helper()
		waitFor(t, q, "the tab's ⚠ "+map[bool]string{true: "on", false: "gone"}[on],
			`(on) => !!document.querySelector("#qtabs .qtab.on .qunsaved") === on`, on)
	}
	logHas := func(what, s string) {
		t.Helper()
		waitFor(t, q, what, `(s) => document.getElementById("log").textContent.includes(s)`, s)
	}

	// 1. dbc web stops answering: the edit is kept in this browser, the
	// tab marked, the log told once
	failSaves(true)
	eval(t, q, `() => dbc.editor.focus()`)
	q.MustInsertText("SELECT 'kept'")
	marked(true)
	logHas("the failure logged", "not saved: dbc web did not answer")
	draftIs("the draft kept", `"SELECT 'kept'"`)

	// 2. it answers again: the retry (2 s) saves, the mark goes, the
	// draft is dropped
	failSaves(false)
	marked(false)
	logHas("the recovery logged", "saved again")
	fileIs("the file saved", `"SELECT 'kept'"`)
	draftIs("the draft dropped", `""`)

	// 3. down again, and the page reloads before it is back (the reload a
	// restarted dbc web forces): the goodbye's console PUT is refused too,
	// so only the draft has the edit — and the reload puts it back
	failSaves(true)
	eval(t, q, `() => dbc.editor.focus()`)
	q.KeyActions().Press(input.End).MustDo()
	q.MustInsertText(" -- more")
	marked(true)
	q.MustReload()
	q.MustWaitLoad()
	waitConnected(t, q, "lite")
	waitFor(t, q, "the tab back", `(k) => dbc.state.tab.key === k`, key)
	waitFor(t, q, "the draft's text back", `() => dbc.editor.text() === "SELECT 'kept' -- more"`)
	logHas("the put-back logged", "they are back, and saved now")
	fileIs("the draft saved", `"SELECT 'kept' -- more"`)
	draftIs("the draft dropped", `""`)
	marked(false)
	noJSErrors(q)

	// 4. down again, and meanwhile the console is changed elsewhere (the
	// TUI writes its file): the draft is older than the file, so neither
	// silently wins — the reload shows the file's text, and Ctrl+Z brings
	// the draft back, which then saves over it (a save's conflict, as the
	// consoles have always handled one)
	failSaves(true)
	eval(t, q, `() => dbc.editor.focus()`)
	q.KeyActions().Press(input.End).MustDo()
	q.MustInsertText(" -- mine")
	marked(true)
	name := evalStr(t, q, `() => dbc.state.tab.console`)
	files, _ := filepath.Glob(filepath.Join(e.home, ".config", "dbc", "consoles", "local", "lite.db-*", name+".sql"))
	if len(files) != 1 {
		t.Fatalf("lite's %s: files %v, want one", name, files)
	}
	if err := os.WriteFile(files[0], []byte("SELECT 'from the tui'"), 0o600); err != nil {
		t.Fatal(err)
	}
	q.MustReload()
	q.MustWaitLoad()
	waitConnected(t, q, "lite")
	waitFor(t, q, "the tab back", `(k) => dbc.state.tab.key === k`, key)
	waitFor(t, q, "the file's text shown", `() => dbc.editor.text() === "SELECT 'from the tui'"`)
	logHas("the older draft logged", "was changed elsewhere since")
	eval(t, q, `() => monaco.editor.getEditors()[0].trigger("e2e", "undo")`)
	waitFor(t, q, "the draft back by Ctrl+Z", `() => dbc.editor.text() === "SELECT 'kept' -- more -- mine"`)
	fileIs("the draft saved over the file", `"SELECT 'kept' -- more -- mine"`)
	draftIs("the draft dropped", `""`)
	noJSErrors(q)

	// 5. a console past the keepalive quota, reloaded before its save
	// (setText fires no debounced save): the goodbye cannot carry it, the
	// draft does
	// about 100 KB: past the quota alone, let alone twice over
	big := func(n int) string {
		return `Array.from({ length: 6000 }, (_, i) => "SELECT " + (i + ` + strconv.Itoa(n) + `) + " AS n;").join("\n")`
	}
	eval(t, q, `() => dbc.editor.setText(`+big(0)+`)`)
	q.MustReload()
	q.MustWaitLoad()
	waitConnected(t, q, "lite")
	waitFor(t, q, "the big text back", `() => dbc.editor.text() === `+big(0))
	fileIs("the big text saved", big(0))
	noJSErrors(q)

	// 6. the same, and the window goes away: its release must still go,
	// so a browser tab opened at once gets the tab rather than being told
	// it is open in another one (the claims would lapse only after 15 s)
	eval(t, q, `() => dbc.editor.setText(`+big(1)+`)`)
	q.MustNavigate("about:blank")
	q.MustWaitLoad()
	r := e.page(t, "lite")
	defer r.MustClose()
	waitFor(t, r, "the closed window's tab claimed here", `(k) => !!document.querySelector('#qtabs .qtab[data-key="' + k + '"]')`, key)
	noJSErrors(r)
}
