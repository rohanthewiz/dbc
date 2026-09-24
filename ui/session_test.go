package ui

import (
	"context"
	"errors"
	"testing"

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
