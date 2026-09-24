package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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
	if _, err := m.runOnSession(ctx, "demo", "SELECT 1"); err != nil {
		t.Fatalf("select: %v", err)
	}
	killSession(t, m)
	if _, err := m.runOnSession(ctx, "demo", "SELECT 2"); err != nil {
		t.Fatalf("retry on a fresh session failed: %v", err)
	}
}

// A dead session that held state is not: replaying the statement on a fresh
// session would run it outside the transaction the user thinks is open.
func TestDeadStatefulSessionFailsLoudly(t *testing.T) {
	m := newTestModel(t)
	ctx := context.Background()
	if _, err := m.runOnSession(ctx, "demo", "BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	killSession(t, m)

	_, err := m.runOnSession(ctx, "demo", "COMMIT")
	if !errors.Is(err, db.ErrSessionLost) {
		t.Fatalf("err = %v, want ErrSessionLost", err)
	}
	if m.sess != nil {
		t.Error("the dead session is still pinned")
	}
	// the next run starts clean
	if _, err = m.runOnSession(ctx, "demo", "SELECT 1"); err != nil {
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
		if _, err := m.runOnSession(ctx, "demo", stmt); err != nil {
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
	res, err := m.mgr.Run("demo", "SELECT count(*) FROM cats")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if res.Rows[0][0] == "0" {
		t.Error("the DELETE survived: the open transaction was not rolled back")
	}
}
