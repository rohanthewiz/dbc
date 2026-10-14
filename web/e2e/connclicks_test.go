package e2e

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// connClicks locks down what happens around the Connections list and the
// strip when a connection is clicked, cannot be reached, or goes:
//
//  1. A click on a connection never types into a query tab's console. On a
//     query tab it switches the connection, and the editor shows each
//     database's console as it was left. A script tab types the name in at
//     the caret — but only into its own document: clicked while the script
//     is still being read, the editor still showed the tab left (a
//     console), and the name landed there (app.js typeConn).
//  2. A tab whose FIRST connect fails — a Postgres in Docker while Docker is
//     still starting; here a read-only SQLite file in a directory that does
//     not exist — stays where it was headed. Its workspace still names the
//     config's default as active (workspace.New), and the page used to take
//     that for the tab's connection: the tab left its connection group (an
//     orphan beside it), was saved on the default, and showed the default
//     database's console. It now stays in the group — across a reload too,
//     and when it comes back on screen mid-dial — the + lands there, and
//     the page says it is on nothing.
//  3. A tab closed while on screen stays closed (closeOnScreen), and a
//     connection group whose connection is gone opens no tab: either would
//     leave a tab outside the group it belonged to.
//
// The step leaves the window as it found it (its tab, group, script and
// connection removed), and stands on boot alone, so DBC_E2E_STEPS=clicks
// runs it by itself.
func connClicks(t *testing.T, e *env, p *rod.Page) {
	at := func(sel string) {
		t.Helper()
		clickAt(t, p, sel, proto.InputMouseButtonLeft)
	}
	row := func(conn string) string { return `#conns .conn-item[data-conn="` + conn + `"]` }
	text := func() string {
		t.Helper()
		return evalStr(t, p, `() => dbc.editor.text()`)
	}
	// onConsole waits for the tab on screen's console to be the editor's
	// document (followConsole runs after the connect lands)
	onConsole := func() {
		t.Helper()
		waitFor(t, p, "the tab's console in the editor", `() => {
		  const t = dbc.state.tab, k = dbc.editor.docKey();
		  return !!t.console && k.startsWith("c:") && k.endsWith("/" + t.console);
		}`)
	}

	// closeOnScreen brings tab k on screen and closes it from its ✕, then
	// checks it is gone for good. Closing the tab on screen brings its
	// neighbour on, and activate used to save the tab it left — the closed
	// one — with a PUT that raced the close's DELETE: landing second, it put
	// the tab back on the server, and the next boot claimed it, an extra
	// tab outside the group it was closed from. No PUT for k may follow the
	// click, and the server must not hold it.
	closeOnScreen := func(k string) {
		t.Helper()
		at(`#qtabs .qtab[data-key="` + k + `"]`)
		waitFor(t, p, "the tab on screen", `(k) => dbc.state.tab.key === k`, k)
		eval(t, p, `(k) => {
		  window.__closedPuts = [];
		  window.__fetchBeforeClose = window.fetch;
		  window.fetch = (u, o) => {
		    if (((o && o.method) || "GET") === "PUT" && String(u).includes("/api/v1/tabs/" + encodeURIComponent(k))) window.__closedPuts.push(String(u));
		    return window.__fetchBeforeClose(u, o);
		  };
		}`, k)
		at(`#qtabs .qtab[data-key="` + k + `"] [data-close]`)
		waitFor(t, p, "the tab closed", `(k) => !document.querySelector('#qtabs .qtab[data-key="' + k + '"]') && dbc.state.tab.key !== k`, k)
		waitFor(t, p, "the tab gone from the server", `async (k) => {
		  const r = await (await window.__fetchBeforeClose("/api/v1/tabs")).json();
		  return !(r.data || []).some((x) => x.id === k);
		}`, k)
		if got := evalStr(t, p, `() => { window.fetch = window.__fetchBeforeClose; return window.__closedPuts.join(" "); }`); got != "" {
			t.Fatalf("closing the tab on screen saved it again: %s", got)
		}
	}

	// ── 1. a click on a connection never types into a console ─────────────
	home := evalStr(t, p, `() => dbc.state.tab.key`)
	key := evalStr(t, p, `() => { dbc.cmd.newTab(); return dbc.state.tab.key; }`)
	at(row("lite"))
	waitConnected(t, p, "lite")
	onConsole()
	const mine = "SELECT 'typed on lite'"
	eval(t, p, `(s) => dbc.editor.setText(s)`, mine)
	at(row("lite2"))
	waitConnected(t, p, "lite2")
	onConsole()
	other := text()
	if strings.Contains(other, `"lite`) {
		t.Fatalf("lite2's console holds a typed connection name: %q", other)
	}
	at(row("lite"))
	waitConnected(t, p, "lite")
	onConsole()
	if got := text(); got != mine {
		t.Fatalf("back on lite the console reads %q, want %q", got, mine)
	}

	// a script tab clicked while its file is still being read: the editor
	// still shows lite's console, and a click on a connection must not
	// type into it — it says to wait instead. Once the script shows, the
	// click types the name in, there.
	dir := filepath.Join(e.home, ".config", "dbc", "scripts")
	const script = "e2e_clicks.go"
	body := "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, script), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, script)) })
	eval(t, p, `(name) => {
	  window.__realFetch = window.fetch;
	  window.fetch = async (u, o) => {
	    const m = (o && o.method) || "GET";
	    if (m === "GET" && String(u).split("?")[0].endsWith("/api/v1/scripts/" + name)) await new Promise((r) => setTimeout(r, 1500));
	    return window.__realFetch(u, o);
	  };
	  dbc.cmd.openScript(name);
	}`, script)
	waitFor(t, p, "the script tab chosen, its file not yet read", `(name) => dbc.state.tab.script === name && dbc.editor.docKey().startsWith("c:")`, script)
	at(row("lite2"))
	// the click's handler has run (clickAt returns once the press is
	// dispatched); the editor is still on lite's console
	if got := text(); got != mine || !strings.HasPrefix(evalStr(t, p, `() => dbc.editor.docKey()`), "c:") {
		t.Fatalf("a click while the script opened typed into lite's console: %q", got)
	}
	waitFor(t, p, "the click refused while the script opens", `() => document.getElementById("status").textContent.includes("still opening")`)
	waitFor(t, p, "the script on screen", `(name) => dbc.editor.docKey() === "s:" + name`, script)
	eval(t, p, `() => { window.fetch = window.__realFetch; }`)
	at(row("lite2"))
	waitFor(t, p, `"lite2" typed into the script`, `() => dbc.editor.text().includes('"lite2"')`)
	// back to the saved text, so the tab closes without asking
	eval(t, p, `(s) => { dbc.editor.setText(s); dbc.cmd.closeTab(); }`, body)
	// (the neighbour that comes on screen may be any kind of tab — after the
	// whole suite, another script's)
	waitFor(t, p, "the script tab closed", `(name) => dbc.state.tab.script !== name &&
	  ![...document.querySelectorAll("#qtabs .qtab .qt")].some((x) => x.textContent === name)`, script)
	at(`#qtabs .qtab[data-key="` + key + `"]`)
	waitConnected(t, p, "lite")
	onConsole()
	if got := text(); got != mine {
		t.Fatalf("lite's console after the script tab: %q, want %q", got, mine)
	}

	// ── 2. a tab whose first connect fails stays where it was headed ──────
	// Two connections no tab can reach: down, whose dial fails at once,
	// and slow, a Postgres whose port accepts and then never answers — what
	// Docker's port proxy does while the server in the container is still
	// starting — until the test lets it fail.
	hole := blackhole(t)
	broken := "file:" + filepath.Join(e.home, "no", "such", "dir", "down.db") + "?mode=ro"
	slow := "postgres://e2e:e2e@" + hole.addr + "/e2e?sslmode=disable&connect_timeout=60"
	for _, c := range [][2]string{{"down", "sqlite"}, {"slow", "postgres"}} {
		dsn := map[string]string{"down": broken, "slow": slow}[c[0]]
		eval(t, p, `async (name, driver, dsn) => {
		  const r = await fetch("/api/v1/conns", { method: "POST", headers: { "Content-Type": "application/json" },
		    body: JSON.stringify({ name, driver, dsn }) });
		  if (!r.ok) throw new Error("adding " + name + ": " + r.status + " " + await r.text());
		}`, c[0], c[1], dsn)
	}
	t.Cleanup(func() {
		_, _ = p.Eval(`() => Promise.all(["down", "slow"].map((n) => fetch("/api/v1/conns/" + n, { method: "DELETE" })))`)
	})
	waitFor(t, p, "down and slow in the list", `() => !!document.querySelector('#conns .conn-item[data-conn="down"]') &&
	  !!document.querySelector('#conns .conn-item[data-conn="slow"]')`)

	// a connection group on each, with no tab yet: an idle chip opens one.
	// No tab can reach either to start it from a menu, so they come in as a
	// reload restores any group: from the layout.
	eval(t, p, `async () => {
	  const l = (await (await fetch("/api/v1/layout")).json()).data || {};
	  const gs = JSON.parse(l.groups || "[]").concat([{ name: "down", conn: "down", color: 3 }, { name: "slow", conn: "slow", color: 4 }]);
	  await fetch("/api/v1/layout", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ groups: JSON.stringify(gs) }) });
	}`)
	p.MustReload()
	p.MustWaitLoad()
	waitConnected(t, p, "lite")

	// inGroup: tab k sits right behind group g's chip, in its colour
	inGroup := `(k, g) => {
	  const s = [...document.querySelectorAll("#qtabs > *")];
	  const chip = s.findIndex((x) => x.matches('.qchip[data-group="' + g + '"]'));
	  const tab = s.findIndex((x) => x.dataset.key === k);
	  return chip >= 0 && !s[chip].classList.contains("idle") && tab === chip + 1 && s[tab].classList.contains("grp");
	}`
	// nowhere: the tab on screen is on nothing, headed for conn, and the
	// page says so. The status is the page's word for exactly this (app.js
	// onNowhere), so the wait cannot end before the failure has landed.
	nowhere := func(conn string) string {
		return `() => {
		  const t = dbc.state.tab;
		  return !!t && t.conn === "` + conn + `" && dbc.state.active === "" &&
		    !document.querySelector("#conns .conn-item.active") && !document.querySelector("#conns .conn-item.connecting") &&
		    document.getElementById("active-conn").textContent === "not connected" &&
		    document.getElementById("status").textContent.includes("the connect to ` + conn + ` failed");
		}`
	}

	// mid-dial, a tab is in the group it is dialing — on screen, and again
	// when it comes back on screen (activate → applyState: its workspace
	// names the default until a connect lands)
	at(`#qtabs .qchip.idle[data-group="slow"]`)
	dialing := evalStr(t, p, `() => dbc.state.tab.key`)
	waitFor(t, p, "dialing slow", `() => !!document.querySelector('#conns .conn-item.connecting[data-conn="slow"]')`)
	waitFor(t, p, "the dialing tab behind slow's chip", inGroup, dialing, "slow")
	at(`#qtabs .qtab[data-key="` + key + `"]`)
	waitConnected(t, p, "lite")
	at(`#qtabs .qtab[data-key="` + dialing + `"]`)
	waitFor(t, p, "back on the dialing tab, still dialing slow", `(k) => dbc.state.tab.key === k &&
	  document.getElementById("status").textContent.includes("connecting to slow") &&
	  !!document.querySelector('#conns .conn-item.connecting[data-conn="slow"]') && !document.querySelector("#conns .conn-item.active")`, dialing)
	waitFor(t, p, "the dialing tab still behind slow's chip", inGroup, dialing, "slow")
	if got := evalStr(t, p, `() => dbc.state.tab.conn`); got != "slow" {
		t.Fatalf("back mid-dial, the tab reads as on %q, want slow", got)
	}
	// the server lets go: the dial fails, and the tab stays in slow's group
	hole.release()
	waitFor(t, p, "the dial failed, the tab on nothing headed for slow", nowhere("slow"))
	waitFor(t, p, "the failed tab still behind slow's chip", inGroup, dialing, "slow")
	closeOnScreen(dialing)

	// a first connect that fails at once: on nothing, said so — and in
	// down's group, not beside it
	at(`#qtabs .qchip.idle[data-group="down"]`)
	waitFor(t, p, "the new tab on nothing, headed for down", nowhere("down"))
	failed := evalStr(t, p, `() => dbc.state.tab.key`)
	waitFor(t, p, "the tab behind down's chip, in its group", inGroup, failed, "down")
	// the default's console never came into it, and a new tab from it lands
	// in down, as the + says
	if got := evalStr(t, p, `() => (dbc.state.tab.console || "") + "|" + dbc.editor.text()`); got != "|" {
		t.Fatalf("the failed tab took a console and text: %q", got)
	}
	waitFor(t, p, "+ marked for down", `() => { const b = document.querySelector("#qnew");
	  return b.classList.contains("grp") && b.title.includes("in group down") && b.title.includes(" on down "); }`)
	// and it is saved there, so a restart puts it back in down
	waitFor(t, p, "saved on down", `async (k) => {
	  const r = await (await fetch("/api/v1/tabs")).json();
	  return (r.data || []).some((t) => t.id === k && t.conn === "down");
	}`, failed)

	// a reload brings it back in its group; its look tries down again
	p.MustReload()
	p.MustWaitLoad()
	waitFor(t, p, "back on nothing after the reload, headed for down", nowhere("down"))
	waitFor(t, p, "still in down's group after the reload", inGroup, failed, "down")

	// leave as found, on the way checking that a group whose connection is
	// gone opens no tab (a tab opened there would land in another
	// connection, outside the group): the failed tab closed, down removed,
	// its idle chip refusing, both groups ungrouped from their chips' menus,
	// the step's first tab closed and the tab it started from back on
	// screen. slow's removal is the cleanup's.
	closeOnScreen(failed)
	eval(t, p, `async () => { const r = await fetch("/api/v1/conns/down", { method: "DELETE" }); if (!r.ok) throw new Error("removing down: " + r.status); }`)
	waitFor(t, p, "down gone from the list", `() => !document.querySelector('#conns .conn-item[data-conn="down"]')`)
	n := evalStr(t, p, `() => String(document.querySelectorAll("#qtabs .qtab").length)`)
	at(`#qtabs .qchip.idle[data-group="down"]`)
	waitFor(t, p, "the refusal", `() => [...document.querySelectorAll("#log > div")].some((d) => d.textContent.includes("down, which is no longer a connection"))`)
	if got := evalStr(t, p, `() => String(document.querySelectorAll("#qtabs .qtab").length)`); got != n {
		t.Fatalf("a group whose connection is gone opened a tab: %s tabs, was %s", got, n)
	}
	for _, g := range []string{"down", "slow"} {
		rightClick(t, p, `#qtabs .qchip.idle[data-group="`+g+`"]`)
		menuPick(t, p, "Ungroup")
		waitFor(t, p, g+"'s chip gone", `(g) => !document.querySelector('#qtabs .qchip[data-group="' + g + '"]')`, g)
	}
	closeOnScreen(key)
	if evalStr(t, p, `(k) => document.querySelector('#qtabs .qtab[data-key="' + k + '"]') ? "yes" : ""`, home) != "" {
		at(`#qtabs .qtab[data-key="` + home + `"]`)
	}
	waitFor(t, p, "no group left on down or slow", `async () => !JSON.parse(((await (await fetch("/api/v1/layout")).json()).data || {}).groups || "[]")
	  .some((g) => g.name === "down" || g.name === "slow")`)
}

// hole is a TCP listener that accepts and never answers — a server port
// whose process is not listening yet behind a proxy that is — until
// release closes what it holds, failing every dial in flight and after.
type hole struct {
	addr    string
	release func()
}

func blackhole(t *testing.T) hole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = ln.Close()
			mu.Lock()
			defer mu.Unlock()
			for _, c := range held {
				_ = c.Close()
			}
		})
	}
	t.Cleanup(release)
	return hole{addr: ln.Addr().String(), release: release}
}
