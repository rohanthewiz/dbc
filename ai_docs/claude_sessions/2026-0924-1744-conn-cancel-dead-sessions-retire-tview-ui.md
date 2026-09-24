# Cancelable connects, dead sessions beyond ErrBadConn, retire the tview UI

Session: 1d31443e-e13c-44ea-8ee3-62885e2d69fc
Date: 2026-09-24

Continues `2026-0924-1725-conn-mgmt-session-discard-timeouts` in the same
session. That doc covers the connection-management review, discarding
sessions on close, `conn_idle_timeout` and `connect_timeout`.

## Ask

1. "do N-033 and N-034", then "commit and push" (`6ce3293`).
2. "now drop the old UI code" (N-025).

## N-033: cancelable connects

Opening a connection called `mgr.DB(name)`, which uses a background context.
Only `connect_timeout` bounded it, and with `connect_timeout = "0"` an
unreachable host left "connecting…" up until the OS's TCP timeout.

- `tui/run.go` `connectCmd` now creates a context per connect and passes it
  to `mgr.DBContext`. The catalog fetch's 10s timeout is derived from that
  context too.
  - New Model fields: `connGen`, `connCancel`, `connName`. They're only
    touched on the Update goroutine.
  - `connectMsg` carries the `gen` it was started under. `connected` drops a
    message whose gen isn't current, and logs "connect to X canceled" as a
    warning for `ErrCanceled`.
- **Starting a connect cancels the one in flight.** This also fixes an older
  race: if you picked a slow host A and then a fast host B, A could finish
  last and switch the connection back.
- Ctrl+K cancels a connect when no run is going (`cancelRun` →
  `cancelConnect`). Ctrl+C (`interrupt`) cancels it instead of quitting.
  `shutdown` also cancels it.
- The classic UI got the same changes in `setActive`, `cancelRun`,
  `interrupt` and `quit` before it was removed (below), so commit `6ce3293`
  has them.

## N-034: dead sessions that aren't `ErrBadConn`

A driver returns `driver.ErrBadConn` only when it knows the statement was
never sent. A connection cut mid-statement comes back as the driver's own
network error, so the old policy missed it until the next run.

- New `db.Session.Classify(err) Fault`, the one rule for whoever holds a
  session:

  ```
  err == nil or connection still alive ─────────────► FaultNone
  connection dead, session stateful ────────────────► FaultLost
  connection dead, stateless, driver.ErrBadConn ────► FaultRetry
  connection dead, stateless, anything else ────────► FaultDrop
  ```

  "Alive" is checked with `sql.Conn.Raw`, and the callback always returns
  nil so the check never closes anything. `driverConnAlive` uses
  `*pgxstdlib.Conn` → `!Conn().IsClosed()`, because pgx's stdlib adapter
  doesn't implement `driver.Validator`. Every other driver is asked through
  `driver.Validator.IsValid()` (MySQL, modernc SQLite and bytdb all
  implement it). A driver with neither is assumed alive, as `database/sql`
  assumes. The pgx import is now named instead of `_`.
- **FaultDrop is the behavior change.** A stateless session whose statement
  may have reached the server is dropped, and its error shown as-is. It isn't
  retried, because the statement may already have run.
- `runOnSession` (both UIs at the time) switches on `Classify`. When a
  stopped run cost a stateful session, `reportRunErr` adds "the session was
  lost with it" after "stopped". `SessionLost` wraps with `%w: %w`, so
  `errors.Is` finds both `ErrCanceled` and `ErrSessionLost`.

## N-025: the tview UI removed

- `git rm -r ui/`: 25 files plus testdata, about 5,300 lines.
- `main.go`: removed the `-ui` flag, `DBC_UI`, the `ui` import and `envOr`
  (its only caller was the flag). `dbc` calls `tui.Run` directly.
- `go mod tidy` dropped `gdamore/tcell/v2`, `rivo/tview`,
  `gdamore/encoding` and `golang.org/x/term`. `atotto/clipboard` stays
  because `clip/` uses it.
- Checked before deleting:
  - Only `main.go` imported `ui`, and tview/tcell were used only in `ui/`.
  - The new UI has its own cats integration, ⌘ shortcuts, Ctrl+G agent
    handoff, window title and OSC 7.
  - `userdata/` uses the same history and buffer paths and format, so
    existing `~/.config/dbc` files still load.
- Comments pointing at `ui/…` paths, or describing the tview UI as present,
  were updated in:
  - `theme/host.go`, `theme/palette.go`, `theme/theme.go`
  - `userdata/history.go`, `cats/detect.go`, `cats/events.go`
  - `ai/agents.go`, `cats-plugin.toml`
  - `tui/cats.go`, `tui/app.go`, `tui/run.go`, `tui/grid.go`,
    `tui/cats_test.go`

  Historical "ported from the former tview UI" notes were kept.
- README: `./dbc -ui classic` removed from "The TUI". Old session docs and
  `ai_docs/plans/ui-revamp.md` are history and left alone.
- Scripts that pass `-ui` (even `-ui tui`) now fail with "flag provided but
  not defined". A leftover `DBC_UI` is ignored.

## Tests (full suite passes with `-race`)

- `db/fault_test.go`: `faultDriver` registered as `dbc-fault`, whose
  statements `CUT` (network error, becomes invalid), `GONE` (`ErrBadConn`),
  `FAIL` (SQL error, stays valid). A table test covers every Fault. The
  session is built directly, as `&Session{m, name, conn}`.
- `tui/session_test.go`: `TestCancelConnect` (Ctrl+K and Ctrl+C against a
  loopback listener that never answers, with no connect_timeout set) and
  `TestNewerConnectSupersedesOlder`.
- The classic UI's tests (including the ones added earlier this session)
  were removed with it. The same behavior is covered in `tui/` and `db/`.

## Notes

- The editor showed stale "undefined: Classify / FaultRetry" diagnostics
  several times after edits. `go build` and `go vet` were the source of
  truth.
- A Python string replacement in `main.go` silently missed once: a `\n`
  inside a Go string literal became a real newline in the pattern. It was
  caught by the build, and that part was redone with the Edit tool.

## Next

Closed: N-007, N-025, N-033, N-034. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: N-035 (adds `Classify`'s pgx branch). Full list: `ai_docs/todo/next-list.md`.
