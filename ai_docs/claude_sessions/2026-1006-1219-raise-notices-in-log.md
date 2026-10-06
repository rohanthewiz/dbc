# Postgres RAISE NOTICE / EXCEPTION in the log

Session: `470ed914-dd39-42e6-91b6-273b2f7f8aa6`

## Ask

From the cats-todo backlog: "Raise notice messages are not shown anywhere.
Perhaps we can show these in the message console at the bottom." Example:
`RAISE NOTICE '% : %', r.tablename, cnt;`. Follow-up question: what about
`RAISE EXCEPTION '… (%) …', p_num_words, total, p_job_name;`?

## What was done

### Notices (commit `2624ce1`)

Postgres sends a notice on the connection while a statement runs. pgx hands
it to `ConnConfig.OnNotice` together with the `*pgconn.PgConn`, but with no
context and no statement, so dbc had been dropping every notice. The new
`db/notice.go` routes notices by connection:

```
Manager.Session ─► noticeSinks[pgconn] = buf      (pgx sessions only)
Session.Run     ─► buf.reset ─► statement ─► pgconn reader ─► dispatchNotice ─► buf.add
Session.Notices ─► buf.take
Session.Close   ─► delete(noticeSinks, pgconn)
```

- `openPool` sets `pcfg.OnNotice = dispatchNotice` on every Postgres pool.
  `noticeSinks` is a package-level `sync.Map`. A PgConn pointer is unique in
  the process, and the callback is fixed when the pool opens.
- `Notice{Severity, Message, Detail, Hint}`, with `String()` in psql style:
  `NOTICE: users : 42 — DETAIL: … — HINT: …`. The severity is the
  unlocalized one when the server sends it.
- There is a cap of 1000 notices per statement. Past it they are counted,
  and `take` appends `… N more notices not shown`.
- Notices are kept when the statement fails, because the ones raised before
  an exception often explain it.
- `workspace.runOnSession` now also returns the notices. It takes them
  inside the `onSession` closure, so on a retry only the last attempt's
  notices are kept. `Run`'s job turns them into Notes (`noticeNotes`:
  WARNING → Warn, everything else → Info) and puts them into `RunDone.Notes`
  before `landRun` appends the completed or failure note. Both UIs already
  log `RunDone.Notes`: the web hub's `t.notes` and the TUI's `m.notes`.
- Not covered: pooled runs (`Manager.RunContext`, which a script's `s.Query`
  and the catalog queries use) still drop notices. MySQL warnings and SQLite
  are untouched. See N-122.

### RAISE EXCEPTION (uncommitted at the time of the doc, in this wrap)

A probe against live Postgres showed that RAISE EXCEPTION was already
logged as the run's red error line,
`ERROR: <message> (SQLSTATE P0001) - Error: conn[live], location[…]`, with
any earlier notices above it. The gap was `USING DETAIL/HINT`:
`PgError.Error()` contains only the message. `db.withPgDetail` (called from
`wrapRunErr` when the context was not canceled) appends
`— DETAIL: … — HINT: …` with `%w`, so `errors.As(*PgError)` still works.
CONTEXT (`Where`) was deliberately left out (N-121).

## Tests

- `db/notice_test.go`: `TestNoticeString`, `TestNoticeBufCapAndTake`,
  `TestSessionNoticesNilOffPostgres` (SQLite registers no sink), and
  `TestWithPgDetail`. The live test `TestLiveSessionNotices` covers the order
  of a DO loop's notices plus a WARNING with a HINT, notices kept before an
  exception, an empty buffer for the next statement, the exception's
  DETAIL/HINT in the error, and the sink gone after Close.
- `workspace/live_test.go` `TestLiveWorkspaceNotices`: two statements
  produce the NOTICE (Info), then the WARNING (Warn), then the Ok completed
  note.
- `workspace_test.go`: only the call sites changed for the new
  `runOnSession` signature.
- `go test ./...` passes with `DBC_LIVE_PG_DSN` against a throwaway
  `postgres:17` container (the recipe in the `live-db-tests` memory, with
  `CATS_*` stripped). The web page itself was not checked in a browser, but
  notices take the same log path as the "completed" line.

## Housekeeping

`ai_docs/todo/next-list.md` already had an edit outside this session: N-044
and N-061 were moved from Open to Roadmap. That edit is committed with this
doc.

## Next

Closed: None. Declined: None. Raised: N-121, N-122.
Deferred: N-044, N-061 (moved by the user). Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
