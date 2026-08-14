package ui

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/cats"
)

// fullServer answers the whole startup conversation on one socket: ping,
// config.get, pane.list, and the event stream.
type fullServer struct {
	path string
	mu   sync.Mutex
	got  []string
}

func newFullServer(t *testing.T) *fullServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "d")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &fullServer{path: filepath.Join(dir, "c.sock")}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
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
				var req struct {
					Method string `json:"method"`
				}
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					return
				}
				s.mu.Lock()
				s.got = append(s.got, req.Method)
				s.mu.Unlock()

				var data any
				switch req.Method {
				case cats.MethodPing:
					data = cats.Pong{Protocol: 1, Service: "catway"}
				case cats.MethodConfigGet:
					data = cats.ConfigGetResult{Theme: cats.ConfigTheme{Name: "cats-green"}}
				case cats.MethodPaneList:
					data = cats.PaneListResult{Panes: []cats.PaneInfo{{Pane: 7, Handle: "w1:p7"}}}
				case cats.MethodSubscribe:
					_, _ = conn.Write([]byte(`{"ok":true}` + "\n"))
					time.Sleep(2 * time.Second)
					return
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

func (s *fullServer) saw(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.got {
		if m == method {
			return true
		}
	}
	return false
}

func (s *fullServer) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// The whole startup conversation, driven by catsInit exactly as Run drives it:
// probe, resolve our own pane, subscribe, prime the pane cache.
func TestCatsInitCompletesTheTier1Handshake(t *testing.T) {
	srv := newFullServer(t)
	t.Setenv(cats.EnvMarker, "1")
	t.Setenv(cats.EnvPaneID, "w1:p7")
	t.Setenv(cats.EnvControlSocket, srv.path)
	t.Setenv(cats.EnvHookSocket, "")

	a := newTestApp(t)
	onUI(t, a, func() bool { a.catsInit(); return true })

	waitFor(t, a, "Tier 1", func() bool { return a.catsTier1() })
	for _, m := range []string{cats.MethodPing, cats.MethodPaneList, cats.MethodSubscribe} {
		deadline := time.Now().Add(5 * time.Second)
		for !srv.saw(m) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !srv.saw(m) {
			t.Errorf("startup never sent %s; saw %v", m, srv.all())
		}
	}
	if !onUI(t, a, func() bool { return a.cats.selfOK && a.cats.self == 7 }) {
		t.Error("our own pane id was not resolved")
	}
	onUI(t, a, func() bool { a.catsClose(); return true })
}
