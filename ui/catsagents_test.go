package ui

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/dbc/cats"
)

// ctlServer is a stand-in for cats' control socket: one JSON request per
// connection, one reply, close.
type ctlServer struct {
	path string

	mu    sync.Mutex
	reqs  []map[string]any
	panes []cats.PaneInfo
}

func newCtlServer(t *testing.T, panes []cats.PaneInfo) *ctlServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "d") // short: sun_path is 104 bytes
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	s := &ctlServer{path: filepath.Join(dir, "c.sock"), panes: panes}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var req map[string]any
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					return
				}
				s.mu.Lock()
				s.reqs = append(s.reqs, req)
				panes := s.panes
				s.mu.Unlock()

				var data any
				switch req["method"] {
				case cats.MethodPing:
					data = cats.Pong{Protocol: 1, Service: "catway"}
				case cats.MethodPaneList:
					data = cats.PaneListResult{Panes: panes}
				}
				raw, _ := json.Marshal(data)
				reply := map[string]any{"ok": true}
				if data != nil {
					reply["data"] = json.RawMessage(raw)
				}
				line, _ := json.Marshal(reply)
				_, _ = conn.Write(append(line, '\n'))
			}()
		}
	}()
	return s
}

// requestsFor returns the params of every request for a method.
func (s *ctlServer) requestsFor(method string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, r := range s.reqs {
		if r["method"] != method {
			continue
		}
		p, _ := r["params"].(map[string]any)
		out = append(out, p)
	}
	return out
}

func (s *ctlServer) waitFor(t *testing.T, method string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.requestsFor(method); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s requests", n, method)
	return nil
}

// newTier1App builds an app already at Tier 1 against a fake control socket,
// with the given panes cached. The probe is bypassed: this exercises the
// picker, not the detection that Phase 1 already covers.
func newTier1App(t *testing.T, panes []cats.PaneInfo) (*App, *ctlServer) {
	t.Helper()
	srv := newCtlServer(t, panes)
	a := newTestApp(t)
	onUI(t, a, func() bool {
		a.cats.caps = cats.Caps{
			InCats: true, Control: true,
			PaneHandle: "w1:p1", ControlSocket: srv.path,
		}
		a.cats.client = cats.NewClient(srv.path)
		a.cats.stopping = make(chan struct{})
		a.cats.self, a.cats.selfOK = 1, true
		a.cats.panes = panes
		return true
	})
	return a, srv
}

func agentPanes() []cats.PaneInfo {
	return []cats.PaneInfo{
		{Pane: 1, Handle: "w1:p1", Agent: "dbc", AgentState: cats.StateIdle},    // us
		{Pane: 2, Handle: "w1:p2", Agent: "claude", AgentState: cats.StateIdle}, //
		{Pane: 3, Handle: "w1:p3"}, // a plain shell
		{Pane: 4, Handle: "w1:p4", Agent: "codex", AgentState: cats.StateBlocked}, //
	}
}

// dbc reports itself to cats as the agent "dbc", so without the self check it
// would offer to send the query to itself.
func TestAgentPanesExcludeUsAndPlainShells(t *testing.T) {
	a, _ := newTier1App(t, agentPanes())

	got := onUI(t, a, func() []cats.PaneInfo { return a.catsAgentPanes() })
	if len(got) != 2 {
		t.Fatalf("expected 2 agent panes, got %d: %+v", len(got), got)
	}
	// Blocked first: it is the one worth interrupting.
	if got[0].Agent != "codex" || got[1].Agent != "claude" {
		t.Errorf("order should rank blocked above idle, got %s then %s",
			got[0].Agent, got[1].Agent)
	}
	for _, p := range got {
		if p.Agent == "dbc" {
			t.Error("dbc offered to send the query to itself")
		}
		if p.Agent == "" {
			t.Error("a plain shell was offered a fenced SQL block")
		}
	}
}

func TestAgentRankOrdersByAttention(t *testing.T) {
	if catsAgentRank(cats.StateBlocked) <= catsAgentRank(cats.StateWorking) ||
		catsAgentRank(cats.StateWorking) <= catsAgentRank(cats.StateIdle) ||
		catsAgentRank(cats.StateIdle) <= catsAgentRank("") {
		t.Error("rank should be blocked > working > idle > unknown")
	}
}

// The question carries what an agent cannot infer: which database, and what
// went wrong if anything did.
func TestQuestionCarriesTheDialectAndTheError(t *testing.T) {
	a, _ := newTier1App(t, agentPanes())
	setBuffer(t, a, "SELECT 1;\nSELECT 2;", 0)

	q := onUI(t, a, func() string { return a.catsQuestion() })
	if !strings.Contains(q, "sqlite") {
		t.Errorf("the driver should be named, got %q", q)
	}
	if !strings.Contains(q, "```sql") || !strings.Contains(q, "SELECT 1") {
		t.Errorf("the statement should be fenced, got %q", q)
	}
	if strings.Contains(q, "SELECT 2") {
		t.Errorf("only the statement under the cursor should be sent, got %q", q)
	}
	if strings.Contains(q, "It failed") {
		t.Errorf("a query that has not failed should carry no error, got %q", q)
	}

	onUI(t, a, func() bool { a.lastRunErr = "no such table: cats"; return true })
	q = onUI(t, a, func() string { return a.catsQuestion() })
	if !strings.Contains(q, "no such table: cats") {
		t.Errorf("the failure should ride along, got %q", q)
	}
}

// A selection wins over the statement under the cursor — the same rule Ctrl+R
// runs by, so what is asked about is what would be executed.
func TestQuestionPrefersTheSelection(t *testing.T) {
	a, _ := newTier1App(t, agentPanes())
	onUI(t, a, func() bool {
		a.editor.SetText("SELECT 1;\nSELECT 2;", false)
		a.editor.Select(10, 19)
		return true
	})

	q := onUI(t, a, func() string { return a.catsQuestion() })
	if !strings.Contains(q, "SELECT 2") || strings.Contains(q, "SELECT 1") {
		t.Errorf("the selection should be what is sent, got %q", q)
	}
}

// The flagship: the question reaches the agent's pane STAGED, and the user is
// taken there to press Enter themselves.
func TestSendToPaneStagesAndFocuses(t *testing.T) {
	a, srv := newTier1App(t, agentPanes())
	setBuffer(t, a, "SELECT count(*) FROM cats", 0)

	onUI(t, a, func() bool {
		a.catsSendToPane(cats.PaneInfo{Pane: 2, Agent: "claude"}, a.catsQuestion())
		return true
	})

	sent := srv.waitFor(t, cats.MethodPaneSendIn, 1)[0]
	if sent["pane"].(float64) != 2 {
		t.Errorf("sent to pane %v", sent["pane"])
	}
	if !strings.Contains(sent["text"].(string), "SELECT count(*) FROM cats") {
		t.Errorf("the query did not arrive: %v", sent["text"])
	}
	// submit is omitempty: staged means the key is absent entirely.
	if _, ok := sent["submit"]; ok {
		t.Errorf("dbc must never press Enter in another pane, got submit=%v", sent["submit"])
	}
	srv.waitFor(t, cats.MethodPaneFocus, 1)
}

func TestSendToChatSubmits(t *testing.T) {
	a, srv := newTier1App(t, agentPanes())

	onUI(t, a, func() bool { a.catsSendToChat("why is this slow?"); return true })

	sent := srv.waitFor(t, cats.MethodChatSend, 1)[0]
	if sent["text"] != "why is this slow?" {
		t.Errorf("chat got %v", sent["text"])
	}
}

// The picker lists the agents plus cats chat, and nothing else.
func TestAgentModalRows(t *testing.T) {
	a, _ := newTier1App(t, agentPanes())
	setBuffer(t, a, "SELECT 1", 0)

	press(a, tcell.KeyCtrlG)
	drain(t, a)

	front := onUI(t, a, func() string { n, _ := a.pages.GetFrontPage(); return n })
	if front != "agents" {
		t.Fatalf("Ctrl+G did not open the picker, front page is %q", front)
	}

	press(a, tcell.KeyEsc)
	drain(t, a)
	if front := onUI(t, a, func() string { n, _ := a.pages.GetFrontPage(); return n }); front != "main" {
		t.Errorf("Esc should close the picker, front page is %q", front)
	}
}

// Outside cats the feature is honestly absent rather than broken: it says so
// and changes nothing.
func TestAgentModalRefusesAtTier0(t *testing.T) {
	a := newTestApp(t)
	setBuffer(t, a, "SELECT 1", 0)

	press(a, tcell.KeyCtrlG)
	drain(t, a)

	front := onUI(t, a, func() string { n, _ := a.pages.GetFrontPage(); return n })
	if front != "main" {
		t.Errorf("no picker should open outside cats, front page is %q", front)
	}
	log := onUI(t, a, func() string { return a.logView.GetText(true) })
	if !strings.Contains(log, "standalone") {
		t.Errorf("the refusal should explain itself, log is %q", log)
	}
}

// With nothing under the cursor there is nothing to ask about.
func TestAgentModalRefusesAnEmptyStatement(t *testing.T) {
	a, _ := newTier1App(t, agentPanes())
	setBuffer(t, a, "   \n  ", 0)

	press(a, tcell.KeyCtrlG)
	drain(t, a)

	if front := onUI(t, a, func() string { n, _ := a.pages.GetFrontPage(); return n }); front != "main" {
		t.Errorf("an empty statement should not open the picker, front page is %q", front)
	}
}

// A keystroke must never dial a socket, so the list it shows is the cached
// one and the refresh it triggers is for the next open.
func TestPollPanesIsRateLimited(t *testing.T) {
	a, srv := newTier1App(t, agentPanes())

	onUI(t, a, func() bool {
		a.cats.panesAt = time.Time{} // never polled
		a.catsPollPanes(false)
		for range 5 {
			a.catsPollPanes(false) // all inside the rate-limit window
		}
		return true
	})
	srv.waitFor(t, cats.MethodPaneList, 1)

	time.Sleep(100 * time.Millisecond)
	if got := len(srv.requestsFor(cats.MethodPaneList)); got != 1 {
		t.Errorf("expected the rate limit to collapse the burst to 1 call, got %d", got)
	}
}
