package ui

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/dbc/cats"
)

// hookServer is a stand-in for cats' hook socket: it reads one JSON request
// per connection and never replies, which is exactly what the real one looks
// like to a fire-and-forget reporter.
type hookServer struct {
	path string

	mu   sync.Mutex
	reqs []map[string]any
}

// newHookServer listens on a SHORT path. t.TempDir() embeds the test name and
// would overrun sun_path (104 bytes on macOS) with a "bind: invalid argument"
// that reads like a permissions failure.
func newHookServer(t *testing.T) *hookServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "d")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	s := &hookServer{path: filepath.Join(dir, "h.sock")}
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
				s.mu.Unlock()
			}()
		}
	}()
	return s
}

// states returns the (state, custom_status) pairs reported so far, in arrival
// order.
func (s *hookServer) states() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][2]string, 0, len(s.reqs))
	for _, r := range s.reqs {
		p, ok := r["params"].(map[string]any)
		if !ok {
			continue
		}
		state, _ := p["state"].(string)
		status, _ := p["custom_status"].(string)
		out = append(out, [2]string{state, status})
	}
	return out
}

func (s *hookServer) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.reqs))
	for _, r := range s.reqs {
		m, _ := r["method"].(string)
		out = append(out, m)
	}
	return out
}

// waitForState polls until a (state, status) pair has been reported.
func (s *hookServer) waitForState(t *testing.T, state, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, got := range s.states() {
			if got[0] == state && got[1] == status {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for state %q/%q; got %v", state, status, s.states())
}

// inCatsPane points the environment at a fake hook socket, with no control
// socket — the Tier-0-with-hooks case, which is all the reporter needs and
// which keeps the test off a control-socket implementation.
func inCatsPane(t *testing.T, hook string) {
	t.Helper()
	t.Setenv(cats.EnvMarker, "1")
	t.Setenv(cats.EnvPaneID, "w1:p3")
	t.Setenv(cats.EnvControlSocket, "")
	t.Setenv(cats.EnvHookSocket, hook)
}

// newCatsApp is the standard harness plus a live cats integration.
func newCatsApp(t *testing.T) (*App, *hookServer) {
	t.Helper()
	srv := newHookServer(t)
	inCatsPane(t, srv.path)

	a := newTestApp(t)
	onUI(t, a, func() bool { a.catsInit(); return true })
	return a, srv
}

// Claiming the pane at startup is what puts "dbc" in cats' sidebar, and it
// establishes the state the first transition is measured against.
func TestCatsClaimsThePaneAtStartup(t *testing.T) {
	_, srv := newCatsApp(t)
	srv.waitForState(t, cats.StateIdle, "")
}

// The flagship path: a run reports working with its tag as the host-visible
// status, and the end of the run reports idle — the edge cats turns into a
// "finished" notification.
func TestCatsReportsRunTransitions(t *testing.T) {
	a, srv := newCatsApp(t)
	srv.waitForState(t, cats.StateIdle, "")

	setBuffer(t, a, slowQuery, 0)
	press(a, tcell.KeyCtrlR)
	srv.waitForState(t, cats.StateWorking, "query")

	press(a, tcell.KeyCtrlK)
	waitFor(t, a, "the run to end", func() bool { return !a.busy.Load() })
	srv.waitForState(t, cats.StateIdle, "")
}

// Every report is a potential toast or phone push, so steady state must be
// silent: only transitions go out.
func TestCatsReportsOnlyOnChange(t *testing.T) {
	a, srv := newCatsApp(t)
	srv.waitForState(t, cats.StateIdle, "")

	before := len(srv.states())
	onUI(t, a, func() bool {
		for range 5 {
			a.catsAfterTransition()
		}
		return true
	})
	// Give any report that was going to be sent time to arrive.
	time.Sleep(200 * time.Millisecond)
	if after := len(srv.states()); after != before {
		t.Errorf("idle was re-reported %d times; state reports must be transitions only",
			after-before)
	}
}

// blocked outranks working: a question the user did not ask for is the thing
// worth paging them about, even mid-run.
func TestCatsBlockedOutranksWorking(t *testing.T) {
	a, _ := newCatsApp(t)

	got := onUI(t, a, func() [2]string {
		a.busy.Store(true)
		a.runMu.Lock()
		a.runTag = "query"
		a.runMu.Unlock()
		s, st := a.catsSelfState()
		return [2]string{s, st}
	})
	if got[0] != cats.StateWorking || got[1] != "query" {
		t.Fatalf("busy should read as working/query, got %q/%q", got[0], got[1])
	}

	got = onUI(t, a, func() [2]string {
		a.cats.asking = "connection lost"
		s, st := a.catsSelfState()
		return [2]string{s, st}
	})
	if got[0] != cats.StateBlocked || got[1] != "connection lost" {
		t.Errorf("an unbidden question should outrank a run, got %q/%q", got[0], got[1])
	}

	// Clearing the mark hands the state back to the run underneath it.
	got = onUI(t, a, func() [2]string {
		a.catsAskingDone()
		s, st := a.catsSelfState()
		return [2]string{s, st}
	})
	if got[0] != cats.StateWorking {
		t.Errorf("clearing the question should fall back to working, got %q", got[0])
	}
	onUI(t, a, func() bool { a.busy.Store(false); return true })
}

// The pane must not keep wearing a "dbc" badge after dbc is gone.
func TestCatsClosesByReleasingThePane(t *testing.T) {
	a, srv := newCatsApp(t)
	srv.waitForState(t, cats.StateIdle, "")

	onUI(t, a, func() bool { a.catsClose(); return true })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range srv.methods() {
			if m == "pane.release_agent" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no release was sent; methods: %v", srv.methods())
}

// Outside cats the whole integration is the zero value: no reporter, no
// client, and nothing that can fail. The rest of the ui suite runs in exactly
// this state, which is the broader proof.
func TestCatsIsInertOutsideCats(t *testing.T) {
	t.Setenv(cats.EnvMarker, "")
	t.Setenv(cats.EnvPaneID, "")
	t.Setenv(cats.EnvControlSocket, "")
	t.Setenv(cats.EnvHookSocket, "")

	a := newTestApp(t)
	onUI(t, a, func() bool { a.catsInit(); return true })

	if onUI(t, a, func() bool { return a.cats.reporter != nil }) {
		t.Error("no hook socket should mean no reporter")
	}
	if onUI(t, a, func() bool { return a.catsTier1() }) {
		t.Error("outside cats must be Tier 0")
	}
	// The inert path still has to survive every call site.
	onUI(t, a, func() bool {
		a.catsAfterTransition()
		a.catsAsking("x")
		a.catsAskingDone()
		a.catsClose()
		return true
	})
}
