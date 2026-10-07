package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// These tests run a real server on a loopback port, gonotes' way, and talk
// to it over HTTP the way the page does — including the SSE stream, read
// line by line. The database is a fresh, seeded in-memory SQLite per test.

const testSecret = "test-secret"

var dbSeq atomic.Int64

// testEnv is a running server and a client for it.
type testEnv struct {
	t    *testing.T
	srv  *Server
	base string
	hc   *http.Client
}

// newTestEnv starts a server; tweaks adjust the config and options first
// (a scripted assistant, a chats or scripts directory).
func newTestEnv(t *testing.T, tweaks ...func(*config.Config, *Options)) *testEnv {
	t.Helper()
	name := "demo-sqlite"
	cfg := &config.Config{
		MaxRows: 1000, MaxDisplayRows: 2000, AIContextRows: config.DefaultAIContextRows,
		ConnIdleTimeout: time.Hour, DefaultConnection: name,
		Connections: []config.Connection{{Name: name, Driver: "sqlite",
			DSN: fmt.Sprintf("file:webtest%d?mode=memory&cache=shared", dbSeq.Add(1))}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, name); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ready := make(chan string, 1)
	opt := Options{
		Listen: "127.0.0.1:0", Secret: testSecret,
		Ready: func(login string) { ready <- login },
	}
	for _, tw := range tweaks {
		tw(cfg, &opt)
	}
	srv, err := New(cfg, mgr, opt)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Run blocks until SIGINT; the listener lives for the test binary, as
	// in gonotes' tests. Shutdown releases the workspaces' sessions.
	go func() { _ = srv.Run() }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	t.Cleanup(srv.Shutdown)
	return &testEnv{t: t, srv: srv, base: srv.URL(), hc: &http.Client{
		Timeout: 10 * time.Second,
		// the login's redirect is asserted, not followed
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// req is one request. Bearer auth unless the caller sets headers of its own.
func (e *testEnv) req(method, path, body string, hdr map[string]string) *http.Response {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, e.base+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if hdr == nil {
		hdr = map[string]string{"Authorization": "Bearer " + testSecret}
	}
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	res, err := e.hc.Do(r)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

type testEnvelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
	Stopped bool            `json:"stopped"`
}

// api makes an authenticated request and decodes the envelope, failing the
// test unless the status is want.
func (e *testEnv) api(method, path, body string, want int) testEnvelope {
	e.t.Helper()
	res := e.req(method, path, body, nil)
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, res.StatusCode, want, b)
	}
	var env testEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		e.t.Fatalf("%s %s: not an envelope: %s", method, path, b)
	}
	return env
}

func decodeData[T any](t *testing.T, env testEnvelope) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(env.Data, &v); err != nil {
		t.Fatalf("data: %v: %s", err, env.Data)
	}
	return v
}

// sseEvent is one {type, data} event off a tab's stream.
type sseEvent struct {
	Type string          `json:"type"`
	WS   string          `json:"ws"` // the query tab; "" for the window's own
	Data json.RawMessage `json:"data"`
}

// stream is a tab's open SSE stream, its events on a channel.
type stream struct {
	events chan sseEvent
	body   io.Closer
}

// open makes a workspace and attaches its stream, as the page's boot does.
func (e *testEnv) open() (id string, s *stream) {
	e.t.Helper()
	st := decodeData[wsState](e.t, e.api("POST", "/api/v1/ws", "", 200))
	res := e.req("GET", "/api/v1/ws/"+st.ID+"/events", "", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		e.t.Fatalf("events: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	s = &stream{events: make(chan sseEvent, 256), body: res.Body}
	e.t.Cleanup(func() { _ = res.Body.Close() })
	go func() {
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev sseEvent
			if json.Unmarshal([]byte(line), &ev) == nil {
				s.events <- ev
			}
		}
		close(s.events)
	}()
	// the stream is attached once the hub counts it; wait for that, so no
	// event sent next can miss it
	deadline := time.Now().Add(3 * time.Second)
	for {
		t, _ := e.srv.hub.get(st.ID)
		if t.win.sse.ClientCount() > 0 {
			break
		}
		if time.Now().After(deadline) {
			e.t.Fatal("stream never attached")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return st.ID, s
}

// await reads events until one of type typ arrives, returning it and every
// log line seen on the way.
func (s *stream) await(t *testing.T, typ string) (sseEvent, []string) {
	t.Helper()
	var logs []string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatalf("stream closed waiting for %q", typ)
			}
			if ev.Type == "log" {
				var l logLine
				_ = json.Unmarshal(ev.Data, &l)
				logs = append(logs, l.Text)
			}
			if ev.Type == typ {
				return ev, logs
			}
		case <-timeout:
			t.Fatalf("no %q event; saw logs %q", typ, logs)
		}
	}
}

// awaitLog reads events until a log line reading text arrives, returning
// every log line seen on the way, that one included.
func (s *stream) awaitLog(t *testing.T, text string) (sseEvent, []string) {
	t.Helper()
	var logs []string
	for {
		ev, seen := s.await(t, "log")
		logs = append(logs, seen...)
		if seen[len(seen)-1] == text {
			return ev, logs
		}
	}
}

// connected opens a workspace, attaches its stream and connects it.
func (e *testEnv) connected() (string, *stream) {
	e.t.Helper()
	id, s := e.open()
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"demo-sqlite"}`, 200)
	ev, _ := s.await(e.t, "conn")
	c := decodeData[connEvent](e.t, testEnvelope{Data: ev.Data})
	if c.Active != "demo-sqlite" || c.Failed || len(c.Tables) == 0 {
		e.t.Fatalf("conn event = %+v", c)
	}
	return id, s
}

func runBody(buffer string, caret int, all bool) string {
	b, _ := json.Marshal(runReq{Buffer: buffer, Caret: caret, All: all})
	return string(b)
}

// ---------------------------------------------------------------------------
// Access control
// ---------------------------------------------------------------------------

func TestHealthIsPublic(t *testing.T) {
	e := newTestEnv(t)
	res := e.req("GET", "/api/v1/health", "", map[string]string{})
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("health = %d", res.StatusCode)
	}
}

func TestNoTokenIs401(t *testing.T) {
	e := newTestEnv(t)
	for _, p := range []string{"/api/v1/conns", "/"} {
		res := e.req("GET", p, "", map[string]string{})
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Errorf("GET %s without a token = %d, want 401", p, res.StatusCode)
		}
		if p == "/" && !strings.Contains(string(body), "not signed in") {
			t.Errorf("the page's 401 does not say how to sign in: %s", body)
		}
	}
	res := e.req("GET", "/api/v1/conns", "", map[string]string{"Authorization": "Bearer wrong"})
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Errorf("a wrong bearer = %d, want 401", res.StatusCode)
	}
}

func TestBearerWorks(t *testing.T) {
	e := newTestEnv(t)
	env := e.api("GET", "/api/v1/conns", "", 200)
	if strings.Contains(string(env.Data), "mode=memory") {
		t.Fatalf("a DSN reached the browser: %s", env.Data)
	}
	if !strings.Contains(string(env.Data), `"demo-sqlite"`) {
		t.Fatalf("conns = %s", env.Data)
	}
}

// The DNS-rebinding guard: a hostile page that points its own hostname at
// 127.0.0.1 still sends that hostname, and is refused — even with the
// secret, and even on the public health check.
func TestBadHostIs403(t *testing.T) {
	e := newTestEnv(t)
	port := e.srv.rw.GetListenPort()
	for _, host := range []string{"evil.example:" + port, "127.0.0.1:1", "evil.example"} {
		for _, p := range []string{"/api/v1/health", "/api/v1/conns"} {
			res := e.req("GET", p, "", map[string]string{
				"Host": host, "Authorization": "Bearer " + testSecret})
			res.Body.Close()
			if res.StatusCode != 403 {
				t.Errorf("GET %s with Host %q = %d, want 403", p, host, res.StatusCode)
			}
		}
	}
	res := e.req("GET", "/api/v1/conns", "", map[string]string{
		"Host": "localhost:" + port, "Authorization": "Bearer " + testSecret})
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("Host localhost = %d, want 200", res.StatusCode)
	}
}

func TestCrossSitePostIs403(t *testing.T) {
	e := newTestEnv(t)
	res := e.req("POST", "/api/v1/ws", "", map[string]string{
		"Authorization": "Bearer " + testSecret, "Origin": "https://evil.example"})
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("cross-site POST = %d, want 403", res.StatusCode)
	}
	// the same request from the page's own origin goes through
	res = e.req("POST", "/api/v1/ws", "", map[string]string{
		"Authorization": "Bearer " + testSecret, "Origin": e.base})
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("same-origin POST = %d, want 200", res.StatusCode)
	}
}

func TestLoginSetsSessionCookie(t *testing.T) {
	e := newTestEnv(t)
	res := e.req("GET", "/login?s=nope", "", map[string]string{})
	res.Body.Close()
	if res.StatusCode != 401 || len(res.Cookies()) != 0 {
		t.Fatalf("a wrong secret = %d with %d cookies, want 401 and none", res.StatusCode, len(res.Cookies()))
	}

	res = e.req("GET", "/login?s="+testSecret, "", map[string]string{})
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("login = %d → %q, want 303 → /", res.StatusCode, res.Header.Get("Location"))
	}
	var sess *http.Cookie
	for _, c := range res.Cookies() {
		if strings.HasPrefix(c.Name, "dbc_session_") {
			sess = c
		}
	}
	if sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v, want HttpOnly and SameSite=Strict", sess)
	}
	if strings.Contains(sess.Value, testSecret) {
		t.Fatal("the cookie carries the secret itself")
	}

	res = e.req("GET", "/", "", map[string]string{"Cookie": sess.Name + "=" + sess.Value})
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), `id="editor"`) {
		t.Fatalf("the page with the cookie = %d", res.StatusCode)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	e := newTestEnv(t)
	for _, c := range []struct {
		path string
		hdr  map[string]string
	}{
		{"/", nil},
		{"/", map[string]string{}}, // the sign-in notice
		{"/api/v1/health", map[string]string{}},
		{"/static/js/app.js", map[string]string{}},
	} {
		res := e.req("GET", c.path, "", c.hdr)
		res.Body.Close()
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("GET %s: CSP = %q", c.path, csp)
		}
		if res.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("GET %s: no Referrer-Policy", c.path)
		}
	}
}

// ---------------------------------------------------------------------------
// Running statements
// ---------------------------------------------------------------------------

// The whole Phase 2 loop: pick a connection, run a query, see its rows.
func TestRunShowsResult(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()

	env := e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT name, age FROM cats ORDER BY name", 0, false), 200)
	if tag := decodeData[map[string]string](t, env)["tag"]; tag != "query" {
		t.Fatalf("tag = %q", tag)
	}
	ev, logs := s.await(t, "run")
	run := decodeData[runEvent](t, testEnvelope{Data: ev.Data})
	if !run.OK || !run.HasResult || !strings.Contains(run.Status, "rows in") {
		t.Fatalf("run event = %+v", run)
	}
	if len(logs) == 0 || !strings.HasPrefix(logs[0], "running query on demo-sqlite") {
		t.Fatalf("logs = %q, want the workspace's own words", logs)
	}

	pg := decodeData[resultPage](t, e.api("GET", "/api/v1/ws/"+id+"/result", "", 200))
	if strings.Join(pg.Columns, ",") != "name,age" || pg.Total != 8 || len(pg.Cells) != 8 || *pg.Cells[0][0] != "Bella" {
		t.Fatalf("result page = %+v", pg)
	}
}

// The caret picks the statement, in UTF-16 units as the browser counts
// them: the é before it is one unit but two bytes.
func TestRunPicksStatementUnderCaret(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	buf := "SELECT 'é' AS first;\nSELECT 2 AS second;"
	caret := len([]rune("SELECT 'é' AS first;\nSELECT")) // mid second statement
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody(buf, caret, false), 200)
	s.await(t, "run")
	pg := decodeData[resultPage](t, e.api("GET", "/api/v1/ws/"+id+"/result", "", 200))
	if len(pg.Columns) != 1 || pg.Columns[0] != "second" {
		t.Fatalf("ran the wrong statement: %v", pg.Columns)
	}

	e.api("POST", "/api/v1/ws/"+id+"/run", runBody(buf, 0, true), 200)
	ev, logs := s.await(t, "run")
	if run := decodeData[runEvent](t, testEnvelope{Data: ev.Data}); !run.OK || run.Tag != "all 2 statements" {
		t.Fatalf("run all = %+v (logs %q)", run, logs)
	}
}

func TestRefusalsAre400(t *testing.T) {
	e := newTestEnv(t)
	id, _ := e.connected()
	env := e.api("POST", "/api/v1/ws/"+id+"/run", runBody("  -- nothing\n", 0, false), 400)
	if !strings.Contains(env.Error, "nothing to run") {
		t.Fatalf("error = %q", env.Error)
	}
	e.api("POST", "/api/v1/ws/"+id+"/run", `{not json`, 400)
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"nope"}`, 400)
	e.api("POST", "/api/v1/ws/nope/run", runBody("SELECT 1", 0, false), 404)
}

// A slow statement: busy while it runs (a second run is a 409 in the TUI's
// words), and Ctrl+K stops it on the server.
func TestBusyThenCancel(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	slow := "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n) SELECT count(*) FROM n"
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody(slow, 0, false), 200)
	s.await(t, "busy")

	env := e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 1", 0, false), 409)
	if !strings.HasPrefix(env.Error, "busy — query is still running") {
		t.Fatalf("busy error = %q", env.Error)
	}
	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if !st.Busy {
		t.Fatalf("state not busy mid-run: %+v", st)
	}

	e.api("POST", "/api/v1/ws/"+id+"/cancel", "", 200)
	ev, logs := s.await(t, "run")
	run := decodeData[runEvent](t, testEnvelope{Data: ev.Data})
	if run.OK || !run.Stopped || !strings.HasPrefix(run.Status, "stopped after") {
		t.Fatalf("after cancel: %+v (logs %q)", run, logs)
	}
	// the slot is free again
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 1", 0, false), 200)
	s.await(t, "run")
}

// A session that may hold state shows the badge, and keeps it across runs
// on the same tab — the pinned session. Session.Stateful errs on the side of
// yes, so even a ROLLBACK does not lower it: only a new session does.
func TestSessionStateBadge(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 1", 0, false), 200)
	ev, _ := s.await(t, "run")
	if run := decodeData[runEvent](t, testEnvelope{Data: ev.Data}); run.Stateful {
		t.Fatalf("a plain read raised the badge: %+v", run)
	}
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("BEGIN", 0, false), 200)
	ev, _ = s.await(t, "run")
	if run := decodeData[runEvent](t, testEnvelope{Data: ev.Data}); !run.Stateful {
		t.Fatalf("BEGIN did not raise the badge: %+v", run)
	}
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 2", 0, false), 200)
	ev, _ = s.await(t, "run")
	if run := decodeData[runEvent](t, testEnvelope{Data: ev.Data}); !run.Stateful {
		t.Fatalf("the badge fell on the next run inside the transaction: %+v", run)
	}
	// a reload reattaches through the state, which carries the badge too
	if st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200)); !st.Stateful {
		t.Fatalf("the reattach state lost the badge: %+v", st)
	}
}

// An idle tab — no stream attached — has its session released once
// conn_idle_timeout passes, rolling back what it held; much later it is
// forgotten.
func TestIdleTabReleasedThenForgotten(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("BEGIN", 0, false), 200)
	s.await(t, "run")
	tb, _ := e.srv.hub.get(id)
	if _, stateful := tb.ws.Session(); !stateful {
		t.Fatal("no open transaction to release")
	}
	_ = s.body.Close()
	for deadline := time.Now().Add(3 * time.Second); tb.win.sse.ClientCount() > 0; {
		if time.Now().After(deadline) {
			t.Fatal("the stream never detached")
		}
		time.Sleep(5 * time.Millisecond)
	}

	now := time.Now()
	e.srv.hub.reap(now) // starts the idle clock
	e.srv.hub.reap(now.Add(2 * time.Hour))
	for deadline := time.Now().Add(3 * time.Second); ; {
		if conn, _ := tb.ws.Session(); conn == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle session was not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.api("GET", "/api/v1/ws/"+id, "", 200) // released, not forgotten

	e.srv.hub.reap(now.Add(25 * time.Hour))
	e.api("GET", "/api/v1/ws/"+id, "", 404)
}

// ---------------------------------------------------------------------------
// Tabs, layout, and the error mapping
// ---------------------------------------------------------------------------

func TestTabsAndLayoutRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	e.api("PUT", "/api/v1/tabs/1", `{"title":"Query 1","conn":"demo-sqlite","buffer":"SELECT 1;"}`, 200)
	tabs := decodeData[[]Tab](t, e.api("GET", "/api/v1/tabs", "", 200))
	if len(tabs) != 1 || tabs[0].Buffer != "SELECT 1;" || tabs[0].Conn != "demo-sqlite" {
		t.Fatalf("tabs = %+v", tabs)
	}
	e.api("PUT", "/api/v1/layout", `{"editorHeight":"240"}`, 200)
	l := decodeData[map[string]string](t, e.api("GET", "/api/v1/layout", "", 200))
	if l["editorHeight"] != "240" {
		t.Fatalf("layout = %v", l)
	}
}

// A folded sidebar is rendered into the page, as the theme is, so a reload
// does not flash the column before app.js could hide it: the layout root
// carries side-off, and the edge's tooltip offers the way back.
func TestSidebarFoldRendered(t *testing.T) {
	e := newTestEnv(t)
	page := func() string {
		res := e.req("GET", "/", "", nil)
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return string(b)
	}
	if p := page(); strings.Contains(p, "side-off") || !strings.Contains(p, `id="side-split"`) ||
		!strings.Contains(p, `id="side-fold"`) || !strings.Contains(p, `id="log-split"`) ||
		!strings.Contains(p, `id="side-hsplit"`) {
		t.Fatal("unfolded page: want the sidebar's edge, fold button and both row bars, and no side-off")
	}
	e.api("PUT", "/api/v1/layout", `{"sideHidden":"1"}`, 200)
	if p := page(); !strings.Contains(p, `class="app side-off"`) || !strings.Contains(p, "Show the sidebar") {
		t.Fatal("folded page: want side-off on the layout root and the reveal tooltip")
	}
	e.api("PUT", "/api/v1/layout", `{"sideHidden":""}`, 200)
	if strings.Contains(page(), "side-off") {
		t.Fatal("shown again: side-off is still rendered")
	}
}

func TestStorePersistsAndLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.bytdb")
	st, err := OpenStore(path)
	if err != nil || !st.Persistent() {
		t.Fatalf("open: %v", err)
	}
	if err = st.SaveTab(Tab{ID: "1", Title: "Query 1", Conn: "pg", Buffer: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveTab(Tab{ID: "1", Title: "Query 1", Conn: "pg", Buffer: "SELECT 2"}); err != nil {
		t.Fatal(err) // the upsert
	}
	if err = st.SetLayout(map[string]string{"editorHeight": "300"}); err != nil {
		t.Fatal(err)
	}

	if err = st.Close(); err != nil {
		t.Fatal(err)
	}

	// Another process's open finds the file locked, and falls back to memory.
	// A second OpenStore here would not do: in one process the bytdb driver
	// shares one engine per path, so it would get the same engine, not the
	// lock. A bare bytdb.Open is a separate engine taking its own lock on the
	// sidecar — what a second dbc web's open is.
	holder, err := bytdb.Open(path)
	if err != nil {
		t.Fatalf("the stand-in for another process: %v", err)
	}
	second, err := OpenStore(path)
	if !errors.Is(err, errLocked) || second.Persistent() {
		t.Fatalf("an open of a held store: err=%v persistent=%v", err, second.Persistent())
	}
	if err = second.SaveTab(Tab{ID: "x"}); err != nil {
		t.Fatalf("the memory fallback cannot save: %v", err)
	}
	if err = holder.Close(); err != nil {
		t.Fatal(err)
	}

	// the holder gone, the file opens again — with what was saved before
	st, err = OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	tabs, err := st.Tabs()
	if err != nil || len(tabs) != 1 || tabs[0].Buffer != "SELECT 2" {
		t.Fatalf("after reopen: %+v, %v", tabs, err)
	}
	l, err := st.Layout()
	if err != nil || l["editorHeight"] != "300" {
		t.Fatalf("layout after reopen: %v, %v", l, err)
	}
}

func TestByteOffset(t *testing.T) {
	for _, c := range []struct {
		s     string
		utf16 int
		want  int
	}{
		{"abc", 0, 0},
		{"abc", 2, 2},
		{"abc", 9, 3},
		{"é;x", 1, 2}, // é: 1 unit, 2 bytes
		{"😀;x", 2, 4}, // 😀: 2 units (a surrogate pair), 4 bytes
		{"😀;x", 3, 5},
	} {
		if got := byteOffset(c.s, c.utf16); got != c.want {
			t.Errorf("byteOffset(%q, %d) = %d, want %d", c.s, c.utf16, got, c.want)
		}
	}
}

// The row counts follow the "conn" that drew the list, as a "counts" event
// carrying the list again with each table's count and its tooltip words;
// the state a reattaching page reads has them too.
//
// The counts are opt-in (the sidebar's "rows" box): a connect with them off
// draws the list with no numbers, and ticking the box is what counts.
func TestRowCountsFollowTheConnect(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	st0 := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if st0.RowCounts {
		t.Error("the rows box starts ticked")
	}
	for _, tr := range st0.Tables {
		if tr.Rows != "" {
			t.Errorf("%s has a count before the box was ticked: %+v", tr.QName, tr)
		}
	}
	e.api("POST", "/api/v1/ws/"+id+"/rowcounts", `{"on":true}`, 200)
	ev, _ := s.await(t, "counts")
	c := decodeData[countsEvent](t, testEnvelope{Data: ev.Data})
	find := func(tables []tabRef) tabRef {
		for _, tr := range tables {
			if tr.QName == "cats" {
				return tr
			}
		}
		t.Fatalf("no cats in %+v", tables)
		return tabRef{}
	}
	if c.Active != "demo-sqlite" {
		t.Errorf("counts for %q", c.Active)
	}
	if cats := find(c.Tables); cats.Rows != "8" || cats.RowsHint != "cats with 8 rows" {
		t.Errorf("cats = %+v", cats)
	}
	if !c.On {
		t.Error("the counts event should say the box is on")
	}
	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if cats := find(st.Tables); cats.Rows != "8" || !st.RowCounts {
		t.Errorf("state's cats = %+v, box %v", cats, st.RowCounts)
	}

	// unticked: a "counts" at once, with the numbers gone
	e.api("POST", "/api/v1/ws/"+id+"/rowcounts", `{"on":false}`, 200)
	ev, _ = s.await(t, "counts")
	c = decodeData[countsEvent](t, testEnvelope{Data: ev.Data})
	if cats := find(c.Tables); cats.Rows != "" || c.On {
		t.Errorf("after unticking: cats = %+v, on %v", cats, c.On)
	}
}

// A write in one query tab recounts every tab on that connection, in any
// window (N-090): the other tab's stream gets a "counts" with the new
// number, not only the writer's.
//
// The writer's own box need not be ticked: a tab whose counts are on is
// recounted by another tab's write all the same.
func TestWriteRecountsOtherTabsOnTheConnection(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	otherID, other := e.connected() // another window, on the same connection
	e.api("POST", "/api/v1/ws/"+id+"/rowcounts", `{"on":true}`, 200)
	e.api("POST", "/api/v1/ws/"+otherID+"/rowcounts", `{"on":true}`, 200)
	s.await(t, "counts")
	other.await(t, "counts")

	e.api("POST", "/api/v1/ws/"+id+"/run",
		runBody("INSERT INTO cats (name, breed, age) VALUES ('Nova', 'tabby', 2)", 0, false), 200)
	for _, st := range []*stream{s, other} {
		ev, _ := st.await(t, "counts")
		c := decodeData[countsEvent](t, testEnvelope{Data: ev.Data})
		var rows string
		for _, tr := range c.Tables {
			if tr.QName == "cats" {
				rows = tr.Rows
			}
		}
		if rows != "9" {
			t.Errorf("cats after the insert = %q, want 9", rows)
		}
	}
}

// A write in a tab whose rows box is off still recounts another tab on the
// connection whose box is on (RunDone.Wrote, not its nil Counts, decides).
func TestWriteRecountsOtherTabsWithTheWriterOff(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	otherID, other := e.connected()
	e.api("POST", "/api/v1/ws/"+otherID+"/rowcounts", `{"on":true}`, 200)
	other.await(t, "counts")

	e.api("POST", "/api/v1/ws/"+id+"/run",
		runBody("INSERT INTO cats (name, breed, age) VALUES ('Nova', 'tabby', 2)", 0, false), 200)
	s.await(t, "run")
	ev, _ := other.await(t, "counts")
	c := decodeData[countsEvent](t, testEnvelope{Data: ev.Data})
	for _, tr := range c.Tables {
		if tr.QName == "cats" && tr.Rows != "9" {
			t.Errorf("the other tab's cats after the insert = %q, want 9", tr.Rows)
		}
	}
}

// Disconnect takes a tab off its connection and keeps the connection: the
// tab's sidebar empties, its session goes (the badge with it), the list
// still has the connection, and a connect brings the tab back. While
// another tab is on the connection, its pool stays open, and the log says so.
func TestDisconnectKeepsTheConnection(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.connected() // another tab, in another window, on the same connection
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("BEGIN", 0, false), 200)
	s.await(t, "run")

	e.api("POST", "/api/v1/ws/"+id+"/disconnect", "", 200)
	ev, _ := s.await(t, "conn")
	c := decodeData[connEvent](t, testEnvelope{Data: ev.Data})
	if c.Active != "" || !c.Changed || c.Status != "disconnected" || len(c.Tables) != 0 {
		t.Fatalf("conn event = %+v", c)
	}
	// said after the "conn", so it lands in the no-connection log the page
	// shows from then on (handleDisconnect), not in the one that just left
	s.awaitLog(t, "disconnected from demo-sqlite")
	// the session's release, then the pool's fate, follow off the request
	_, logs := s.awaitLog(t, "1 query tab is still on demo-sqlite: its connection stays open for it")
	if !slices.ContainsFunc(logs, func(l string) bool { return strings.HasPrefix(l, "left demo-sqlite") }) {
		t.Errorf("the open transaction's rollback went unsaid: %q", logs)
	}

	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if st.Active != "" || st.Connected || st.Stateful {
		t.Errorf("state after = %+v", st)
	}
	list := decodeData[struct {
		Conns []connInfo `json:"conns"`
	}](t, e.api("GET", "/api/v1/conns", "", 200))
	if !slices.ContainsFunc(list.Conns, func(c connInfo) bool { return c.Name == "demo-sqlite" }) {
		t.Errorf("the connection left the list: %+v", list.Conns)
	}
	env := e.api("POST", "/api/v1/ws/"+id+"/disconnect", "", 400)
	if !strings.Contains(env.Error, "not connected") {
		t.Errorf("a second disconnect: %+v", env)
	}

	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"demo-sqlite"}`, 200)
	ev, _ = s.await(t, "conn")
	if c := decodeData[connEvent](t, testEnvelope{Data: ev.Data}); c.Active != "demo-sqlite" || len(c.Tables) == 0 {
		t.Fatalf("reconnect = %+v", c)
	}
}
