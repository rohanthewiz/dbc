package tui

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/cats"
)

// Fake cats sockets, as in the tview UI's tests: a hook socket that records
// one request per connection and never replies, and a control socket that
// answers one request per connection. Paths are short on purpose — a
// t.TempDir() path embeds the test name and overruns sun_path (104 bytes on
// macOS) with an error that reads like a permissions failure.

type sockRecorder struct {
	path string
	mu   sync.Mutex
	reqs []map[string]any
}

func listenUnix(t *testing.T, name string, reply func(req map[string]any) any) *sockRecorder {
	t.Helper()
	dir, err := os.MkdirTemp("", "d")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &sockRecorder{path: filepath.Join(dir, name)}
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
				var req map[string]any
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				s.mu.Lock()
				s.reqs = append(s.reqs, req)
				s.mu.Unlock()
				if reply == nil {
					return
				}
				raw, _ := json.Marshal(reply(req))
				line, _ := json.Marshal(map[string]any{"ok": true, "data": json.RawMessage(raw)})
				_, _ = conn.Write(append(line, '\n'))
			}()
		}
	}()
	return s
}

func (s *sockRecorder) states() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][2]string
	for _, r := range s.reqs {
		p, _ := r["params"].(map[string]any)
		st, _ := p["state"].(string)
		cs, _ := p["custom_status"].(string)
		out = append(out, [2]string{st, cs})
	}
	return out
}

func (s *sockRecorder) waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("timed out waiting for %s; got %v", what, s.reqs)
}

func (s *sockRecorder) requestsFor(method string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, r := range s.reqs {
		if r["method"] == method {
			p, _ := r["params"].(map[string]any)
			out = append(out, p)
		}
	}
	return out
}

// A query run is a working → idle transition, which is what cats turns into
// its "finished" notification; the pane is claimed at startup.
func TestCatsReportsRunTransitions(t *testing.T) {
	hook := listenUnix(t, "h.sock", nil)
	t.Setenv(cats.EnvMarker, "1")
	t.Setenv(cats.EnvPaneID, "w1:p3")
	t.Setenv(cats.EnvControlSocket, "")
	t.Setenv(cats.EnvHookSocket, hook.path)

	m := newTestModelInHost(t)
	hook.waitFor(t, func() bool { return len(hook.states()) >= 1 }, "the startup claim")
	if got := hook.states()[0]; got[0] != cats.StateIdle {
		t.Errorf("startup report = %v", got)
	}
	key(t, m, "ctrl+r")
	hook.waitFor(t, func() bool {
		var sawWork, sawIdle bool
		for _, s := range hook.states()[1:] {
			sawWork = sawWork || (s[0] == cats.StateWorking && s[1] == "query")
			sawIdle = sawIdle || (sawWork && s[0] == cats.StateIdle)
		}
		return sawIdle
	}, "working then idle")
}

// Outside cats nothing is dialed and ^G says what it would need.
func TestCatsIsInertOutsideCats(t *testing.T) {
	t.Setenv(cats.EnvMarker, "")
	t.Setenv(cats.EnvPaneID, "")
	m := newTestModel(t)
	if m.cats.reporter != nil || m.cats.client != nil {
		t.Error("no cats, no reporter or client")
	}
	key(t, m, "ctrl+g")
	if !strings.Contains(logText(m), "needs cats") {
		t.Errorf("log: %s", logText(m))
	}
}

// ^G lists agent panes — not ours, not plain shells, most-blocked first — and
// picking one stages the statement there unsent, then focuses it.
func TestCatsAgentPickerStagesTheStatement(t *testing.T) {
	panes := []cats.PaneInfo{
		{Pane: 1, Handle: "w1:p1", Agent: "dbc", AgentState: cats.StateIdle},
		{Pane: 2, Handle: "w1:p2", Agent: "claude", AgentState: cats.StateIdle},
		{Pane: 3, Handle: "w1:p3"},
		{Pane: 4, Handle: "w1:p4", Agent: "codex", AgentState: cats.StateBlocked},
	}
	ctl := listenUnix(t, "c.sock", func(req map[string]any) any {
		if req["method"] == cats.MethodPaneList {
			return cats.PaneListResult{Panes: panes}
		}
		return map[string]any{}
	})
	m := newTestModel(t)
	m.cats.caps = cats.Caps{InCats: true, Control: true, PaneHandle: "w1:p1", ControlSocket: ctl.path}
	m.cats.client = cats.NewClient(ctl.path)
	m.cats.self, m.cats.selfOK, m.cats.panes = 1, true, panes

	key(t, m, "ctrl+g")
	if m.menu == nil {
		t.Fatalf("^G should open the picker; log: %s", logText(m))
	}
	var labels []string
	for _, it := range m.menu.items {
		if !it.head {
			labels = append(labels, it.label)
		}
	}
	if strings.Join(labels, ",") != "codex,claude,cats chat" {
		t.Errorf("picker rows = %v", labels)
	}
	key(t, m, "enter") // codex, the blocked one
	sent := ctl.requestsFor(cats.MethodPaneSendIn)
	if len(sent) != 1 || sent[0]["pane"] != float64(4) || sent[0]["submit"] == true ||
		!strings.Contains(sent[0]["text"].(string), "FROM cats ORDER BY age") {
		t.Errorf("send_input = %v", sent)
	}
	if len(ctl.requestsFor(cats.MethodPaneFocus)) != 1 {
		t.Error("the pane the question went to should be focused")
	}
	if !strings.Contains(logText(m), "waiting there unsent") {
		t.Errorf("log: %s", logText(m))
	}
}

// A theme_changed event repaints in the host's colors.
func TestCatsThemeEventRestyles(t *testing.T) {
	m := newTestModel(t)
	data, _ := json.Marshal(cats.ThemeChangedEvent{Name: "ocean", Colors: map[string]string{
		"bg": "#101418", "fg": "#e0e0e0", "muted": "#8899aa", "line": "#223344",
		"accent": "#5aa9e6", "warn": "#e6c15a", "err": "#e65a5a",
	}})
	drive(t, m, catsMsg{kind: catsEvent, event: cats.Event{Name: cats.EventThemeChanged, Data: data}})
	if m.st.pal.Accent != "#5aa9e6" {
		t.Errorf("accent = %s", m.st.pal.Accent)
	}
	if !strings.Contains(logText(m), "theme synced to ocean") {
		t.Errorf("log: %s", logText(m))
	}
	c := frame(m)
	if c.StyleAt(0, 0) != m.st.buttonHover {
		t.Error("the next frame should be drawn in the new palette")
	}
}

// ⌘E opens export where ⌘ is trustworthy; elsewhere the key is left alone.
func TestMetaAccelerators(t *testing.T) {
	m := newTestModel(t)
	key(t, m, "ctrl+r")

	// every variable that arms the layer, cleared: the developer's own
	// terminal may well be one of them
	for _, k := range []string{"KITTY_WINDOW_ID", "GHOSTTY_RESOURCES_DIR", "WEZTERM_PANE"} {
		t.Setenv(k, "")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	if _, used := m.metaAccel(keyMsg("super+e")); used {
		t.Error("an untrusted terminal's ⌘ (maybe ⌥-as-Meta) must pass through")
	}

	t.Setenv("KITTY_WINDOW_ID", "1")
	key(t, m, "super+e")
	if _, ok := m.modal.(*exportModal); !ok {
		t.Fatalf("⌘E should open export, modal = %T", m.modal)
	}
	// over a modal a ⌘ chord is swallowed rather than typed into a field
	before := m.modal.(*exportModal).path.Text()
	key(t, m, "super+s")
	if m.modal.(*exportModal).path.Text() != before {
		t.Error("⌘S typed into the path field")
	}
	for _, a := range metaAccels {
		if a.twin == "" {
			t.Errorf("⌘%c has no Ctrl twin — nothing may be ⌘-only", a.key)
		}
	}
}

func TestOSC7EncodesThePath(t *testing.T) {
	if got := osc7CwdSeq("host", "/a b/c\x07d"); got != "\x1b]7;file://host/a%20b/c%07d\x07" {
		t.Errorf("osc7 = %q", got)
	}
}
