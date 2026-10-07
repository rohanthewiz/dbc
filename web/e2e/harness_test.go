package e2e

// The harness: build dbc, give it a HOME of its own, run `dbc web`, launch
// Chrome, sign a page in. The checks themselves are in web_test.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const (
	// secret is the login secret every run uses: the server is on loopback
	// for the length of one test, so a fixed one costs nothing and keeps
	// the login URL predictable.
	secret = "e2e-s3cret"

	// waitLimit bounds every wait for the page to reach a state. Generous:
	// a cold Monaco load or a first connect on a slow machine takes seconds,
	// and a wait that passes returns as soon as it does.
	waitLimit = 20 * time.Second
)

// env is one run's world: the temporary HOME, the built binary, the
// server's address and the browser.
type env struct {
	home, bin, base string
	browser         *rod.Browser
	server          *exec.Cmd
	serverLog       *lockedBuffer
	pgDSN           string // DBC_LIVE_PG_DSN, when the Postgres checks run
}

// lockedBuffer collects the server's stderr from its own goroutine (exec's
// copier) while the test may read it, so it needs a lock.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// setup builds and starts everything, registering the teardown with
// t.Cleanup — which also runs when the test panics (a rod Must* call that
// fails does), so a failed run never leaves a dbc web holding its port or a
// Chrome running. A stale server matters beyond the leak: a later run on
// the same port would talk to the OLD binary and its old JavaScript.
func setup(t *testing.T) *env {
	t.Helper()
	if os.Getenv("DBC_E2E") == "" {
		t.Skip("browser checks of dbc web: set DBC_E2E=1 to run them (needs Chrome)")
	}
	chrome := chromePath(t)

	e := &env{home: t.TempDir(), pgDSN: os.Getenv("DBC_LIVE_PG_DSN")}
	e.bin = filepath.Join(e.home, "dbc")
	build(t, e.bin)
	e.writeConfig(t)
	e.seed(t)
	e.startServer(t)
	e.launchBrowser(t, chrome)
	return e
}

// chromePath finds a browser without ever letting rod download one: a
// test that silently fetches 150 MB of Chromium on a laptop is a surprise.
func chromePath(t *testing.T) string {
	if p := os.Getenv("DBC_E2E_CHROME"); p != "" {
		return p
	}
	if runtime.GOOS == "darwin" {
		const mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := os.Stat(mac); err == nil {
			return mac
		}
	}
	if p, ok := launcher.LookPath(); ok {
		return p
	}
	t.Skip("no Chrome found: set DBC_E2E_CHROME to the browser binary")
	return ""
}

// build compiles dbc from this checkout (two levels up), so the run always
// exercises the code — and the embedded JavaScript — in the working tree.
func build(t *testing.T, out string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building dbc: %v\n%s", err, b)
	}
}

// writeConfig writes HOME/.config/dbc/config.toml. A config file replaces
// dbc's built-in demos, so the connections are exactly these: two SQLite
// files (two, so switching has somewhere to go) and, when a DSN is given,
// a Postgres for the schema picker, which SQLite's single schema never
// shows.
func (e *env) writeConfig(t *testing.T) {
	t.Helper()
	dir := filepath.Join(e.home, ".config", "dbc")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("default_connection = \"lite\"\n")
	conn := func(name, driver, dsn string) {
		fmt.Fprintf(&b, "\n[[connection]]\nname = %q\ndriver = %q\ndsn = %q\n", name, driver, dsn)
	}
	conn("lite", "sqlite", "file:"+filepath.Join(e.home, "lite.db"))
	conn("lite2", "sqlite", "file:"+filepath.Join(e.home, "lite2.db"))
	if e.pgDSN != "" {
		conn("pg", "postgres", e.pgDSN)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seed fills the connections through dbc's own headless mode — the same
// binary, so no SQL driver has to be a dependency of this module.
func (e *env) seed(t *testing.T) {
	t.Helper()
	e.dbc(t, "lite", `CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT NOT NULL, breed TEXT, age INTEGER);
INSERT INTO cats (name, breed, age) VALUES ('Tom', 'tabby', 3), ('Mia', 'siamese', 5), ('Leo', NULL, 1)`)
	e.dbc(t, "lite2", `CREATE TABLE dogs (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO dogs (name) VALUES ('Rex')`)
	if e.pgDSN != "" {
		// two schemas of our own beside public, so the picker has a list;
		// dropped again when the test ends
		drop := `DROP SCHEMA IF EXISTS e2e_a CASCADE; DROP SCHEMA IF EXISTS e2e_b CASCADE`
		e.dbc(t, "pg", drop+`; CREATE SCHEMA e2e_a; CREATE SCHEMA e2e_b;
CREATE TABLE e2e_a.alpha (id int); CREATE TABLE e2e_b.beta (id int, label text)`)
		t.Cleanup(func() { e.dbc(t, "pg", drop) })
	}
	e.seedChats(t)
}

// seedChats writes saved assistant conversations into HOME's archive
// (~/.config/dbc/chats, userdata.ChatsDir) so the assistant's empty pane
// has a Recent conversations list to draw. Seven: more than the five the
// pane shows, so its "all N recent…" link is drawn too. The files are
// written by hand, in userdata.Chat's JSON shape, because this module does
// not depend on dbc's own packages.
func (e *env) seedChats(t *testing.T) {
	t.Helper()
	dir := filepath.Join(e.home, ".config", "dbc", "chats")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i := range 7 {
		at := base.Add(time.Duration(i) * time.Minute)
		id := fmt.Sprintf("%s-%09d", at.Format("20060102-150405"), i)
		q := fmt.Sprintf("saved question %d", i+1)
		bs, err := json.Marshal(map[string]any{
			"id": id, "title": q, "conn": "lite", "started": at, "updated": at,
			"msgs": []map[string]string{{"role": "user", "text": q}, {"role": "agent", "text": "an answer"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, id+".json"), bs, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// dbc runs one headless query on conn and fails the test if it fails.
func (e *env) dbc(t *testing.T, conn, sql string) {
	t.Helper()
	cmd := exec.Command(e.bin, "-c", conn, sql)
	cmd.Dir = e.home
	cmd.Env = e.environ()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dbc -c %s: %v\n%s", conn, err, b)
	}
}

// environ is the binary's environment: this process's, with HOME pointing
// at the run's directory (every ~/.config/dbc file — tabs, history,
// connections.toml — lands there, never in the real one) and with the
// variables that would reach outside it removed.
//
// CATS_*: Claude Code sessions on dbc run inside a cats pane, and a dbc
// that inherits CATS_* reports to the LIVE pane and dials its control
// socket. DBC_WEB_SECRET and DBC_DEMO would override the flags and config
// the run sets up.
func (e *env) environ() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CATS_") || k == "HOME" || k == "DBC_WEB_SECRET" || k == "DBC_DEMO" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+e.home)
}

// startServer runs `dbc web` on a free loopback port and waits until it
// answers. The health check must name THIS run's state file: that is how
// we know the server answering is ours and not a leftover on the port.
func (e *env) startServer(t *testing.T) {
	t.Helper()
	addr := freeAddr(t)
	e.base = "http://" + addr
	if health(e.base) != nil {
		t.Fatalf("something already answers on %s", addr)
	}
	e.serverLog = &lockedBuffer{}
	e.server = exec.Command(e.bin, "web", "--no-open", "--listen", addr, "--secret", secret)
	e.server.Dir = e.home
	e.server.Env = e.environ()
	e.server.Stdout = e.serverLog
	e.server.Stderr = e.serverLog
	if err := e.server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.stopServer()
		if t.Failed() {
			t.Logf("dbc web's output:\n%s", e.serverLog.String())
		}
	})

	want := filepath.Join(e.home, ".config", "dbc", "web.bytdb")
	deadline := time.Now().Add(waitLimit)
	for {
		if h := health(e.base); h != nil {
			if h["state_file"] != want {
				t.Fatalf("the server on %s is not this run's: state file %v", addr, h["state_file"])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("dbc web did not come up on %s:\n%s", addr, e.serverLog.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stopServer interrupts dbc web the way Ctrl+C would — it then releases
// every tab's session — and kills it if it has not gone in a few seconds.
func (e *env) stopServer() {
	if e.server == nil || e.server.Process == nil {
		return
	}
	_ = e.server.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = e.server.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = e.server.Process.Kill()
		<-done
	}
	e.server = nil
}

// freeAddr asks the kernel for a free loopback port. It is released before
// dbc binds it, a small race the health check above would catch.
func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// health returns /api/v1/health's data, or nil when nothing answers.
func health(base string) map[string]any {
	c := http.Client{Timeout: time.Second}
	res, err := c.Get(base + "/api/v1/health")
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if json.NewDecoder(res.Body).Decode(&env) != nil {
		return nil
	}
	return env.Data
}

// launchBrowser starts Chrome and lets the server's origin use the
// clipboard: the copy checks read back what the page wrote, which needs
// both grants (the sanitized one is what ClipboardItem writes ask for).
func (e *env) launchBrowser(t *testing.T, chrome string) {
	t.Helper()
	l := launcher.New().Bin(chrome).Headless(os.Getenv("DBC_E2E_HEADFUL") == "")
	u, err := l.Launch()
	if err != nil {
		t.Fatalf("launching %s: %v", chrome, err)
	}
	e.browser = rod.New().ControlURL(u)
	if err := e.browser.Connect(); err != nil {
		l.Kill()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = e.browser.Close()
		l.Kill()
		l.Cleanup() // the profile directory
	})
	err = proto.BrowserGrantPermissions{
		Origin: e.base,
		Permissions: []proto.BrowserPermissionType{
			proto.BrowserPermissionTypeClipboardReadWrite,
			proto.BrowserPermissionTypeClipboardSanitizedWrite,
		},
	}.Call(e.browser)
	if err != nil {
		t.Fatal(err)
	}
}

// probe is installed in every document before the page's own scripts run.
// It records two things the checks assert on:
//
//   - errs: uncaught errors, unhandled promise rejections and
//     console.error calls. A run that logs one fails: those are exactly
//     the bugs a person would not notice (Phase 6's disposed Monaco model
//     and its stray rejection were found this way).
//   - reqs: every fetch the page makes, "METHOD path", so a check can say
//     a key did NOT fire a request (⌘C on a table must not run Show
//     columns), which the screen alone cannot prove.
const probe = `(() => {
  const p = window.__e2e = { errs: [], reqs: [] };
  const why = (x) => x && (x.stack || x.message) || String(x);
  addEventListener("error", (e) => p.errs.push("error: " + (e.error ? why(e.error) : e.message)));
  addEventListener("unhandledrejection", (e) => p.errs.push("unhandledrejection: " + why(e.reason)));
  const ce = console.error;
  console.error = function (...a) { p.errs.push("console.error: " + a.map(why).join(" ")); return ce.apply(this, a); };
  const f = window.fetch;
  window.fetch = function (input, init) {
    const url = typeof input === "string" ? input : input.url;
    p.reqs.push(((init && init.method) || "GET") + " " + url);
    return f.apply(this, arguments);
  };
})()`

// page opens a browser tab — to dbc web, a window of its own — signed in
// through the login link, and waits until the workbench has booted onto
// conn.
func (e *env) page(t *testing.T, conn string) *rod.Page {
	t.Helper()
	p := e.browser.MustPage("")
	p.MustSetViewport(1400, 900, 1, false)
	p.MustEvalOnNewDocument(probe)
	p.MustNavigate(e.base + "/login?s=" + secret)
	p.MustWaitLoad()
	waitConnected(t, p, conn)
	return p
}

// ── waiting and asking ───────────────────────────────────────────────────

// eval runs a JS function expression in the page and returns its value.
func eval(t *testing.T, p *rod.Page, js string, args ...any) any {
	t.Helper()
	r, err := p.Eval(js, args...)
	if err != nil {
		t.Fatalf("eval %s: %v", js, err)
	}
	return r.Value.Val()
}

func evalStr(t *testing.T, p *rod.Page, js string, args ...any) string {
	t.Helper()
	s, _ := eval(t, p, js, args...).(string)
	return s
}

// waitFor polls a JS predicate until it holds. Polling from Go rather than
// rod's p.Timeout(...).Wait: rod's timeout is one deadline on the page's
// context, inherited by every later call made through it, which kills a
// long run part way. On failure it says what the page was showing.
func waitFor(t *testing.T, p *rod.Page, what, js string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		r, err := p.Eval(js, args...)
		if err == nil && r.Value.Bool() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (last error: %v)\n%s", what, err, pageState(p))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pageState is the status bar, the log's last lines and the probe's
// errors: what a person would look at to see why the page got stuck.
func pageState(p *rod.Page) string {
	r, err := p.Eval(`() => {
	  const log = [...document.querySelectorAll("#log > div")].slice(-10).map((d) => d.textContent);
	  return "status: " + document.getElementById("status").textContent +
	    "\nlog:\n  " + log.join("\n  ") + "\nerrors: " + JSON.stringify((window.__e2e || {}).errs);
	}`)
	if err != nil {
		return "(page state unavailable: " + err.Error() + ")"
	}
	return r.Value.Str()
}

// waitConnected waits until the active tab is on conn, the connect is
// over, and the Tables list has been drawn for it.
func waitConnected(t *testing.T, p *rod.Page, conn string) {
	t.Helper()
	waitFor(t, p, "connected to "+conn, `(c) => {
	  const a = document.querySelector("#conns .conn-item.active");
	  return !!dbc.state.ws && dbc.state.active === c && !!a && a.dataset.conn === c &&
	    !document.querySelector("#conns .conn-item.connecting") &&
	    document.getElementById("active-conn").textContent === c &&
	    document.querySelectorAll("#tables li").length > 0;
	}`, conn)
}

// gridSeq is the result on show in the grid (0: none); each new result
// takes a higher one, so "the grid moved on" is a seq above the last.
func gridSeq(t *testing.T, p *rod.Page) int {
	t.Helper()
	n, _ := eval(t, p, `() => dbc.grid.view().seq`).(float64)
	return int(n)
}

// waitResult waits for a result newer than after, whose header holds
// every column in cols, and returns its grid-info line ("3 rows").
func waitResult(t *testing.T, p *rod.Page, after int, cols ...string) string {
	t.Helper()
	waitFor(t, p, fmt.Sprintf("a result with columns %v", cols), `(after, cols) => {
	  if (dbc.grid.view().seq <= after || document.getElementById("grid").hidden) return false;
	  const head = [...document.querySelectorAll("#grid .gh .hc")].map((h) => h.textContent);
	  return cols.every((c) => head.some((h) => h.includes(c)));
	}`, after, cols)
	return evalStr(t, p, `() => document.getElementById("grid-info").textContent`)
}

// reqs returns the fetches the page has made since the last reset, and
// resets them.
func reqs(t *testing.T, p *rod.Page) []string {
	t.Helper()
	v, _ := eval(t, p, `() => { const r = window.__e2e.reqs; window.__e2e.reqs = []; return r; }`).([]any)
	out := make([]string, len(v))
	for i, s := range v {
		out[i], _ = s.(string)
	}
	return out
}

// jsErrors returns, and clears, what the probe caught.
func jsErrors(t *testing.T, p *rod.Page) []string {
	t.Helper()
	v, _ := eval(t, p, `() => { const e = (window.__e2e || {errs: []}).errs; if (window.__e2e) window.__e2e.errs = []; return e; }`).([]any)
	out := make([]string, len(v))
	for i, s := range v {
		out[i], _ = s.(string)
	}
	return out
}

// clipboard reads the system clipboard back through the page.
func clipboard(t *testing.T, p *rod.Page) string {
	t.Helper()
	return evalStr(t, p, `() => navigator.clipboard.readText()`)
}

// setClipboard puts a sentinel there, so a later read proves a write.
func setClipboard(t *testing.T, p *rod.Page, s string) {
	t.Helper()
	eval(t, p, `(s) => navigator.clipboard.writeText(s)`, s)
}

// ── input ────────────────────────────────────────────────────────────────

// rightClick opens the context menu of the element sel names.
func rightClick(t *testing.T, p *rod.Page, sel string) {
	t.Helper()
	clickAt(t, p, sel, proto.InputMouseButtonRight)
}

// clickAt clicks the element sel names by its coordinates, looked up in
// the page at the moment of the click, rather than through an element
// handle. The sidebar's connection list and the tab strip are rebuilt on
// many events (a connect, a tab's marks, the in-use dashes), so a handle
// found a moment earlier can be detached by the time it is clicked — and
// rod's MustClick on a detached node waits for it to become interactable,
// which it never does: the step hung until the suite's timeout (N-120).
// A coordinate click lands on whatever is drawn there now, which is the
// same row redrawn. A right-click waits for the menu it opens.
//
// The target must be drawn — a box of some size — not merely present: a
// row inside the folded sidebar is in the DOM with a zero box, and a
// click at its "centre" (0, 0) lands on the top bar and does nothing,
// which surfaced only as a later wait timing out.
func clickAt(t *testing.T, p *rod.Page, sel string, button proto.InputMouseButton) {
	t.Helper()
	waitFor(t, p, sel+" drawn", `(s) => { const e = document.querySelector(s);
	  if (!e) return false;
	  const r = e.getBoundingClientRect();
	  return r.width > 0 && r.height > 0; }`, sel)
	box, ok := eval(t, p, `(s) => { const e = document.querySelector(s);
	  if (!e) return null;
	  e.scrollIntoView({ block: "nearest", inline: "nearest" });
	  const r = e.getBoundingClientRect();
	  return [r.x + r.width / 2, r.y + r.height / 2]; }`, sel).([]any)
	if !ok {
		t.Fatalf("click %s: it went away before the click", sel)
	}
	p.Mouse.MustMoveTo(box[0].(float64), box[1].(float64))
	if err := p.Mouse.Click(button, 1); err != nil {
		t.Fatalf("click %s: %v", sel, err)
	}
	if button == proto.InputMouseButtonRight {
		waitFor(t, p, "a menu", `() => !!document.querySelector(".menu")`)
	}
}

// tabSelector names the query tab titled title on the strip by its key —
// a selector clickAt can look up again at the moment of the click. The
// first such tab, when two share a title.
func tabSelector(t *testing.T, p *rod.Page, title string) string {
	t.Helper()
	waitFor(t, p, "the tab "+title, `(s) => [...document.querySelectorAll("#qtabs .qtab")]
	  .some((b) => b.querySelector(".qt").textContent === s)`, title)
	return `#qtabs .qtab[data-key="` + evalStr(t, p, `(s) => [...document.querySelectorAll("#qtabs .qtab")]
	  .find((b) => b.querySelector(".qt").textContent === s).dataset.key`, title) + `"]`
}

// menuPick clicks the open menu's row labeled label.
func menuPick(t *testing.T, p *rod.Page, label string) {
	t.Helper()
	el, err := p.ElementR(".menu .mitem .ml", "^"+regexp.QuoteMeta(label)+"$")
	if err != nil {
		t.Fatalf("no menu row %q: %v", label, err)
	}
	el.MustClick()
}

// chord dispatches a modifier+key press as Chrome itself would for a real
// keyboard. CDP's synthetic keys do not run the browser's editing commands
// on their own — on a Mac those come from the app menu, not the renderer —
// so a ⌘C that should copy says so in Commands. That is what makes "⌘C on
// a table still copies" a real check rather than a no-op that passes.
func chord(t *testing.T, p *rod.Page, mod int, key, code string, vk int, commands ...string) {
	t.Helper()
	for _, typ := range []proto.InputDispatchKeyEventType{proto.InputDispatchKeyEventTypeRawKeyDown, proto.InputDispatchKeyEventTypeKeyUp} {
		ev := proto.InputDispatchKeyEvent{Type: typ, Modifiers: mod, Key: key, Code: code,
			WindowsVirtualKeyCode: vk, NativeVirtualKeyCode: vk}
		if typ == proto.InputDispatchKeyEventTypeRawKeyDown {
			ev.Commands = commands
		}
		if err := ev.Call(p); err != nil {
			t.Fatalf("key %s: %v", key, err)
		}
	}
}

// CDP modifier bits (Input.dispatchKeyEvent).
const (
	modAlt  = 1
	modCtrl = 2
	modMeta = 4
)
