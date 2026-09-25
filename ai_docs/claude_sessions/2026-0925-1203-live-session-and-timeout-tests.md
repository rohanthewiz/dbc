# Session changes and pool timeouts on live servers

Session: d83c70c6-dea9-4132-886f-27bd02bd6fa3
Date: 2026-09-25

## Ask

Next-list item **N-035**: test the session changes from
`2026-0924-1725-conn-mgmt-session-discard-timeouts` against a live MySQL and
Postgres. Those tests used SQLite, bytdb and a fake driver only. The item
called for checking four things on real servers:
- closing a session ends its transaction
- the stateful and stateless dead-session paths
- `Session.Classify`'s pgx branch (`pgx.Conn.IsClosed`), which had no test
- `conn_idle_timeout` and `connect_timeout`

## Setup

Throwaway Docker containers (`postgres:17` → 17.11, `mysql:8.4` → 8.4.11)
on random localhost ports, stopped and removed afterwards (`--rm`). A probe
test was written first to log what each driver really returns, then
deleted.

## Result: no product change needed

`db/live_session_test.go` is new. It is opt-in like `db/live_test.go`, with
the same `DBC_LIVE_PG_DSN` / `DBC_LIVE_MYSQL_DSN` variables. Each test runs
once for each engine. They identify a connection by the server's own ID for
it (`pg_backend_pid()`, `CONNECTION_ID()`) and ask the server whether it
still lists that ID. That is the only way to tell "discarded" apart from
"handed back to the pool".

- `TestLiveSessionCloseEndsItsState` opens a session that runs a SET, creates
  a temp table, runs BEGIN, and updates a row (taking its lock), then closes
  it. The pool is capped at one connection. The checks:
  - a pooled write to the same row goes through at once
  - the session's uncommitted value was rolled back
  - the pool is on a new connection, and the SET value and temp table are
    gone
  - the server drops the old connection
- `TestLivePoolReturnKeepsSessionState` tests the reasoning behind
  `Session.Close` directly. When a connection is returned to the pool the
  normal way, SET values and temp tables survive on both drivers.
  Mid-transaction, pgx throws the connection away, while the MySQL driver
  hands it back with the transaction open. The next pooled statement then
  sees the uncommitted write. If a driver upgrade changes this, the test
  fails and `Session.Close`'s comment needs updating. The discard stays
  correct either way.
- `TestLiveDeadSession` covers killed and idle-timed-out connections, each
  stateless and stateful. The server's idle timeout is `idle_session_timeout`
  on Postgres; inside a transaction Postgres needs
  `idle_in_transaction_session_timeout` instead. MySQL uses `wait_timeout`.
  The first statement after the cut fails with the driver's own error, not
  `ErrBadConn`:
  - Postgres: `FATAL 57P01` / `57P05` / `25P03`
  - MySQL: `invalid connection` / `Error 4031`

  Only `alive` catches that, and on Postgres that is the pgx `IsClosed`
  branch. The result is FaultDrop when stateless and FaultLost when
  stateful. A second statement gets `ErrBadConn` (Retry or Lost). The server
  rolls back the lost transaction, and a new session works.
- `TestLiveSessionSQLErrorKeepsSession`: a bad table and Postgres's
  aborted-transaction state both give FaultNone, and the session stays on the
  same connection.
- `TestLiveConnIdleTimeout`: with a 1s timeout, the idle pooled connection
  disappears on the server. A pinned session outlives the timeout on its
  original connection.
- `TestLiveCutPooledConnReplaced`: a pooled connection killed on the server
  is replaced without an error on the next statement.
- `TestLiveConnectTimeoutSparesStatements`: with a 1s `connect_timeout`,
  1.5s statements finish, both pooled and on a session.

Supporting changes:
- `liveMgr` (`db/live_test.go`) now takes an optional config tweak.
- `TestConnectTimeoutBoundsOpen` (`db/session_test.go`) now runs for MySQL
  too, against the blackhole listener with no server needed. go-sql-driver
  honors the ping's context through its handshake, so `connect_timeout`
  bounds MySQL opens as well.

## Found on the way: pgx's 1s ping window (raised as N-043)

The first run of `TestLiveCutPooledConnReplaced` failed on Postgres. The
connection was killed and then reused about 20ms later, and the first pooled
statement returned `FATAL 57P01`. The cause is pgx v5.10.0's stdlib
`ResetSession`: it pings before reuse only when the connection has been idle
more than 1s since its last checkout. MySQL checks on every checkout.

`config.DefaultConnIdleTimeout`'s comment ("replaced transparently") is
right for interactive use but not within that second. The test now waits
past the 1s, with a comment explaining why. `stdlib.OptionShouldPing` would
close the window, at the cost of one round trip per checkout.

## Verification

- The live suite passes on both servers, three runs in a row
  (`-count=3`). The full `go test ./...` passes, with the live tests
  skipping when no DSN is set.
- Checked that the tests catch the bugs they are meant to:
  - Reverting `Session.Close` to a plain `conn.Close()` fails the MySQL
    close test: n=12 (the pooled update ran inside the leaked transaction),
    and the pool handed out the session's connection again. Postgres passes
    there, correctly, because pgx discards a connection mid-transaction.
  - Disabling the pgx branch of `driverConnAlive` fails all four Postgres
    dead-session cases (`Classify = 0`).
  - `db/manager.go` was restored from a scratchpad copy after each check
    and is unchanged.

## Next

Closed: N-035. Declined: None. Raised: N-043.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
