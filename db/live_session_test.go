package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/model"
)

// The live session tests check Session and the pool settings against real
// servers. The rest of the suite covers them with SQLite, bytdb and a fake
// driver (fault_test.go). What a real server adds is its own view of a
// connection: a backend it lists, can kill, or drops on its own when idle,
// and a driver that finds out only when it next reads the socket. Opt-in
// like live_test.go (same DSN variables). They create and drop a
// dbc_live_sess table; the Postgres DSN must allow SET idle_session_timeout
// (Postgres 14+).
//
// Each test names a connection by the server's id for it (pg_backend_pid,
// CONNECTION_ID), and checks what the server says about that id. That is the
// only way to tell "the connection was discarded" from "it was handed back
// to the pool" — both look the same from a new statement that happens to see
// no leftover state.

// liveEngine is what the session tests need to know about one server.
type liveEngine struct {
	env, driver string
	// backend returns the server's id for the connection it runs on.
	backend string
	// listed counts the connections with the id given to %s: 1 while the
	// server still holds it, 0 once it has let it go.
	listed string
	// kill ends the connection with the id given to %s, from another one.
	kill string
	// idle and idleTx make the server drop the connection they run on once it
	// has sat idle a second, outside a transaction and inside one. Postgres
	// needs both: idle_session_timeout does not apply inside a transaction.
	idle, idleTx string
	// setVar leaves a session variable behind; getVar reads back varSet while
	// it is still there.
	setVar, getVar, varSet string
	// keepsTx: the driver hands a connection back to the pool mid-transaction
	// (go-sql-driver/mysql), rather than discarding it (pgx).
	keepsTx bool
}

var liveEngines = []liveEngine{
	{
		env: "DBC_LIVE_PG_DSN", driver: "postgres",
		backend: "SELECT pg_backend_pid()",
		listed:  "SELECT count(*) FROM pg_stat_activity WHERE pid = %s",
		kill:    "SELECT pg_terminate_backend(%s)",
		idle:    "SET idle_session_timeout = '1s'",
		idleTx:  "SET idle_in_transaction_session_timeout = '1s'",
		setVar:  "SET application_name = 'dbc_live_sess'",
		getVar:  "SELECT current_setting('application_name')",
		varSet:  "dbc_live_sess",
	},
	{
		env: "DBC_LIVE_MYSQL_DSN", driver: "mysql",
		backend: "SELECT CONNECTION_ID()",
		listed:  "SELECT count(*) FROM information_schema.processlist WHERE id = %s",
		kill:    "KILL %s",
		idle:    "SET SESSION wait_timeout = 1",
		idleTx:  "SET SESSION wait_timeout = 1",
		setVar:  "SET @dbc_live = 'dbc_live_sess'",
		getVar:  "SELECT @dbc_live",
		varSet:  "dbc_live_sess",
		keepsTx: true,
	},
}

// forLive runs fn once per engine, as a subtest named for its driver; each
// skips on its own when its DSN is unset.
func forLive(t *testing.T, fn func(t *testing.T, e liveEngine)) {
	for _, e := range liveEngines {
		t.Run(e.driver, func(t *testing.T) { fn(t, e) })
	}
}

// runner is a place to run a statement — the pool or a Session — so liveVal
// reads a value from either.
type runner func(q string) (*model.Result, error)

func onPool(mgr *Manager) runner {
	return func(q string) (*model.Result, error) { return mgr.Run("live", q) }
}

func onSession(s *Session) runner {
	return func(q string) (*model.Result, error) { return s.Run(context.Background(), q) }
}

// liveVal runs a one-value query and returns the value as displayed.
func liveVal(t *testing.T, run runner, q string) string {
	t.Helper()
	res, err := run(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("%s: want one value, got %v", q, res.Rows)
	}
	return res.Rows[0][0]
}

// liveTable makes dbc_live_sess with the one row (1, 1), dropped afterwards.
func liveTable(t *testing.T, mgr *Manager) {
	t.Helper()
	liveExec(t, mgr,
		`DROP TABLE IF EXISTS dbc_live_sess`,
		`CREATE TABLE dbc_live_sess (id int PRIMARY KEY, n int)`,
		`INSERT INTO dbc_live_sess VALUES (1, 1)`)
	t.Cleanup(func() { _, _ = mgr.Run("live", `DROP TABLE IF EXISTS dbc_live_sess`) })
}

// waitGone polls, through obs, until the server no longer lists connection
// id. Letting go of a connection is asynchronous on the server side — a
// killed backend still has to exit, an idle one waits for its timer — so a
// single look straight after would race it.
func waitGone(t *testing.T, obs *Manager, e liveEngine, id string) {
	t.Helper()
	q := fmt.Sprintf(e.listed, id)
	deadline := time.Now().Add(15 * time.Second)
	for liveVal(t, onPool(obs), q) != "0" {
		if time.Now().After(deadline) {
			t.Fatalf("the server still lists connection %s", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Session.Close discards its connection, so whatever the session left open
// ends with it on the server: the transaction (and its row lock), SET values
// and temp tables. The pool is capped at one connection, so if Close had
// handed the connection back, the very next pooled statement would be on it.
func TestLiveSessionCloseEndsItsState(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		ctx := context.Background()
		mgr := liveMgr(t, e.env, e.driver)
		liveTable(t, mgr)
		dbh, err := mgr.DB("live")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		dbh.SetMaxOpenConns(1)

		s, err := mgr.Session(ctx, "live")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		id := liveVal(t, onSession(s), e.backend)
		for _, stmt := range []string{
			e.setVar,
			`CREATE TEMPORARY TABLE dbc_live_tmp (n int)`,
			`BEGIN`,
			`UPDATE dbc_live_sess SET n = 2 WHERE id = 1`, // holds the row lock
		} {
			if _, err = s.Run(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if err = s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		// The row lock is gone at once: a pooled write to the same row goes
		// through. The deadline is only so a held lock fails the test in
		// seconds instead of waiting out the server's lock timeout.
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err = mgr.RunContext(wctx, "live", `UPDATE dbc_live_sess SET n = n + 10 WHERE id = 1`); err != nil {
			t.Fatalf("pooled write to the session's locked row: %v", err)
		}
		// 11, not 12: the session's uncommitted 2 was rolled back
		if n := liveVal(t, onPool(mgr), `SELECT n FROM dbc_live_sess WHERE id = 1`); n != "11" {
			t.Errorf("n = %s, want 11", n)
		}

		pool := onPool(mgr)
		if got := liveVal(t, pool, e.backend); got == id {
			t.Fatalf("the pool handed out the session's connection %s again", id)
		}
		if v := liveVal(t, pool, e.getVar); v == e.varSet {
			t.Error("the session's SET value reached the pool")
		}
		if _, err = mgr.Run("live", `SELECT count(*) FROM dbc_live_tmp`); err == nil {
			t.Error("the session's temp table reached the pool")
		}
		waitGone(t, mgr, e, id)
	})
}

// Session.Close discards the connection rather than hand it back because the
// drivers' reset on return to the pool does not clear session state. This
// pins down, on the servers themselves, what that reset does — so the reasons
// given in Session.Close's comment are checked rather than read off driver
// source. If a driver upgrade changes them, this fails and that comment needs
// revisiting; the discard stays correct either way.
func TestLivePoolReturnKeepsSessionState(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		ctx := context.Background()
		mgr := liveMgr(t, e.env, e.driver)
		liveTable(t, mgr)
		dbh, err := mgr.DB("live")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		dbh.SetMaxOpenConns(1) // the connection returned is the next one out
		pool := onPool(mgr)

		// Outside a transaction, every driver hands the connection back with
		// its SET values and temp tables still on it.
		c, err := dbh.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		var id string
		if err = c.QueryRowContext(ctx, e.backend).Scan(&id); err != nil {
			t.Fatalf("backend: %v", err)
		}
		for _, stmt := range []string{e.setVar, `CREATE TEMPORARY TABLE dbc_live_tmp (n int)`} {
			if _, err = c.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		_ = c.Close() // back to the pool, through the driver's reset
		if got := liveVal(t, pool, e.backend); got != id {
			t.Fatalf("connection %s was discarded outside a transaction (now %s)", id, got)
		}
		if v := liveVal(t, pool, e.getVar); v != e.varSet {
			t.Errorf("SET value = %q after the reset, want it kept (%q)", v, e.varSet)
		}
		if _, err = mgr.Run("live", `SELECT count(*) FROM dbc_live_tmp`); err != nil {
			t.Errorf("temp table gone after the reset, want it kept: %v", err)
		}

		// Mid-transaction, pgx discards the connection; the MySQL driver
		// hands it back with the transaction still open.
		c, err = dbh.Conn(ctx)
		if err != nil {
			t.Fatalf("conn: %v", err)
		}
		for _, stmt := range []string{`BEGIN`, `UPDATE dbc_live_sess SET n = 2 WHERE id = 1`} {
			if _, err = c.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		_ = c.Close()
		got := liveVal(t, pool, e.backend)
		switch {
		case e.keepsTx && got != id:
			t.Errorf("connection %s was discarded mid-transaction, want it handed back", id)
		case e.keepsTx:
			// A pooled statement that never asked for a transaction is
			// inside the one left open, and sees its uncommitted write.
			if n := liveVal(t, pool, `SELECT n FROM dbc_live_sess WHERE id = 1`); n != "2" {
				t.Errorf("pooled read n = %s, want the open transaction's 2", n)
			}
			_, _ = mgr.Run("live", `ROLLBACK`)
		case got == id:
			t.Errorf("connection %s was handed back mid-transaction, want it discarded", id)
		}
	})
}

// A session's connection can die in two ways a real server produces: killed
// by an administrator, or dropped by the server's own idle timeout. Either
// way the driver learns of it only when the next statement reads the
// socket, so that statement fails with the driver's own error, not
// driver.ErrBadConn — Classify has to ask the driver (Session.alive) to see
// the connection is dead. On Postgres that is the pgx branch, IsClosed.
func TestLiveDeadSession(t *testing.T) {
	cases := []struct {
		name     string
		stateful bool
		idle     bool // cut by the server's idle timeout instead of a kill
	}{
		{"killed, stateless", false, false},
		{"killed, stateful", true, false},
		{"idle timeout, stateless", false, true},
		{"idle timeout, stateful", true, true},
	}
	forLive(t, func(t *testing.T, e liveEngine) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				ctx := context.Background()
				mgr := liveMgr(t, e.env, e.driver)
				liveTable(t, mgr)
				s, err := mgr.Session(ctx, "live")
				if err != nil {
					t.Fatalf("session: %v", err)
				}
				defer s.Close()
				id := liveVal(t, onSession(s), e.backend)

				if c.idle {
					set := e.idle
					if c.stateful {
						set = e.idleTx
					}
					// Straight on the connection: through s.Run a SET would
					// make the session stateful, which the stateless cases
					// must not be.
					if _, err = s.conn.ExecContext(ctx, set); err != nil {
						t.Fatalf("%s: %v", set, err)
					}
				}
				if c.stateful {
					for _, stmt := range []string{`BEGIN`, `UPDATE dbc_live_sess SET n = 2 WHERE id = 1`} {
						if _, err = s.Run(ctx, stmt); err != nil {
							t.Fatalf("%s: %v", stmt, err)
						}
					}
				}
				if s.Stateful() != c.stateful {
					t.Fatalf("Stateful = %v, want %v", s.Stateful(), c.stateful)
				}
				if !c.idle {
					liveExec(t, mgr, fmt.Sprintf(e.kill, id))
				}
				waitGone(t, mgr, e, id)

				// First statement after the cut: sent, then the socket read
				// fails. Not ErrBadConn, so only alive can call it dead.
				_, err = s.Run(ctx, `SELECT 1`)
				if err == nil {
					t.Fatal("statement on a dead connection succeeded")
				}
				if BadConn(err) {
					t.Errorf("first error is ErrBadConn (%v); this case is meant to exercise alive", err)
				}
				if s.alive() {
					t.Errorf("alive after the cut (err: %v)", err)
				}
				want := FaultDrop
				if c.stateful {
					want = FaultLost
				}
				if got := s.Classify(err); got != want {
					t.Errorf("Classify = %d, want %d (err: %v)", got, want, err)
				}

				// A holder that kept the session anyway: the driver now knows
				// before sending, says ErrBadConn — the case safe to retry,
				// unless there was state to lose.
				_, err = s.Run(ctx, `SELECT 1`)
				if !BadConn(err) {
					t.Errorf("second error = %v, want ErrBadConn", err)
				}
				want = FaultRetry
				if c.stateful {
					want = FaultLost
				}
				if got := s.Classify(err); got != want {
					t.Errorf("second Classify = %d, want %d (err: %v)", got, want, err)
				}
				if err = s.Close(); err != nil {
					t.Errorf("close of a dead session: %v", err)
				}

				if c.stateful {
					// the server rolled back what the lost session had open
					if n := liveVal(t, onPool(mgr), `SELECT n FROM dbc_live_sess WHERE id = 1`); n != "1" {
						t.Errorf("n = %s after the lost transaction, want 1", n)
					}
				}
				// and the replacement session works
				s2, err := mgr.Session(ctx, "live")
				if err != nil {
					t.Fatalf("new session: %v", err)
				}
				defer s2.Close()
				if got := liveVal(t, onSession(s2), e.backend); got == id {
					t.Errorf("new session is on the dead connection %s", id)
				}
			})
		}
	})
}

// An error from the server on a live connection — bad SQL, an aborted
// transaction — must leave the session alone. alive answering wrongly here
// would throw the user's transaction away on a typo.
func TestLiveSessionSQLErrorKeepsSession(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		ctx := context.Background()
		mgr := liveMgr(t, e.env, e.driver)
		s, err := mgr.Session(ctx, "live")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		defer s.Close()
		id := liveVal(t, onSession(s), e.backend)
		if _, err = s.Run(ctx, `BEGIN`); err != nil {
			t.Fatalf("begin: %v", err)
		}
		for _, bad := range []string{
			`SELECT * FROM dbc_live_no_such_table`,
			// on Postgres the transaction is now aborted, and this one fails
			// for that reason — still a live connection
			`SELECT 1`,
		} {
			_, err = s.Run(ctx, bad)
			if err == nil {
				continue // MySQL: the transaction is not aborted, so SELECT 1 runs
			}
			if got := s.Classify(err); got != FaultNone {
				t.Errorf("%s: Classify = %d, want FaultNone (err: %v)", bad, got, err)
			}
		}
		if _, err = s.Run(ctx, `ROLLBACK`); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if got := liveVal(t, onSession(s), e.backend); got != id {
			t.Errorf("session moved from connection %s to %s", id, got)
		}
	})
}

// conn_idle_timeout closes an idle pooled connection on the server too, and
// leaves a pinned session alone however long it sits: a session is checked
// out, never idle, so the pool's cleaner never sees it.
func TestLiveConnIdleTimeout(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		ctx := context.Background()
		mgr := liveMgr(t, e.env, e.driver, func(c *config.Config) { c.ConnIdleTimeout = time.Second })
		// Watch from another Manager: polling through mgr would reuse the
		// idle connection and so keep it from ever going idle.
		obs := liveMgr(t, e.env, e.driver)

		// The session first, so the pooled statement after it has to open a
		// second connection rather than reuse the session's.
		s, err := mgr.Session(ctx, "live")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		defer s.Close()
		sid := liveVal(t, onSession(s), e.backend)
		start := time.Now()
		pid := liveVal(t, onPool(mgr), e.backend)
		if pid == sid {
			t.Fatalf("pooled statement ran on the session's connection %s", sid)
		}

		waitGone(t, obs, e, pid)
		if waited := time.Since(start); waited < time.Second {
			t.Fatalf("pooled connection gone after %s, before the 1s timeout: not the pool's doing", waited)
		}
		if got := liveVal(t, onSession(s), e.backend); got != sid {
			t.Errorf("session moved from connection %s to %s", sid, got)
		}
		if got := liveVal(t, onPool(mgr), e.backend); got == pid {
			t.Errorf("pool reused the closed connection %s", pid)
		}
	})
}

// config.DefaultConnIdleTimeout says a pooled connection the server cuts
// sooner is replaced transparently, by the driver's liveness check on
// checkout. The pool is capped at one connection, so the next statement has
// to check out the cut one.
//
// The checks differ. go-sql-driver/mysql peeks at the socket on every
// checkout. pgx pings only when the connection has been idle more than a
// second since its last checkout (stdlib ResetSession, v5.10.0), so a
// Postgres connection cut and reused within that second fails its first
// statement — found here with a kill straight after the statement. That
// window does not arise in interactive use, where a pooled connection sits
// for longer between statements, so the test waits it out rather than
// ask pgx to ping on every checkout.
func TestLiveCutPooledConnReplaced(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		mgr := liveMgr(t, e.env, e.driver)
		obs := liveMgr(t, e.env, e.driver)
		dbh, err := mgr.DB("live")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		dbh.SetMaxOpenConns(1)
		id := liveVal(t, onPool(mgr), e.backend)
		used := time.Now()
		liveExec(t, obs, fmt.Sprintf(e.kill, id))
		waitGone(t, obs, e, id)
		time.Sleep(time.Until(used.Add(1200 * time.Millisecond))) // past pgx's 1s

		res, err := mgr.Run("live", e.backend)
		if err != nil {
			t.Fatalf("first statement after the server cut the pooled connection: %v", err)
		}
		if got := res.Rows[0][0]; got == id {
			t.Errorf("still on the cut connection %s", id)
		}
	})
}

// connect_timeout bounds opening a connection and nothing after it: a real
// server's handshake fits well inside a second, and a statement that runs
// longer than that is not cut short. (The bound itself, against a server that
// never answers, is TestConnectTimeoutBoundsOpen.)
func TestLiveConnectTimeoutSparesStatements(t *testing.T) {
	sleep := map[string]string{"postgres": `SELECT pg_sleep(1.5)`, "mysql": `SELECT SLEEP(1.5)`}
	forLive(t, func(t *testing.T, e liveEngine) {
		mgr := liveMgr(t, e.env, e.driver, func(c *config.Config) { c.ConnectTimeout = time.Second })
		if _, err := mgr.DB("live"); err != nil {
			t.Fatalf("open within connect_timeout: %v", err)
		}
		if _, err := mgr.Run("live", sleep[e.driver]); err != nil {
			t.Errorf("1.5s statement under a 1s connect_timeout: %v", err)
		}
		// a session's connection is opened by the pool, not the first ping
		s, err := mgr.Session(context.Background(), "live")
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		defer s.Close()
		if _, err = s.Run(context.Background(), sleep[e.driver]); err != nil {
			t.Errorf("1.5s statement on a session under a 1s connect_timeout: %v", err)
		}
	})
}
