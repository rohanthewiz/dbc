package workspace

import (
	"context"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
)

// The pinned session. Every statement and explain of the active connection
// runs on one pinned connection, so BEGIN/COMMIT, SET and temp tables carry
// across runs the way they do in psql. Switching connections closes the old
// session (see releaseJob), deliberately rolling back whatever it left open.

// onSession runs f on the session pinned to conn, opening or replacing it as
// needed. Called from a Job's goroutine; it holds sessMu for as long as f
// runs.
//
// A pinned connection that has gone bad (a server idle timeout) is replaced
// once, transparently — unless the session held state (a BEGIN, a SET), in
// which case the call fails with db.ErrSessionLost rather than carry on
// without it, or the statement may already have reached the server, in which
// case its error stands rather than risk running it twice.
//
//	f(sess) ─► err ─► Classify ─┬─ FaultNone  ─► return err (nil or a SQL error)
//	                            ├─ FaultRetry ─► drop, fresh session, f again (once)
//	                            ├─ FaultDrop  ─► drop, return err
//	                            └─ FaultLost  ─► drop, return ErrSessionLost
func (w *Workspace) onSession(ctx context.Context, conn string, f func(*db.Session) error) error {
	w.sessMu.Lock()
	defer w.sessMu.Unlock()
	for retried := false; ; retried = true {
		if w.sess == nil || w.sessFor != conn {
			w.dropSessionLocked()
			sess, err := w.mgr.Session(ctx, conn)
			if err != nil {
				return err
			}
			w.sess, w.sessFor = sess, conn
		}
		err := f(w.sess)
		// db.Session.Classify holds the rule; see the Fault constants.
		// In short: retry only what never reached the server on a session
		// that held nothing; fail loudly when a transaction or setting died
		// with the connection; otherwise just stop using the dead session.
		switch w.sess.Classify(err) {
		case db.FaultRetry:
			w.dropSessionLocked()
			if !retried {
				continue
			}
		case db.FaultDrop:
			w.dropSessionLocked()
		case db.FaultLost:
			w.dropSessionLocked()
			return db.SessionLost(conn, err)
		}
		return err
	}
}

// runOnSession executes one statement on the session pinned to conn.
func (w *Workspace) runOnSession(ctx context.Context, conn, stmt string) (*model.Result, error) {
	var res *model.Result
	err := w.onSession(ctx, conn, func(s *db.Session) (err error) {
		res, err = s.Run(ctx, stmt)
		return err
	})
	return res, err
}

func (w *Workspace) dropSessionLocked() {
	if w.sess != nil {
		_ = w.sess.Close()
		w.sess, w.sessFor = nil, ""
	}
}

func (w *Workspace) dropSession() {
	w.sessMu.Lock()
	defer w.sessMu.Unlock()
	w.dropSessionLocked()
}
