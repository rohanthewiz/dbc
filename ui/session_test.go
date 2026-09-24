package ui

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// A dead pinned session is replaced and the statement retried only when the
// session held no state; one that did fails with ErrSessionLost, because
// replaying a COMMIT on a fresh session would "succeed" with nothing to
// commit. The session is closed behind the app's back to stand in for a
// server-side idle timeout.
func TestDeadSessionRetryPolicy(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()

	if _, err := a.runOnSession(ctx, "demo", "SELECT 1"); err != nil {
		t.Fatalf("select: %v", err)
	}
	_ = a.sess.Close()
	if _, err := a.runOnSession(ctx, "demo", "SELECT 2"); err != nil {
		t.Fatalf("stateless session was not replaced: %v", err)
	}

	if _, err := a.runOnSession(ctx, "demo", "BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	_ = a.sess.Close()
	_, err := a.runOnSession(ctx, "demo", "COMMIT")
	if !errors.Is(err, db.ErrSessionLost) {
		t.Fatalf("err = %v, want ErrSessionLost", err)
	}
	if _, err = a.runOnSession(ctx, "demo", "SELECT 1"); err != nil {
		t.Fatalf("run after a lost session: %v", err)
	}
}

// blackholeDSN is a postgres DSN for a loopback listener that accepts and
// never answers: a connect to it waits in the handshake until canceled.
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

// Ctrl+K and Ctrl+C stop a connect still dialing (the config sets no
// connect_timeout, so nothing else would), and Ctrl+C does not quit the app
// while one is in flight.
func TestCancelConnect(t *testing.T) {
	for _, key := range []tcell.Key{tcell.KeyCtrlK, tcell.KeyCtrlC} {
		t.Run(tcell.KeyNames[key], func(t *testing.T) {
			a := newTestApp(t)
			dsn := blackholeDSN(t)
			onUI(t, a, func() bool {
				a.cfg.Connections = append(a.cfg.Connections,
					config.Connection{Name: "slow", Driver: "postgres", DSN: dsn})
				a.setActive("slow")
				return true
			})
			press(a, key)
			waitFor(t, a, "the connect to be canceled", func() bool {
				return strings.Contains(a.logView.GetText(true), "connect to slow canceled")
			})
			// still running (onUI would time out on a stopped loop), still on demo
			if got := onUI(t, a, func() string { return a.active }); got != "demo" {
				t.Errorf("active = %q, want demo", got)
			}
		})
	}
}
