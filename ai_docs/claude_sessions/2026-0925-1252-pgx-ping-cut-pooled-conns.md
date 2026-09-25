# Replace Postgres pooled connections cut within pgx's 1s window

Session: 66daec25-da45-42fe-bd23-ec38dc9ca0a4
Date: 2026-09-25

## Ask

Next-list item **N-043**: pgx v5.10.0 (stdlib `Conn.ResetSession`) pings a
pooled connection before reuse only when `idle > time.Second`. A Postgres
connection the server cut and dbc reused within that second failed its first
statement once. It was not replaced quietly, which is what the comment on
`config.DefaultConnIdleTimeout` promises. MySQL checks on every checkout.

The premise checked out in the module source. `sql.Open("pgx", dsn)` builds
pgx's bare `driverConnector`, which never sets `shouldPing`, so the only way
to pass `OptionShouldPing` is to open through `pgxstdlib.OpenDB`.

## Change

`db/manager.go`:
- **`openPool`** replaces the `sql.Open` call in `Manager.open`. Other
  drivers still use `sql.Open`. Postgres runs `pgx.ParseConfig(dsn)` and then
  `pgxstdlib.OpenDB(*cfg, OptionShouldPing(pgShouldPing))`. The DSN is now
  parsed once per pool rather than per new connection, so a `.pgpass` or
  `PG*` change takes effect when dbc reopens the pool. A malformed DSN now
  fails at parse, not at the first ping. pgconn's error still masks the
  password (checked for both URL and key/value DSNs).
- **`pgShouldPing`** returns `IdleDuration > 1s || !sockQuiet(conn)`. pgx's
  own rule stays, and the socket check can only add pings, never skip one.
  The comment has the checkout flow diagram. It also explains why this is
  not always-ping: that would add a round trip to every pooled statement,
  which is costly for a script against a remote server.

`db/sockpeek_unix.go` (new), for linux, darwin, the BSDs, solaris and illumos:
- **`sockQuiet`** does one non-blocking `recv(MSG_PEEK)` through
  `SyscallConn().Read`, and returns `true` from the callback so it never
  waits on the netpoller. This is go-sql-driver/mysql's `connCheck`, except
  that it peeks instead of reading: bytes waiting on an idle Postgres
  connection may be legitimate (a NOTIFY, a ParameterStatus), and pgx must
  still receive them. Waiting bytes, a FIN or a socket error mean the socket
  is not quiet. Under TLS it peeks at `tls.Conn.NetConn()`.
- If there is nothing to check (no file descriptor, a syscall error), the
  socket counts as quiet, so pgx's own rule decides.

`db/sockpeek_other.go` (new): on other platforms the socket always counts as
quiet, the same fallback the MySQL driver uses there.

`config/config.go`: the `DefaultConnIdleTimeout` comment now names
`db.pgShouldPing` as the thing that makes the promise hold on Postgres.

What the peek cannot see, pgx's 1s ping still covers: a path that died
silently (no FIN, no RST), and a FIN swallowed by a pgconn background read
left over from a slow write.

## Tests

- `TestSockQuiet` (new, `db/sockpeek_test.go`, loopback TCP):
  - an idle socket is quiet, and the check returns at once
  - a waiting byte makes it not quiet, and the byte can still be read
    afterwards
  - a closed peer makes it not quiet
  - `net.Pipe` (no file descriptor) is quiet
  - passes under `-race`
- `TestLiveCutPooledConnReplaced` now has two subtests. "at once" reuses the
  killed connection inside pgx's second, and fails if the kill took 1s or
  more (the peek would go untested). "after 1s" is the old test.
- Ran against throwaway Postgres 17.11 and MySQL 8.4.11 containers
  (`dbc-n043-*`, own ports, stopped afterwards):
  - **With the option:** all subtests passed 5 of 5 runs, and the full live
    suite passes.
  - **Control, option removed:** Postgres "at once" failed 3 of 3 with
    `FATAL: terminating connection due to administrator command (SQLSTATE 57P01)`.
- `go test ./...` passes. The peek files cross-compile for every Unix target
  and Windows. The `db` package itself does not build on dragonfly, solaris
  or illumos, because modernc sqlite does not support them. That was already
  true.
- Not tested live: the TLS path, since the stock postgres image has no SSL.

## Next

Closed: N-043. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
