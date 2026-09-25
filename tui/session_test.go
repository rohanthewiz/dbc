package tui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// killSession closes the pinned session's connection behind the model's back,
// the way a server-side idle timeout would: m.sess still points at it, and the
// next statement on it fails as a bad connection.
func killSession(t *testing.T, m *Model) {
	t.Helper()
	if m.sess == nil {
		t.Fatal("no session to kill")
	}
	if err := m.sess.Close(); err != nil {
		t.Fatalf("kill: %v", err)
	}
}

// A dead session that only ever ran queries is replaced and the statement
// retried: nothing was lost, so the user need not hear about it.
func TestDeadStatelessSessionIsReplaced(t *testing.T) {
	m := newTestModel(t)
	ctx := context.Background()
	if _, err := m.runOnSession(ctx, "demo-sqlite", "SELECT 1"); err != nil {
		t.Fatalf("select: %v", err)
	}
	killSession(t, m)
	if _, err := m.runOnSession(ctx, "demo-sqlite", "SELECT 2"); err != nil {
		t.Fatalf("retry on a fresh session failed: %v", err)
	}
}

// A dead session that held state is not: replaying the statement on a fresh
// session would run it outside the transaction the user thinks is open.
func TestDeadStatefulSessionFailsLoudly(t *testing.T) {
	m := newTestModel(t)
	ctx := context.Background()
	if _, err := m.runOnSession(ctx, "demo-sqlite", "BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	killSession(t, m)

	_, err := m.runOnSession(ctx, "demo-sqlite", "COMMIT")
	if !errors.Is(err, db.ErrSessionLost) {
		t.Fatalf("err = %v, want ErrSessionLost", err)
	}
	if m.sess != nil {
		t.Error("the dead session is still pinned")
	}
	// the next run starts clean
	if _, err = m.runOnSession(ctx, "demo-sqlite", "SELECT 1"); err != nil {
		t.Fatalf("run after a lost session: %v", err)
	}
}

// Switching connections closes the old session at once — its open
// transaction rolled back — and says so, rather than leaving it open until
// the next run on the new connection happens to replace it.
func TestSwitchingConnectionReleasesSession(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Connections = append(m.cfg.Connections, config.Connection{
		Name: "other", Driver: "sqlite",
		DSN: fmt.Sprintf("file:tuitest%d?mode=memory&cache=shared", dbSeq.Add(1)),
	})
	ctx := context.Background()
	for _, stmt := range []string{"BEGIN", "DELETE FROM cats"} {
		if _, err := m.runOnSession(ctx, "demo-sqlite", stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	drive(t, m, nil, m.setActive("other"))

	if m.active != "other" {
		t.Fatalf("active = %q, want other", m.active)
	}
	if m.sess != nil {
		t.Errorf("session on %q still pinned after switching away", m.sessFor)
	}
	if !strings.Contains(logText(m), "left demo") {
		t.Errorf("no warning that the demo session's state was dropped; log:\n%s", logText(m))
	}
	res, err := m.mgr.Run("demo-sqlite", "SELECT count(*) FROM cats")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if res.Rows[0][0] == "0" {
		t.Error("the DELETE survived: the open transaction was not rolled back")
	}
}

// blackholeDSN is a postgres DSN for a loopback listener that accepts and
// never answers, so a connect to it dials and then waits in the handshake
// until something cancels it. The config sets no connect_timeout, which is
// the case N-033 was about: only a cancel ends it.
func blackholeDSN(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var held []net.Conn
	var mu sync.Mutex
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
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	})
	return "postgres://u:p@" + ln.Addr().String() + "/x?sslmode=disable"
}

// runAsync runs a command on its own goroutine, as Bubble Tea would, and
// returns a channel for its message.
func runAsync(cmd tea.Cmd) <-chan tea.Msg {
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	return ch
}

func await(t *testing.T, ch <-chan tea.Msg, what string) tea.Msg {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// Ctrl+K (and Ctrl+C) stop a connect that is still dialing, and Ctrl+C does
// not quit while one is.
func TestCancelConnect(t *testing.T) {
	for _, key := range []string{"ctrl+k", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			m := newTestModel(t)
			m.cfg.Connections = append(m.cfg.Connections,
				config.Connection{Name: "slow", Driver: "postgres", DSN: blackholeDSN(t)})

			done := runAsync(m.setActive("slow"))
			var cmd tea.Cmd
			if key == "ctrl+k" {
				cmd = m.cancelRun()
			} else {
				cmd = m.interrupt()
			}
			if cmd != nil {
				t.Fatalf("%s returned a command (quit?) while a connect was in flight", key)
			}
			drive(t, m, await(t, done, "the canceled connect"))

			if m.active != "demo-sqlite" {
				t.Errorf("active = %q, want demo-sqlite: a canceled connect switched", m.active)
			}
			if m.connCancel != nil {
				t.Error("connect still marked in flight")
			}
			if !strings.Contains(logText(m), "connect to slow canceled") {
				t.Errorf("no cancel in the log:\n%s", logText(m))
			}
		})
	}
}

// A newer pick supersedes a connect still in flight: the older one is
// canceled, and its outcome — arriving last — does not switch back.
func TestNewerConnectSupersedesOlder(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Connections = append(m.cfg.Connections,
		config.Connection{Name: "slow", Driver: "postgres", DSN: blackholeDSN(t)},
		config.Connection{Name: "other", Driver: "sqlite",
			DSN: fmt.Sprintf("file:tuitest%d?mode=memory&cache=shared", dbSeq.Add(1))})

	slow := runAsync(m.setActive("slow"))
	drive(t, m, nil, m.setActive("other"))
	if m.active != "other" {
		t.Fatalf("active = %q, want other", m.active)
	}
	drive(t, m, await(t, slow, "the superseded connect"))
	if m.active != "other" {
		t.Errorf("active = %q after the older connect landed, want other", m.active)
	}
	if strings.Contains(logText(m), "connect failed") {
		t.Errorf("a superseded connect was reported as a failure:\n%s", logText(m))
	}
}
