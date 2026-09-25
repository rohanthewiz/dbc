package web

import (
	"io"
	"strings"
	"testing"
	"time"
)

// Query tabs: several workspaces in one window, sharing its stream (and
// its assistant) but nothing else — each has its own run slot, session and
// result.

// openIn opens another query tab in window win and connects it; its
// events arrive on the window's stream s, tagged with its id.
func (e *testEnv) openIn(win string, s *stream) string {
	e.t.Helper()
	st := decodeData[wsState](e.t, e.api("POST", "/api/v1/ws", `{"win":"`+win+`"}`, 200))
	if st.Win != win {
		e.t.Fatalf("opened in window %q, want %q", st.Win, win)
	}
	e.api("POST", "/api/v1/ws/"+st.ID+"/connect", `{"name":"demo-sqlite"}`, 200)
	s.awaitFrom(e.t, st.ID, "conn")
	return st.ID
}

// awaitFrom reads events until one of type typ from query tab ws arrives.
func (s *stream) awaitFrom(t *testing.T, ws, typ string) sseEvent {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatalf("stream closed waiting for %q from %s", typ, ws)
			}
			if ev.Type == typ && ev.WS == ws {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %q event from %s", typ, ws)
		}
	}
}

// Two query tabs in one window run at once — a slow statement in one does
// not make the other busy — and each event names the tab it belongs to.
// A BEGIN in one leaves the other's session plain.
func TestQueryTabsRunApartOnOneStream(t *testing.T) {
	e := newTestEnv(t)
	a, s := e.connected()
	win := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+a, "", 200)).Win
	b := e.openIn(win, s)

	slow := "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 3000000) SELECT count(*) FROM n"
	e.api("POST", "/api/v1/ws/"+a+"/run", runBody(slow, 0, false), 200)
	s.awaitFrom(t, a, "busy")
	e.api("POST", "/api/v1/ws/"+b+"/run", runBody("BEGIN", 0, false), 200) // not 409: its own slot
	ev := s.awaitFrom(t, b, "run")
	if run := decodeData[runEvent](t, testEnvelope{Data: ev.Data}); !run.OK || !run.Stateful {
		t.Errorf("tab b's BEGIN = %+v", run)
	}
	e.api("POST", "/api/v1/ws/"+a+"/cancel", "", 200)
	ev = s.awaitFrom(t, a, "run")
	if run := decodeData[runEvent](t, testEnvelope{Data: ev.Data}); run.Stateful {
		t.Errorf("tab b's transaction leaked into tab a: %+v", run)
	}

	w := decodeData[winState](t, e.api("GET", "/api/v1/win/"+win, "", 200))
	if len(w.Tabs) != 2 {
		t.Errorf("window lists %v, want both tabs", w.Tabs)
	}
	e.api("POST", "/api/v1/ws", `{"win":"nope"}`, 404)
}

// Closing a query tab releases its session — rolling back what it held —
// and forgets it; the window and its other tabs carry on.
func TestCloseQueryTabReleasesItsSession(t *testing.T) {
	e := newTestEnv(t)
	a, s := e.connected()
	win := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+a, "", 200)).Win
	b := e.openIn(win, s)
	e.api("POST", "/api/v1/ws/"+b+"/run", runBody("BEGIN", 0, false), 200)
	s.awaitFrom(t, b, "run")
	tb, _ := e.srv.hub.get(b)

	e.api("DELETE", "/api/v1/ws/"+b, "", 200)
	e.api("GET", "/api/v1/ws/"+b, "", 404)
	for deadline := time.Now().Add(3 * time.Second); ; {
		if conn, _ := tb.ws.Session(); conn == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the closed tab's session was not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.api("POST", "/api/v1/ws/"+a+"/run", runBody("SELECT 1", 0, false), 200)
	s.awaitFrom(t, a, "run")
	if w := decodeData[winState](t, e.api("GET", "/api/v1/win/"+win, "", 200)); len(w.Tabs) != 1 || w.Tabs[0] != a {
		t.Errorf("window after close = %+v", w)
	}
	e.api("DELETE", "/api/v1/ws/"+b, "", 404)
}

// An idle window — no stream — releases every one of its query tabs'
// sessions, and is later forgotten with all of them.
func TestIdleWindowReleasesEveryTab(t *testing.T) {
	e := newTestEnv(t)
	a, s := e.connected()
	win := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+a, "", 200)).Win
	b := e.openIn(win, s)
	for _, id := range []string{a, b} {
		e.api("POST", "/api/v1/ws/"+id+"/run", runBody("BEGIN", 0, false), 200)
		s.awaitFrom(t, id, "run")
	}
	_ = s.body.Close()
	w, _ := e.srv.hub.window(win)
	for deadline := time.Now().Add(3 * time.Second); w.sse.ClientCount() > 0; {
		if time.Now().After(deadline) {
			t.Fatal("the stream never detached")
		}
		time.Sleep(5 * time.Millisecond)
	}
	now := time.Now()
	e.srv.hub.reap(now)
	e.srv.hub.reap(now.Add(2 * time.Hour))
	for _, id := range []string{a, b} {
		tb, _ := e.srv.hub.get(id)
		for deadline := time.Now().Add(3 * time.Second); ; {
			if conn, _ := tb.ws.Session(); conn == "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("tab %s's idle session was not released", id)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	e.srv.hub.reap(now.Add(25 * time.Hour))
	e.api("GET", "/api/v1/ws/"+b, "", 404)
	e.api("GET", "/api/v1/win/"+win, "", 404)
}

// A saved tab can be forgotten.
func TestDeleteSavedTab(t *testing.T) {
	e := newTestEnv(t)
	e.api("PUT", "/api/v1/tabs/k2", `{"title":"Query 2","conn":"demo-sqlite","buffer":"SELECT 2"}`, 200)
	e.api("DELETE", "/api/v1/tabs/k2", "", 200)
	tabs := decodeData[[]Tab](t, e.api("GET", "/api/v1/tabs", "", 200))
	for _, tb := range tabs {
		if tb.ID == "k2" {
			t.Fatalf("deleted tab still saved: %+v", tabs)
		}
	}
}

// The assistant belongs to the window and asks about the query tab the
// request names: its statement, its connection.
func TestChatAsksAboutTheNamedQueryTab(t *testing.T) {
	e := newTestEnv(t)
	a, s := e.connected()
	win := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+a, "", 200)).Win
	b := e.openIn(win, s)
	e.runQuery(b, s, "SELECT breed FROM cats")
	ctxOf := func(id string) string {
		body := `{"editor":{"buffer":""}}`
		return decodeData[map[string]string](t, e.api("POST", "/api/v1/ws/"+id+"/chat/context", body, 200))["note"]
	}
	if got := ctxOf(b); !strings.Contains(got, "query") || !strings.Contains(got, "column names") {
		t.Errorf("tab b's context = %q", got)
	}
	if got := ctxOf(a); strings.Contains(got, "column names") {
		t.Errorf("tab a's context carries tab b's result: %q", got)
	}
	ta, _ := e.srv.hub.get(a)
	tb, _ := e.srv.hub.get(b)
	if ta.win.chat != tb.win.chat {
		t.Error("two query tabs of one window have two assistants")
	}
}

// A page path nothing serves gets a page saying so, with the way back; an
// API path nothing serves gets the envelope. Neither is an empty 404.
func TestErrorPages(t *testing.T) {
	e := newTestEnv(t)
	res := e.req("GET", "/no/such/page", "", nil)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 404 || !strings.Contains(string(b), "back to the workbench") ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Errorf("page 404 = %d %q: %s", res.StatusCode, res.Header.Get("Content-Type"), b)
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("the error page has no CSP")
	}
	env := e.api("GET", "/api/v1/nope", "", 404)
	if !strings.Contains(env.Error, "no such endpoint") {
		t.Errorf("api 404 = %+v", env)
	}
	// a handler's own 404 keeps its words
	env = e.api("GET", "/api/v1/ws/zzz", "", 404)
	if !strings.Contains(env.Error, "no workspace") {
		t.Errorf("workspace 404 = %+v", env)
	}
	// without a session, the sign-in notice, not the 404 page
	res = e.req("GET", "/no/such/page", "", map[string]string{})
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Errorf("unauthenticated 404 = %d, want 401", res.StatusCode)
	}
}

// Light and dark: /theme.css carries both palettes (and both scoped to the
// plan view, whose own toggle must work in either), and the page renders
// the saved choice on its root so a reload does not flash the other.
func TestThemeLightAndDark(t *testing.T) {
	e := newTestEnv(t)
	res := e.req("GET", "/theme.css", "", nil)
	css, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, want := range []string{":root{--bg:", `:root[data-theme="light"]{--bg:#f6f8f6`,
		`.dbc-plan[data-theme="dark"]{`, `.dbc-plan[data-theme="light"]{`, "color-scheme:light"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("theme.css lacks %q:\n%s", want, css)
		}
	}
	page := func() string {
		res := e.req("GET", "/", "", nil)
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	if !strings.Contains(page(), `data-theme="dark"`) {
		t.Error("the default page is not dark")
	}
	e.api("PUT", "/api/v1/layout", `{"theme":"light"}`, 200)
	if !strings.Contains(page(), `<html lang="en" data-theme="light"`) {
		t.Error("the saved light theme is not rendered into the page")
	}
}
