# Server notices in headless `dbc query` (N-123)

Session: `ed5ab58a-cdf9-4b3c-9716-ebbc14fa5189` (continues
`2026-1006-1248-pg-error-context-and-script-notices`)

## Ask

N-123 from the next list, sent from the cats-todo backlog: headless `dbc`
(`main.go` `runStatements`) ran on a pinned `db.Session` but never called
`Session.Notices`, so RAISE NOTICE output was dropped. Print the notices to
stderr, as psql does, since stdout carries the results.

## Change

- `runHooks.onNotice func(i int, n db.Notice)` is new. `runStatements`
  drains `sess.Notices()` right after each `sess.Run`, before it handles the
  statement's result or failure. That puts a notice ahead of the error it
  led up to. The drain has to happen per statement because `Session.Run`
  resets the session's notice buffer.
- `printNotices(w, ns)` writes one `Notice.String()` line per notice, the
  same line the editor's log shows. `runQueryHeadless` sends notices to
  stderr: from the hook, after `--tx`'s `BEGIN`, and after `endTx`
  (COMMIT/ROLLBACK). A deferred constraint trigger's RAISE NOTICE fires at
  commit, so without that last drain it would be lost.

```
stmt i ─► sess.Run ─► sess.Notices() ─► onNotice ─► stderr "NOTICE: …"
                         └─► then: result (onResult/stdout) or failure (onFail / fail)
--tx: BEGIN ─► drain   …   COMMIT/ROLLBACK ─► drain
```

- README, headless section: one paragraph on notices going to stderr.

## Tests

- `main_test.go`:
  - `TestPrintNotices`: the line shape, including a hint.
  - `TestRunStatementsNoticesOffPostgres`: on SQLite, `onNotice` is never
    called.
  - `TestLiveRunStatementsNotices` (`DBC_LIVE_PG_DSN`): with `keepGoing`,
    the order of events is `0 NOTICE: one`, `0 WARNING: two`,
    `2 NOTICE: three`, `2 failed`.
- `-race` run of those tests against a postgres:17 container
  (`CATS_*` stripped): pass. Offline `go test ./...`: pass.
- The real binary was run against that container (temp HOME, `--dsn`):
  - A plain run printed `NOTICE: hello` and `NOTICE: table "nope" does not
    exist, skipping` on stderr, with stdout clean.
  - With `--tx` and a deferred constraint trigger, `NOTICE: at commit`
    appeared after the run.
  - A failing DO block printed `NOTICE: before`, then the error line, and
    exited 1.

## Next

Closed: N-123. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
