package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/ai/aitest"
	"github.com/rohanthewiz/dbc/config"
)

// These pin down the web half of per-connection result sets
// (resulttabs.go): the strip on the wire, the result-tab endpoint, stable
// seqs, the results and plan following the tab's connection, log lines
// naming their connection, renames and removals, and sharing a result tab
// with the assistant.

// withOther adds an empty in-memory SQLite connection named "other".
func withOther(c *config.Config, _ *Options) {
	c.Connections = append(c.Connections, config.Connection{Name: "other", Driver: "sqlite",
		DSN: fmt.Sprintf("file:webother%d?mode=memory&cache=shared", dbSeq.Add(1))})
}

// switchConn connects tab id to name and returns the "conn" event.
func (e *testEnv) switchConn(id string, s *stream, name string) connEvent {
	e.t.Helper()
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"`+name+`"}`, 200)
	for {
		ev := s.awaitFrom(e.t, id, "conn")
		c := decodeData[connEvent](e.t, testEnvelope{Data: ev.Data})
		if c.Active == name {
			return c
		}
	}
}

// runEv runs sql in tab id and returns its "run" event.
func (e *testEnv) runEv(id string, s *stream, sql string) runEvent {
	e.t.Helper()
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody(sql, 0, false), 200)
	return decodeData[runEvent](e.t, testEnvelope{Data: s.awaitFrom(e.t, id, "run").Data})
}

// tabOp posts a result-tab op and decodes the answer.
func (e *testEnv) tabOp(id string, rt int, op string, want int) (resultTabOut, testEnvelope) {
	e.t.Helper()
	env := e.api("POST", "/api/v1/ws/"+id+"/result-tab", fmt.Sprintf(`{"id":%d,"op":%q}`, rt, op), want)
	if want != 200 {
		return resultTabOut{}, env
	}
	return decodeData[resultTabOut](e.t, env), env
}

// seqNow is the seq of the result the grid would fetch now (0 for none).
func (e *testEnv) seqNow(id string) int {
	e.t.Helper()
	env := e.api("GET", "/api/v1/ws/"+id+"/result?n=1", "", 200)
	if d := string(env.Data); d == "" || d == "null" {
		return 0
	}
	return decodeData[resultPage](e.t, env).Seq
}

// The strip rides on the run event and the state; a pinned tab makes the
// next run open a new one; show, close and close-unpinned move the grid;
// and a result keeps its seq while it is the same result, so the page's
// per-seq view comes back.
func TestResultTabsOnTheWire(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()

	run := e.runEv(id, s, "SELECT 1 AS a")
	if len(run.ResultTabs) != 1 || run.ResultTab != run.ResultTabs[0].ID || run.ResultTabMax != config.DefaultResultTabs {
		t.Fatalf("run = %+v", run.resultTabsState)
	}
	first := run.ResultTabs[0]
	if first.Title != "SELECT 1 AS a" || first.Rows != 1 || first.Pinned || first.Seq != e.seqNow(id) {
		t.Errorf("tab = %+v, page seq %d", first, e.seqNow(id))
	}

	out, _ := e.tabOp(id, first.ID, "pin", 200)
	if !out.ResultTabs[0].Pinned || !out.HasResult {
		t.Errorf("pin = %+v", out)
	}
	run = e.runEv(id, s, "SELECT 2 AS a, 3 AS b")
	if len(run.ResultTabs) != 2 || run.ResultTab == first.ID {
		t.Fatalf("after a pin, the run did not open a tab: %+v", run.resultTabsState)
	}
	second := run.ResultTabs[1]

	out, _ = e.tabOp(id, first.ID, "show", 200)
	if out.ResultTab != first.ID || !strings.Contains(out.Status, "1 rows") || e.seqNow(id) != first.Seq {
		t.Errorf("show 1 = %+v, seq %d want %d", out, e.seqNow(id), first.Seq)
	}
	e.tabOp(id, second.ID, "show", 200)
	e.tabOp(id, first.ID, "show", 200)
	if got := e.seqNow(id); got != first.Seq {
		t.Errorf("seq moved on a revisit: %d, want %d", got, first.Seq)
	}
	if st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200)); st.ResultTab != first.ID || len(st.ResultTabs) != 2 {
		t.Errorf("state = %+v", st.resultTabsState)
	}

	// a copy against the other tab's seq is a stale view: refused
	b, _ := json.Marshal(viewReq{Seq: second.Seq, Sort: -1, Cols: []int{0}})
	e.api("POST", "/api/v1/ws/"+id+"/copy", string(b), 409)

	out, _ = e.tabOp(id, first.ID, "close", 200)
	if len(out.ResultTabs) != 1 || out.ResultTab != second.ID || e.seqNow(id) != second.Seq {
		t.Errorf("close = %+v", out)
	}
	_, env := e.tabOp(id, first.ID, "show", 400)
	if !strings.Contains(env.Error, "no result tab") {
		t.Errorf("show a closed tab: %q", env.Error)
	}
	e.tabOp(id, second.ID, "bogus", 400)
	out, _ = e.tabOp(id, 0, "close-unpinned", 200)
	if len(out.ResultTabs) != 0 || out.ResultTab != 0 || out.HasResult || out.Status != "" {
		t.Errorf("close-unpinned = %+v", out)
	}
	if e.seqNow(id) != 0 {
		t.Error("the grid still has a result with every tab closed")
	}
}

// Switching the tab's connection swaps the results pane: the "conn" event
// carries the new connection's set (empty, the first time), the grid's
// result and the plan follow, and switching back brings the old ones back
// with the same seqs.
func TestResultSetFollowsConnection(t *testing.T) {
	e := newTestEnv(t, withOther)
	id, s := e.connected()
	run := e.runEv(id, s, "SELECT name FROM cats")
	demoSeq := e.seqNow(id)
	ex, _ := e.explainAndWait(id, s, explainBody("SELECT name FROM cats", false, false))
	if !ex.OK {
		t.Fatalf("explain = %+v", ex)
	}

	c := e.switchConn(id, s, "other")
	if !c.Changed || len(c.ResultTabs) != 0 || c.HasResult || c.HasPlan || c.ResultTab != 0 {
		t.Fatalf("conn to other = %+v", c)
	}
	if e.seqNow(id) != 0 {
		t.Error("other shows demo's result")
	}
	if env := e.api("GET", "/api/v1/ws/"+id+"/plan", "", 200); len(env.Data) > 0 && string(env.Data) != "null" {
		t.Errorf("other shows demo's plan: %s", env.Data)
	}
	e.runEv(id, s, "SELECT 7 AS n")

	c = e.switchConn(id, s, "demo-sqlite")
	if !c.HasResult || !c.HasPlan || len(c.ResultTabs) != 1 || c.ResultTab != run.ResultTab {
		t.Fatalf("back on demo = %+v", c)
	}
	if got := e.seqNow(id); got != demoSeq {
		t.Errorf("demo's result came back as seq %d, want %d", got, demoSeq)
	}
	if env := e.api("GET", "/api/v1/ws/"+id+"/plan", "", 200); len(env.Data) == 0 || string(env.Data) == "null" {
		t.Error("demo's plan did not come back")
	}
}

// A line names the connection it belongs to: a run's lines its own, a
// landed connect's the new one; the connect's "connecting to …" and a
// failed connect's words name none, so they land in the log on screen.
func TestLogLinesNameTheirConnection(t *testing.T) {
	e := newTestEnv(t, withOther)
	id, s := e.connected()
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 1", 0, false), 200)
	lines := linesUntil(t, s, "run")
	for _, l := range lines {
		if l.Conn != "demo-sqlite" {
			t.Errorf("run line %q names %q", l.Text, l.Conn)
		}
	}
	if len(lines) == 0 {
		t.Fatal("no run lines")
	}

	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"other"}`, 200)
	lines = linesUntil(t, s, "conn")
	var connecting, connected *logLine
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l.Text, "connecting to other"):
			connecting = &lines[i]
		case l.Text == "connected to other":
			connected = &lines[i]
		}
	}
	if connecting == nil || connecting.Conn != "" || connected == nil || connected.Conn != "other" {
		t.Errorf("connect lines = %+v", lines)
	}
}

// linesUntil reads log lines until an event of type typ.
func linesUntil(t *testing.T, s *stream, typ string) []logLine {
	t.Helper()
	var out []logLine
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-s.events:
			if ev.Type == "log" {
				var l logLine
				_ = json.Unmarshal(ev.Data, &l)
				out = append(out, l)
			}
			if ev.Type == typ {
				return out
			}
		case <-timeout:
			t.Fatalf("no %q; lines %+v", typ, out)
		}
	}
}

// A connection renamed in the browser takes the results tabs kept for it
// along; a removed one's are dropped, so a new connection of that name
// starts with none.
func TestRenameAndRemoveKeepResultsInStep(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.api("POST", "/api/v1/conns", connBody("scratch", "sqlite", "file:rtscratch?mode=memory&cache=shared"), 200)
	e.switchConn(id, s, "scratch")
	e.runEv(id, s, "SELECT 1")
	ex, _ := e.explainAndWait(id, s, explainBody("SELECT 1", false, false))
	if !ex.OK {
		t.Fatal("explain failed")
	}
	e.switchConn(id, s, "demo-sqlite")

	e.api("PUT", "/api/v1/conns/scratch", editBody("renamed", "sqlite", "", false), 200)
	c := e.switchConn(id, s, "renamed")
	if !c.HasResult || len(c.ResultTabs) != 1 || !c.HasPlan {
		t.Fatalf("after the rename = %+v", c)
	}

	e.switchConn(id, s, "demo-sqlite")
	e.api("DELETE", "/api/v1/conns/renamed", "", 200)
	e.api("POST", "/api/v1/conns", connBody("renamed", "sqlite", "file:rtscratch?mode=memory&cache=shared"), 200)
	if c = e.switchConn(id, s, "renamed"); c.HasResult || len(c.ResultTabs) != 0 || c.HasPlan {
		t.Errorf("a removed connection's results came back: %+v", c)
	}
}

// Sharing a result tab with the assistant: refused without ai_rows (a 400
// that says so, logged), allowed with it — the strip marks it, the log
// says it — and the shared tab's result goes with a question asked while
// another tab is on screen, without the columns the page kept hidden in it.
func TestShareResultTab(t *testing.T) {
	// without ai_rows
	e := newTestEnv(t)
	id, s := e.connected()
	run := e.runEv(id, s, "SELECT 1")
	if run.CanShare {
		t.Error("canShare without ai_rows")
	}
	_, env := e.tabOp(id, run.ResultTab, "share", 400)
	if !strings.Contains(env.Error, "ai_rows = true") {
		t.Errorf("refusal = %q", env.Error)
	}

	// with ai_rows
	f := &aitest.Fake{}
	e, _ = chatEnv(t, f, func(c *config.Config, _ *Options) { c.Connections[0].AIRows = true })
	id, s = e.connected()
	run = e.runEv(id, s, "SELECT id, name, breed FROM cats ORDER BY id")
	shared := run.ResultTabs[0]
	e.api("POST", "/api/v1/ws/"+id+"/result-tab", fmt.Sprintf(`{"id":%d,"op":"share"}`, shared.ID), 200)
	if _, logs := s.await(t, "log"); len(logs) == 0 || !strings.Contains(logs[0], "shared result 1 with the assistant") {
		t.Errorf("share logs = %q", logs)
	}
	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if !st.CanShare || !st.ResultTabs[0].Shared {
		t.Fatalf("state = %+v", st.resultTabsState)
	}
	// a shared tab is kept: the next run opens a tab of its own
	run = e.runEv(id, s, "SELECT 42 AS answer")
	if len(run.ResultTabs) != 2 || run.ResultTab == shared.ID {
		t.Fatalf("the run replaced the shared tab: %+v", run.resultTabsState)
	}
	cur := run.ResultTabs[1]

	// ask with tab 2 on screen, the shared tab's view hiding name (column 1)
	b, _ := json.Marshal(chatReq{Question: "who?", Attach: true, Editor: runReq{Buffer: "SELECT 42 AS answer"},
		View:   &chatGrid{Seq: cur.Seq, Sort: -1},
		Shared: &chatGrid{Seq: shared.Seq, Sort: -1, Hidden: []int{1}}})
	e.api("POST", "/api/v1/ws/"+id+"/chat/ask", string(b), 200)
	s.await(t, "chat.msg")
	awaitChat(t, s, chatReadyView)
	p := f.Prompts()[0]
	if !strings.Contains(p, "Tabby") && !strings.Contains(p, "Siamese") {
		t.Errorf("the shared result's rows did not go:\n%s", p)
	}
	if strings.Contains(p, "Milo") || strings.Contains(p, "Whiskers") {
		t.Errorf("a column hidden in the shared tab went:\n%s", p)
	}

	// a stale shared view that hides columns is refused, as the grid's is
	b, _ = json.Marshal(chatReq{Question: "q", Attach: true, Editor: runReq{Buffer: "SELECT 1"},
		Shared: &chatGrid{Seq: 9999, Sort: -1, Hidden: []int{1}}})
	e.api("POST", "/api/v1/ws/"+id+"/chat/context", string(b), 409)

	e.api("POST", "/api/v1/ws/"+id+"/result-tab", fmt.Sprintf(`{"id":%d,"op":"unshare"}`, shared.ID), 200)
	if _, logs := s.await(t, "log"); !strings.Contains(strings.Join(logs, "\n"), "stopped sharing result 1") {
		t.Errorf("unshare logs = %q", logs)
	}
}
