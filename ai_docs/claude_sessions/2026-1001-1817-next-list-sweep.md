# Next-list sweep: everything a machine could do

Session: `b93e025d-a1ab-4f7c-8d02-21f21cbc1034`

## Ask

"Do what else we can from the Next list" (`ai_docs/todo/next-list.md`).

Done here: N-072, N-081, N-084 and N-085, plus fixes the merge turned up.
Done by background agents, each in its own git worktree: N-047, N-051 (with
N-063), N-071, N-075 and N-079. Their commits were reviewed, then
cherry-picked onto main. N-061 (the Firefox half) and N-087 went to two
more agents for machine verification. The worktrees, branches and
containers were removed afterwards.

Not done:

- **Need a person or hardware:** N-044 (an X11 desktop), N-064 (clicking
  through `dbc.app`), N-029 (Windows), and Safari for N-061.
- **N-086 (release):** held until the user approved it, then cut as
  v0.3.0 (see Release).

## What changed

### N-072: recount after writes (`db/rowcount.go`, `workspace/run.go`)

- **What triggers it.** `db.ChangesRows(stmt)` is true for any statement
  that is not a plain read and not session setup (SET, BEGIN, SAVEPOINT,
  …). COMMIT and ROLLBACK count. A script is taken to have written.
- **What a run does.** When a run reached such a statement, failed or not,
  `landRun` calls `recountLocked`:
  - `Manager.ForgetRowCounts(conn)` drops the cached counts;
  - any counting in flight is canceled;
  - `RunDone.Counts` recounts the listed tables. The TUI (`runDone`) and
    the web hub run it.
- **Staleness.**
  - The cache records `forgotAt`. A counting that started before it is
    neither served nor cached, even when another workspace had it in
    flight.
  - `countGen` makes a superseded counting land as Stale, not as "row
    counts unavailable".
- **SQLite counts use one read-uncommitted connection.** On a shared-cache
  database (the in-memory demo), suppose a session's open transaction has
  written a table. Another connection's read of that table then waits in
  modernc's driver, past its context, until the transaction ends. A probe
  confirmed it. The full suite hung on it
  (`TestDisconnectAsksWhenTheSessionHoldsState`: BEGIN; DELETE).
  `PRAGMA read_uncommitted` has no effect outside shared-cache mode, and it
  is reset before the connection goes back to the pool.

### N-084: TUI schema picks persist (`userdata/picks.go`)

- Each pick is saved to `~/.config/dbc/schema-picks.json` (`SavePick`). It
  re-reads the file, sets one key and renames a temp file into place, so
  two TUIs keep each other's picks.
- `New` loads the file (`loadPicks`), and the first connect opens on the
  saved pick (`connectCmd` → `ConnectPick`).

### N-085: renames move every saved pick (`web/conns.go`, `web/store.go`)

- `handleConnEdit` → `moveSchemaPicks` moves `tableSchema.<old>` and
  `tableSchema.<old>/<db>` keys in the store's layout. Those include picks
  saved by windows that have since closed.
- `Store.MoveLayout` reads every old key, then deletes them, then writes
  the new ones, so a chain (`a` → `a/b` while `a/b` → `a/b/b`) comes out
  right.
- Old keys are deleted rather than blanked: "" is itself a pick ("every
  schema").

### N-081: quoted names in an unloaded schema (`db/catalog.go`)

- `sqlWords` marks a quoted identifier's delimiters with control bytes
  instead of dropping them, and `wordParts` splits on the dots outside
  quotes.
- In a schema the sidebar has not loaded, `Mentioned` keeps a quoted name's
  case (`billing."Invoices"`) and folds an unquoted one, as Postgres does.

### Agent work (cherry-picked)

- **N-047** (`workspace/live_test.go`): opt-in live workspace tests on both
  engines. They found two `db.Session` bugs, fixed in
  `db/sessionguard.go`:
  - A pinned connection the server cut while idle failed the next run.
    The session now pings first after 1s idle (sooner on Postgres when the
    socket shows data). It reports `driver.ErrBadConn`, so a stateless
    session is retried.
  - Stop left a MySQL statement running on the server. The session now
    sends `KILL <CONNECTION_ID()>`.
- **N-051 / N-063** (`web/e2e/`): the opt-in go-rod browser test, a module
  of its own (`DBC_E2E=1`).
- **N-071** (`erd/route.go`): lines from one port slot share a lane as a
  bus. A 400-child star is 4426×3411, down from 4426×4571, with no widening.
- **N-075** (`db/tlskey.go`): `tls_key_password` as a `${VAR}` reference.
  dbc decrypts legacy PEM and PKCS#8 PBES2 keys itself. Postgres attaches
  the certificate after parsing (`pgClientCert`). There is also a web form
  field and a `--tls-key-password` flag.
- **N-079**: the MySQL database picker. `db.HasDatabases` is separate from
  `db.Navigable`, and `mysqlConfig` sets `DBName` for `<conn>/<database>`.
  It also fixes the web `connItem` for MySQL bases.
- **N-061 (Firefox)**: Ctrl+B inside the editor did not fold the sidebar on
  a Mac (Monaco took it as cursor-left, in Chrome too). It is now bound in
  `editor.js`, and there is a `web/e2e` step for it.

### Smaller fixes

- **MySQL driver log lines.** go-sql-driver/mysql logs "packets.go:58
  unexpected EOF" to stderr, over the TUI's frame. `db.SetDriverLog` sends
  those lines to the TUI's log pane.
- **Live workspace test.** It asserted MySQL had no database level, which
  N-079 changed.

## Decisions

- **Recount the whole listed catalog, not the tables a statement names.**
  Cascades, triggers and functions reach tables the text does not name. A
  big server's sidebar holds one schema, so a recount costs what a connect's
  counting does.
- **Counts on SQLite may include an open transaction's rows.** That is what
  the user's own session sees, and the alternative was a hang.
- **0.3.0, not 0.2.2.** The user's call: the MySQL database picker, encrypted
  client keys and the session guard are new behavior, not fixes.

## Verification

- **Static checks:** gofmt and `go vet ./...` are clean, and `node --check`
  passes on `app.js` and `editor.js`.
- **Full suite:** `go test -race ./...` with `DBC_LIVE_PG_DSN` (postgres:17)
  and `DBC_LIVE_MYSQL_DSN` (mysql:8.4) set, and `CATS_*` stripped. It passed
  after the two merge fixes, except one flake of
  `TestChatSignInThenQueuedQuestionGoes`, which passed 5/5 alone (raised as
  N-091).
- **Browser test:** `web/e2e` with `DBC_E2E=1` and the Postgres step passes
  on main.
- **N-087:** the real TUI binary was driven in a pty through a VT emulator
  against 150 schemas: 40/40 checks, no bugs.
- **N-061:** Firefox via geckodriver passed 34 checks on 3 runs, after the
  Ctrl+B fix.

## Release

v0.3.0, through the `release` workflow: the version was bumped by hand in
`version/version.go` and `cats-plugin.toml` (a commit that edits
`version.go` is used as-is rather than patch-bumped), main was pushed, and
`release` fast-forwarded to it. The workflow runs `go test ./...`, tags
`v0.3.0` and builds the goreleaser archives. No bump commit comes back, so
there is nothing to merge back into main.

## Next

Closed: N-047, N-051, N-063, N-071, N-072, N-075, N-079, N-081, N-084, N-085, N-086.
Declined: None. Raised: N-088, N-089, N-090, N-091, N-092, N-093.
Deferred: None. Promoted: None.
Updated: N-061, N-087. Full list: `ai_docs/todo/next-list.md`.
