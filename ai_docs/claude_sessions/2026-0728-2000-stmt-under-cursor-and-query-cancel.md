# Session: statement-under-cursor execution + query cancellation

Session id: `7fdda8dc-1008-41c7-901a-3aadc05a24cd`
Date: 2026-07-28
Follows: `2026-0728-1923-dbc-tui-db-client.md` (picked up two of its "next ideas")

## Ask

Add statement-under-cursor execution and `Ctrl+K` to cancel long-running
queries — plus a Stop button if possible.

## What was built

### New package `sqlsplit`

Splits a SQL buffer into statements while tracking byte offsets, so the editor
can find the statement the cursor is in. It is a splitter, not a parser: it
only understands the constructs that can legally contain a semicolon.

- single-quoted strings (`''` doubling **and** `\'` — MySQL's default; costs us
  only Postgres strings ending in a literal backslash, noted in the code)
- `"..."` and `` `...` `` quoted identifiers
- line comments, and **nestable** block comments (Postgres allows nesting)
- Postgres dollar quoting — `$$ … $$` / `$tag$ … $tag$`, while `$1` stays a
  placeholder (a tag may not start with a digit)

API: `Split(sql) []Stmt` (drops comment/whitespace-only spans),
`IndexAt(stmts, offset) int`, `FirstKeyword(sql) string`.

Cursor rule: the statement whose span ends at or after the cursor; blank space
after a `;` belongs to the *following* statement; past the last statement it is
the last one.

### Statement-under-cursor (`ui/app.go`)

- `Ctrl+R` runs only the statement under the cursor; the log says which
  (`running statement 2/3 on demo — SELECT count(*) …`).
- A selection wins over the cursor — `stmtToRun()` checks
  `editor.GetSelection()`, whose start offset doubles as the cursor position
  when nothing is selected. That was the key tview detail: `GetSelection`
  returns byte offsets, `GetCursor` only row/column.
- Log tag is `query` for a lone statement, `selection`, or `statement N/M`.

Bonus fix this enabled: `db.isQuery` now uses `sqlsplit.FirstKeyword`, so
`-- note\nSELECT …` is detected as a query instead of being run as an exec
(it previously prefix-matched the raw text, and a leading comment broke it).

### Cancellation (`db`, `sdb`, `ui`, `main.go`)

- `Manager.RunContext(ctx, …)` is now the real implementation; `Run` delegates
  with `context.Background()`. Uses `QueryContext`/`ExecContext` plus a
  `ctx.Err()` check in the row loop.
- `db.ErrCanceled` + `wrapRunErr(ctx, err, …)`: drivers word an aborted
  statement differently ("canceling statement due to user request",
  "interrupted"), so **the context is the reliable signal** — if `ctx.Err()`
  is non-nil the error is re-wrapped as `ErrCanceled` with `cause=user|timeout`
  and the driver text kept in a field. `serr` implements `Unwrap`, so
  `errors.Is` works through the chain.
- All three drivers honor it: pgx and MySQL cancel server-side, modernc sqlite
  calls `sqlite3_interrupt` (verified in its `interruptOnDone`).
- `sdb.S` carries the context: `WithContext` (host-set), `Ctx()`,
  `Canceled()`, and package-level `IsCanceled(err)` — the last exported to
  yaegi in `script/engine.go` so scripts can unwind quietly.
- Headless: `signal.NotifyContext` on SIGINT/SIGTERM for both the one-off
  query and `dbc script`; prints `<what> canceled` and exits **130**.
- TUI `Ctrl+Q`/`Ctrl+C` now cancel in-flight work before `app.Stop()`.

### Stop button + run lifecycle (`ui/app.go`)

- Status bar became a `Flex`: status TextView (proportion 1) + `■ Stop` button
  resized between `0` and `stopBtnWidth` (10). Width 0 means `InRect` fails,
  so it can neither be seen nor clicked when idle — no disabled state needed.
- `beginRun(tag) (ctx, ok)` / `endRun() time.Duration` / `cancelRun()` replace
  the bare `busy` flag; run state (`cancel`, `runTag`, `runAt`, `tick`) is
  guarded by `runMu`.
- `tickElapsed` refreshes the status bar every 150 ms so a slow query reads as
  alive (`demo │ statement 2/2 2s`). It exits on a closed channel and its
  queued update no-ops if the run already ended — `endRun` never *waits* on the
  ticker, which is what keeps `QueueUpdate` (blocking, buffered 100) from
  deadlocking against the event loop.
- A stopped run reports `stopped after 9.394s` in the log and status, not an
  error, and leaves `lastRes` untouched.

## Verification

- `sqlsplit` unit tests: strings/escapes/nested comments/dollar quoting vs
  `$1`, offsets round-trip, cursor mapping, `FirstKeyword`.
- **`ui/app_test.go` drives the real tview event loop on a tcell
  `SimulationScreen`** — the big win over driving `screen(1)`. Covers
  statement-under-cursor at three positions, selection, `Ctrl+K` on a query,
  `Ctrl+K` on a yaegi script (`ui/testdata/slow_script.go`), an actual **mouse
  click on the Stop button** (press + release events → tview synthesizes
  `MouseLeftClick`), button show/hide, and busy refusal. Green under `-race`,
  repeated.
- Live under GNU `screen`: query ran, a 9s recursive CTE showed the ticking
  timer and the Stop button, `Ctrl+K` stopped it, `Ctrl+Q` exited clean.
- Headless: SIGINT cancels a query and a script (exit 130, script saw
  `sdb.IsCanceled`); the three sample scripts still produce their CSV/HTML.
- `go vet`, `gofmt`, full build clean.

## Gotchas worth remembering

- `Application.QueueUpdate` **blocks until the update executes** (it waits on a
  done channel), so never have a queued update wait on a goroutine that itself
  queues one.
- Calling `app.Draw()` *inside* a `QueueUpdate` callback deadlocks (lock is
  already held). In tests, wait for the ticker's redraw instead.
- `app.SetScreen()` with a fresh simulation screen while running panics in
  `CellBuffer.Fill` (never `Init`ed) and then deadlocks in `Fini` — build the
  app around the screen you want to read.
- Test helper named `sync` shadows the `sync` package — named it `drain`.
- Flags: `dbc -- "-- comment first"` is needed when SQL starts with `--`.
- Sample/fixture scripts under `testdata/` need no `//go:build ignore`;
  the go tool ignores that directory outright.

## Loose ends / next ideas

- Multi-statement headless (`dbc "a; b"`) still runs the whole string as one
  statement — `sqlsplit` makes running them in sequence easy if wanted.
- Statement highlighting in the editor was considered and skipped: tview's
  `Select` would leave a destructive selection behind.
- Status bar hints truncate below ~100 columns while the Stop button is up
  (separators were tightened to single spaces to buy room).
- Earlier ideas still open: per-connection query history, result-cell
  inspection popup.
