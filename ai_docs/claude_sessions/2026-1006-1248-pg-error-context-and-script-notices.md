# Postgres error CONTEXT and notices from scripts (N-121, N-122)

Session: `470ed914-dd39-42e6-91b6-273b2f7f8aa6` (continues
`2026-1006-1219-raise-notices-in-log`)

## Ask

"Do N-121 and N-122". Those were the two follow-ups raised by the previous
doc: show a Postgres error's CONTEXT, and show server notices that a Go
script's statements raise.

## N-121: CONTEXT on Postgres errors

`db.withPgDetail` (`db/manager.go`, called from `wrapRunErr`) already
appended DETAIL and HINT. It now also appends ` — CONTEXT: …`, built by the
new `pgContext(PgError.Where)`:

```
PL/pgSQL function check_job(text,integer) line 7 at RAISE      ┐
SQL statement "SELECT check_job('nightly', 5)"                 ├─► "…line 7 at RAISE ← SQL statement \"SELECT check_job('nightly', 5)\""
PL/pgSQL function inline_code_block line 1 at PERFORM          ┘   (DO block's own frame dropped)
```

- Frames are listed innermost first and joined by ` ← `. The DO block's own
  `inline_code_block` frame is dropped, since the block is what the user
  just ran. So a RAISE written directly in a DO block gets no CONTEXT at
  all, which keeps the earlier output for that case.
- At most 4 frames are kept, then `← …`. Each frame is cut at 160 runes
  with `…`. Whitespace runs collapse, so the log line stays one line.
- The first test run caught a real bug. A "SQL statement" frame quotes the
  statement with its newlines and without escaping, so splitting on `\n`
  broke such a frame apart. Lines are now rejoined while the current
  frame's `"` count is odd. Quoted identifiers inside the statement come in
  pairs, so the parity check holds.
- `withPgDetail` now also covers a PgError that has only a `Where`. One
  with no detail, no hint and no usable context is still returned
  unchanged.

## N-122: notices from scripts

`sdb.Query`/`Exec` went through `Manager.RunContext`, which runs on `*sql.DB`
with no way of knowing which connection was used, so no notice sink could
be registered. The new `db.Manager.RunNotices` (`db/notice.go`) handles
this:

```
pgx:     dbh.Conn ─► attachNotices ─► m.run(conn) ─► take ─► unregister ─► Conn.Close (back to the pool)
         └─ driver.ErrBadConn (never sent) ─► one retry on a fresh checkout, as *sql.DB would
others:  m.run(dbh), nil notices
```

- The connection goes back to the pool and is not discarded. A
  single-statement pooled run leaves no more session state behind than
  `RunContext` would.
- `sdb.S.run` (new) is the body of both Query and Exec. It prints each
  notice through `s.Print("%s", n.String())` before returning, whether the
  statement succeeded or failed. Notices therefore appear in the log as
  `ScriptPrint` lines, in order, next to the script's own Print lines.
  Query's signature is unchanged.
- dbc's own statements (catalog, row counts, the MySQL `KILL`) stay on
  `RunContext`, and their notices are still dropped.
- The doc comment at the top of `db/notice.go` now covers both paths.

## Tests

- `db/notice_test.go`:
  - `TestPgContext`: empty input; a DO-only frame; a function called from
    a DO block; a multi-line statement; a multi-line statement containing
    a quoted identifier, followed by its caller; a long frame; a deep
    chain.
  - `TestRunNoticesOffPostgres`: on SQLite, `RunNotices` runs and returns
    nil notices.
  - `TestLiveRunNotices`: pooled notices come back in order, and no sinks
    are left registered. A real `dbc_live_check(int)` function (dropped in
    cleanup) raises a notice and then an exception, and the error carries
    `CONTEXT: PL/pgSQL function dbc_live_check(integer) line 4 at RAISE ←
    SQL statement "SELECT dbc_live_check(5)"`. A first draft used a
    `pg_temp` function, which only exists on one pooled connection and so
    could randomly fail on the pool. That was replaced before the run.
- `workspace/live_test.go` `TestLiveWorkspaceScriptNotices`: a temporary
  script calls `s.Query` with a RAISE NOTICE and then `s.Print("after")`.
  The printed lines are `NOTICE: from script`, then `after`.
- `go test ./...` passes with `DBC_LIVE_PG_DSN` (postgres:17 container,
  `CATS_*` stripped). `-race` on `./db ./workspace` for the notice tests
  passes.

## Not done

The headless `dbc query` command (`main.go` `runStatements`) runs on a
`Session` but never reads `Notices()`, so it still drops them. This was
offered to the user and not picked up; it is raised as N-123.

## Next

Closed: N-121, N-122. Declined: None. Raised: N-123.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
