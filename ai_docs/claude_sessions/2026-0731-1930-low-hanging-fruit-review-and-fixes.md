# Low-hanging-fruit review and first round of fixes

Session: d75de9e8-ebe7-4231-bca8-1f8f1fb1fdf5

## What happened

Asked for low-hanging fruit across features, performance, and robustness, I
read the whole codebase (~3.2k lines) and wrote the findings up in
`ai_docs/fable_recommends.md` — nine robustness items, six small features,
and a note that performance needs nothing at this scale. Then implemented
robustness items 1–5 plus editor-buffer persistence, one commit per step.

## Commits (in order)

1. `396a825` — Add review recommendations doc (`ai_docs/fable_recommends.md`).
2. `cee50aa` — **Script panic messages**: `script/engine.go` used
   `reflect.ValueOf(r).String()`, which renders any non-string panic value as
   `"<*errors.errorString Value>"`. Now `fmt.Sprint(r)`.
3. `b537bc7` — **Pin editor runs to one session per connection**. Ctrl+R used
   to take any pooled connection, so BEGIN/COMMIT/SET/temp tables across
   separate runs landed on different connections and leaked transaction state
   into the pool. Added `App.runOnSession` + `sessMu/sess/sessName`: a
   `db.Session` pinned to the active connection, opened lazily, swapped on
   connection change, rebuilt once on `db.BadConn` (new helper:
   `driver.ErrBadConn || sql.ErrConnDone`). The in-memory demo SQLite now
   allows 2 conns (was 1) so the pinned TUI session cannot starve scripts on
   the pool — with 1 that was a deadlock. Test:
   `TestSessionStateCarriesAcrossRuns` (temp table survives three runs).
4. `4453086` — **Multi-statement selections** run statement-by-statement.
   `stmtToRun` → `stmtsToRun` returning `[]string` + tag; the run goroutine
   loops over `runOnSession`, tags failures `statement=i/n` like headless,
   shows the last result. Test: `TestRunMultiStatementSelection`.
5. `5c90029` — **Ctrl+C cancels when busy, quits when idle** (new
   `App.interrupt`). Modals now intercept Ctrl+C/Ctrl+Q too, so tview's
   default Ctrl+C can no longer stop the app without canceling in-flight
   work. Test: `TestCtrlCCancelsWhenBusy`.
6. `8078477` — **Log pane capped** at 2000 lines (`SetMaxLines`, const
   `logMaxLines`).
7. `5104d9e` — **Editor buffer persists** across sessions: `ui/buffer.go`
   (`bufferFile`/`loadBuffer`/`saveBuffer`), saved to
   `~/.config/dbc/buffer.sql` mode 0600 on quit, restored on launch, beats
   the demo sample. Tests in `ui/buffer_test.go`.
8. `44e370e` — Mark implemented recommendations in the doc.

README updated to match (key table, selection behavior, session semantics,
buffer persistence). All tests pass; `go vet` clean.

## Design decisions

- On a failed/canceled multi-statement selection the TUI shows no partial
  result — the error names the failing statement instead. Keeps the existing
  "canceled runs publish no result" contract (asserted by
  `TestCtrlKCancelsQuery`).
- `sessMu` is uncontended (runs are serialized by the busy slot); it exists
  for the shutdown path — `ui.Run` drops the session after `app.Run` returns.
- Session-switch closes the old session, deliberately rolling back whatever
  it had open.

## Still open (see ai_docs/fable_recommends.md)

Robustness: silent truncation in headless single-statement output; unset env
vars in DSNs expanding to ""; `default_connection`/duplicate-name validation;
SQLite file-DB busy handling; duplicate JSON export columns.
Features: query history; headless script `-f`/`-o`; list-tables key; NULL
styling in results; row/cell copy; display cap decoupled from `max_rows`.
