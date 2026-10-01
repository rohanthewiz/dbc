package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"time"

	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
	"github.com/rohanthewiz/serr"
)

// What a pinned Session does around each statement, which the pool does for
// a pooled one on checkout or which no driver does at all. Both were found
// by the live workspace tests (workspace/live_test.go):
//
//  1. A CUT CONNECTION IS FOUND BEFORE THE STATEMENT IS SENT. The pool vets
//     a connection when it hands one out (pgShouldPing, go-sql-driver's
//     connCheck); a pinned connection is never handed out again, so nothing
//     vetted it. A server that ended the session while it sat idle
//     (idle_session_timeout, wait_timeout, a kill, a restart) then failed
//     the user's next statement with the server's parting words — "FATAL:
//     terminating connection due to administrator command", "invalid
//     connection" — since the statement had been written into the dead
//     socket and nobody could tell whether it ran. ready looks first, so
//     the death is reported as driver.ErrBadConn, "never sent", and a
//     session that held nothing is replaced and the statement retried
//     (workspace.onSession); one that held state still fails as lost.
//
//  2. A CANCELED STATEMENT IS STOPPED ON THE SERVER. pgx sends Postgres a
//     cancel request. go-sql-driver/mysql only closes its socket, and the
//     server does not look at the socket while a statement runs: a heavy
//     query (or a lock wait, holding its locks) went on for its full length
//     after the user pressed Stop. reapCanceled sends the KILL the driver
//     does not.
//
//	guard(f):  ready? ──dead──► ErrBadConn (f never runs)
//	             │ok
//	             ▼
//	           f() ──► used = now ──► canceled and the driver dropped
//	                                  the connection? ──yes──► KILL <id>

// sessionPingIdle is how long a session may sit idle before its next
// statement is preceded by a ping. It is pgx's own threshold for a pooled
// connection (stdlib ResetSession). A person typing between runs is nearly
// always past it, so in practice an interactive run costs one extra round
// trip; the statements of one multi-statement run follow each other within
// it and skip the ping, unless the socket shows the server said something.
const sessionPingIdle = time.Second

// killTimeout bounds the KILL that stops a canceled MySQL statement. It is
// sent over the pool, which may have to dial; a server too slow to take it
// in that time is left to notice the closed socket on its own.
const killTimeout = 3 * time.Second

// guard runs f, one statement's worth of work on the session, between the
// checks described above.
func (s *Session) guard(ctx context.Context, f func() error) error {
	err := s.ready(ctx)
	if err == nil {
		err = f()
	}
	s.used = time.Now()
	s.reapCanceled(err)
	return err
}

// ready checks, before a statement is sent, that the session's connection
// is still there. A fresh session needs no check: the pool vetted its
// connection a moment ago when it handed it out. After that, the connection
// is pinged when it has been idle past sessionPingIdle, or sooner when its
// socket shows the server has sent something or hung up (Postgres only:
// the MySQL driver does not expose its socket).
//
// A failed ping on a connection the driver now calls dead is returned as
// driver.ErrBadConn: the statement was never sent, which is the one case
// Classify lets a stateless session retry. A canceled ping is a cancel. A
// ping that failed yet left the connection usable (not seen in practice) is
// ignored, and the statement reports whatever is wrong.
func (s *Session) ready(ctx context.Context) error {
	if s.used.IsZero() || (time.Since(s.used) <= sessionPingIdle && s.quiet()) {
		return nil
	}
	err := s.conn.PingContext(ctx)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return wrapRunErr(ctx, err, s.name, "op", "ping")
	case BadConn(err) || !s.alive():
		return serr.Wrap(fmt.Errorf("%w: connection found closed before the statement was sent: %w", driver.ErrBadConn, err),
			"conn", s.name)
	}
	return nil
}

// quiet reports whether nothing has arrived on the session's socket since
// the last statement (sockQuiet). Only a pgx connection's socket can be
// reached; any other counts as quiet, leaving it to the idle threshold.
func (s *Session) quiet() bool {
	quiet := true
	_ = s.conn.Raw(func(dc any) error {
		if pc, ok := dc.(*pgxstdlib.Conn); ok && !pc.Conn().IsClosed() {
			quiet = sockQuiet(pc.Conn().PgConn().Conn())
		}
		return nil
	})
	return quiet
}

// learnKillID reads the server's id for the session's connection, for
// reapCanceled. It costs one round trip per session, not per statement. A
// failure leaves killID empty: a canceled statement is then only abandoned
// by the driver, as before, which is no reason to refuse the session.
func (s *Session) learnKillID(ctx context.Context) {
	var id string
	if err := s.conn.QueryRowContext(ctx, `SELECT CONNECTION_ID()`).Scan(&id); err != nil {
		return
	}
	// The id is spliced into the KILL statement, so only a number is kept;
	// CONNECTION_ID() never returns anything else.
	if _, err := strconv.ParseUint(id, 10, 64); err == nil {
		s.killID = id
	}
}

// reapCanceled stops on the server a statement the user canceled, where the
// driver only abandoned it (MySQL: see the top of this file). It acts only
// when err is a cancel and the driver has dropped the connection over it,
// which go-sql-driver/mysql does whenever a cancel lands mid-statement. A
// cancel that came before anything was sent leaves the connection usable,
// and killing it then would cost the user a session that lost nothing.
//
// It kills the connection, not just the query: the driver has closed its
// end, so the connection is finished either way, and ending it now releases
// any locks its open transaction held instead of leaving them until the
// server notices the closed socket. The KILL goes over the pool, and the
// Run waits for it, so "stopped" is shown once the server has been told.
func (s *Session) reapCanceled(err error) {
	if s.killID == "" || !errors.Is(err, ErrCanceled) || s.alive() {
		return
	}
	kctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	// Best effort: a KILL that fails (the connection already gone, an
	// "Unknown thread id") changes nothing the user can act on.
	_, _ = s.m.RunContext(kctx, s.name, "KILL "+s.killID)
	s.killID = "" // the connection is gone; never kill this id again
}
