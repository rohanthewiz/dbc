# Connection management: sessions discarded on close, lost-session policy, timeouts in config

Session: 1d31443e-e13c-44ea-8ee3-62885e2d69fc
Date: 2026-09-24

## Ask

1. "Please dbl check the conn mgmt of dbc. How long do conns last. How are
   they shared?"
2. "Do 1-4": fix the four problems the review found. Also add a default
   1-hour idle timeout for connections.
3. "make the connection timeout a config setting" (read as the idle timeout).
4. "Do the same for pingTimeout too."

## The review (before any change)

- **Lifetime:** one `db.Manager` per process, holding one `*sql.DB` per named
  connection. Each pool opens on first use, is pinged, and is cached until
  exit (`Drop` was used only for failed demo seeds). There was no pool tuning
  except for in-memory SQLite, so `database/sql`'s defaults applied: no limit
  on open connections, 2 idle kept, no expiry. The drivers' checks on
  checkout (pgx pings if idle more than 1s; MySQL runs `connCheck`) replace
  dead idle connections.
- **Sharing:** the editor keeps one pinned `*sql.Conn` (a `db.Session`) per
  UI, for the active connection. Everything else uses the pool: the sidebar
  catalog, scripts' `s.Query`/`s.Exec` (each call on any free connection),
  `s.DB()`, and `migrate`. Headless runs open one session per buffer.

Problems found, in order:

1. `Session.Close` said "an unfinished transaction is rolled back by the
   driver". Reading the driver versions in `go.mod`: pgx v5.10 throws away a
   connection whose `TxStatus` isn't idle and bytdb sends `ROLLBACK`, but
   `go-sql-driver/mysql` v1.10.0 only checks the connection is alive, and
   modernc SQLite returns it as-is. So on MySQL/SQLite an open transaction
   went back to the pool, and the next catalog refresh or script `Query`
   ran inside it. `SET` values and temp tables leaked on every driver.
2. The one retry after `BadConn` was silent. Replaying a `COMMIT` on a fresh
   session "succeeds" with nothing to commit.
3. `DB()` held the manager's lock through the 5s ping, so one unreachable
   host blocked every connection. The ping also ignored the caller's
   context.
4. Switching connections didn't release the old session until the next run,
   so a `BEGIN` stayed open (holding locks) while the user browsed elsewhere.

## What changed

### `db/manager.go`

- **`Session.Close` closes the connection instead of pooling it**, via
  `s.conn.Raw(func(any) error { return driver.ErrBadConn })`. Returning
  `ErrBadConn` from `Raw` is `database/sql`'s supported way to get a
  checked-out connection closed rather than pooled; it also closes the
  `sql.Conn`. Now closing a session ends its transaction, `SET` values and
  temp tables on every engine.
- **An anchor connection for shared in-memory SQLite.** Closing the session's
  connection could close the *last* connection to the in-memory demo
  database, and destroy it. `Manager.anchors` keeps one connection
  checked out and never used for each such database. The pool limit went
  from 2 to 3 (`memSQLiteMaxOpen`: anchor + session + pool). `Drop`/`Close`
  close the anchor first, because `DB.Close` doesn't close checked-out
  connections.
- **`Session.Stateful()`** is set once a non-query statement succeeds.
  **`db.ErrSessionLost` / `db.SessionLost(name, err)`** word the failure
  when a stateful session dies.
- **Opening a connection no longer holds the lock.** `DBContext(ctx, name)`
  records the open in progress in `opening map[string]*openCall`. Other
  callers for the same name wait on `call.done` or give up with their own
  ctx; callers for other names aren't blocked. If the opener's caller cancels,
  a waiter that still wants the connection tries again itself. A `closed` flag
  makes an open that finishes after `Close` close what it opened instead of
  caching it. `DB(name)` wraps `DBContext(context.Background(), name)`;
  `RunContext` and `Session` pass their ctx through. The work itself moved to
  `open()`.
- The pool's idle timeout and the connect timeout come from config (below).

### `tui/run.go`, `tui/app.go`, `ui/app.go`

- `runOnSession`, same logic in both UIs: on `BadConn`, always drop the
  session. If it was stateful, return `db.SessionLost`. Otherwise retry once
  on a fresh session.
- Switching connections releases the old session right away:
  `releaseSessionCmd` (tui, a `tea.Cmd` sent from `connected` once `m.active`
  has moved) and `go a.releaseSessionUnless(name)` (classic, from
  `setActive`). It runs off the UI goroutine because a running query holds
  `sessMu`. It only drops a session pinned to something *other than* the new
  active connection, so a run started in between keeps its session. It logs
  a warning only if the released session was stateful
  (`sessionReleasedMsg` in the tui).

### `config/config.go`

- `conn_idle_timeout` (`ConnIdleTimeout time.Duration`, default
  `DefaultConnIdleTimeout = time.Hour`, `"0"` = never). Passed straight to
  `SetConnMaxIdleTime`. It doesn't affect the pinned session, which is
  always in use. The in-memory SQLite demo still sets 0.
- `connect_timeout` (`ConnectTimeout`, default `DefaultConnectTimeout = 5s`,
  `"0"` = no limit of dbc's own). It wraps the first ping, which is where the
  network connect and login happen. It replaced the `pingTimeout` constant. A
  timeout set in the DSN still applies; the shorter one wins.
- The defaults are pre-filled in `LoadDemo` before decoding, the same pattern
  as `ai_context_rows`, so an absent key keeps the default and an explicit 0
  means 0. BurntSushi toml v1.6 reads duration strings with
  `time.ParseDuration` (a typo stops the load) but **bare integers as
  nanoseconds**. So `checkDuration(key, *d, def)` treats anything between 0
  and 1s, and any negative, as that mistake: it warns and falls back to the
  default.

### Docs

README session paragraph (release on switch, lost-session behavior, idle
timeout); README config example; `dbc.example.toml` documents both keys.

## Tests (all pass with `-race`)

- `db/session_test.go`:
  - `TestSessionCloseLeavesNothingInPool`: file SQLite with
    `MaxOpenConns(1)`, so the pool would hand back the same connection.
  - `TestInMemoryDBSurvivesSessionClose`
  - `TestSessionStateful`
  - `TestSlowOpenDoesNotBlockOtherConnections`: a `blackhole` loopback
    listener that accepts and never answers, which parks a pgx open in the
    login handshake.
  - `TestConnIdleTimeoutClosesIdleConns`: polls
    `Stats().MaxIdleTimeClosed`, because `database/sql`'s cleanup runs at
    most once a second.
  - `TestConnectTimeoutBoundsOpen`
- `tui/session_test.go`: stateless dead session replaced, stateful one fails
  with `ErrSessionLost`, switching releases the session and rolls back a
  `DELETE`. `ui/session_test.go`: the dead-session rule for the classic UI.
- `config/config_test.go`: `TestLoadConnIdleTimeout`,
  `TestLoadConnectTimeout` (absent / set / "0" / bare integer / negative /
  malformed), `TestDemoTimeouts`.
- Checked that the tests catch the bugs: with the old `conn.Close()` the leak
  test fails (row and temp table visible in the pool). With the anchor turned
  off, the in-memory test fails ("no such table: keep").

## Notes

- The leak and the in-memory data loss were confirmed by reading the pinned
  driver versions' `ResetSession` code and by the tests above. No live MySQL
  or Postgres was used (N-035).
- A backup made while switching fixes off for the check above went to
  `$TMPDIR`, not the scratchpad. The restore used the wrong path at first,
  was noticed, and was redone from the right copy. The final tree was
  checked before continuing.

## Next

Closed: None. Declined: None. Raised: N-033, N-034, N-035.
Deferred: None. Promoted: None.
Updated: N-008, N-010 (line references). Full list: `ai_docs/todo/next-list.md`.
