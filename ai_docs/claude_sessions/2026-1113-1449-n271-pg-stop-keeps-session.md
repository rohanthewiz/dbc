# N-271: Stop on Postgres keeps the session

Session: `acbda969-9a04-4496-9a85-1c6665f58eaa` (the same session as
`2026-1113-1433-n270-migrate-run-lock`, which closed N-270 first)

## Ask

"do N-271 next" (RPU plan, phase 2), then `/commit` (nothing left to
commit — the work was already in its commit) and `/sw`. The item: Stop on
Postgres always destroys the pinned session, because `openPool` leaves
pgx's default `DeadlineContextWatcherHandler`, which closes the connection
on cancel — `BEGIN; UPDATE …;` then Stop on a slow SELECT loses the
transaction, the SETs and the temp tables. `workspace/live_test.go:407`
pinned that as what both drivers do. Fix: pgx's
`CancelRequestContextWatcherHandler`. Related: Stop during EXPLAIN ANALYZE
of a write found the connection closed, the undo `ROLLBACK` failed with
`ErrBadConn`, and the run landed as a failure ("may not have been undone",
session lost) though the server had rolled back on disconnect.

Commit: `56c0b27`.

## pgx v5.10.0, read first

- `ParseConfig` sets `BuildContextWatcherHandler` to
  `DeadlineContextWatcherHandler{Conn}` with no delay: on ctx end the
  socket's deadline is *now*, the read fails, and pgconn's `asyncClose`
  sends a cancel request, a Terminate, and closes. That cancel request is
  why the old live tests saw the server stop the statement — the
  connection just went with it.
- `CancelRequestContextWatcherHandler{Conn, CancelRequestDelay,
  DeadlineDelay}`: on ctx end it sets the socket deadline to
  now+DeadlineDelay and (after CancelRequestDelay) sends a cancel request
  on a second connection, TLS'd like the first when the first is (it
  mirrors libpq 17's `PQcancelCreate`); a unix socket falls back to the
  config's host. `HandleUnwatchAfterCancel` waits for that goroutine,
  including a 100 ms sleep that keeps a late cancel from hitting the next
  statement. **DeadlineDelay must be non-zero**, or the socket times out
  before the cancel goes and nothing changes.
- `CopyFrom` reads the server's error after a cancel; it force-closes only
  on a write error or the deadline.
- dbc opens pgx in one place (`openPool`, which TLS also goes through), and
  sets no custom `DialFunc`.

## What was built

**`db/manager.go`** — `openPool` sets
`pcfg.BuildContextWatcherHandler = pgCancelHandler`, which returns the
cancel-request handler with `DeadlineDelay: pgCancelGrace` (2 s). The grace
is for a cancel request that cannot land (a pooler that does not forward
them, a backend in code that does not check interrupts): then the old close
happens, 2 s late. A `var`, read when a connection is made, so a test can
force 0 — pgx's default behaviour. Every Postgres pool gets it: sessions,
catalog reads, row counts, scripts, ETL.

**`db/sessionguard.go`** — `ErrTxAborted` and `markAborted`, called at the
end of `guard` (so `Run` and `Explain`): a stop (`ErrCanceled`) that leaves
the connection alive with pg tx status `'E'` is wrapped as
`ErrTxAborted`. A stop is shown as stopped, not failed, so without this the
user learned of the aborted transaction only from the next statement's
25P02.

**`workspace/run.go`** — `failedLocked` adds, for `ErrTxAborted`: "the open
transaction was aborted with it — every statement fails until ROLLBACK; SET
values and temp tables are kept" (beside the existing session-lost note).

**`db/explain.go`** — in `rolledBack`, an undo that fails on a connection
that is no longer alive returns f's own error (the stop) rather than "the
analyzed write may not have been undone": a server rolls back what a
connection had open when it ends. `stateful` is set only in the savepoint
case (the user's own transaction went too, so Classify says lost); a
stateless session is dropped quietly. With the new handler the normal case
never reaches this: the cancel lands, ROLLBACK / ROLLBACK TO SAVEPOINT runs
on the live connection, and the user's transaction is as it was.

MySQL is unchanged: go-sql-driver closes its socket on cancel and the
session sends the KILL (`reapCanceled`).

## Found on the way

**An aborted Postgres transaction holds no locks.** The rewritten live
test first asserted that the stopped transaction still held the UPDATE's
row lock until ROLLBACK, and it failed: Postgres runs AbortTransaction at
the error, undoing the work and releasing the locks then; only the
transaction *block* stays open, refusing statements until ROLLBACK. The
test now asserts the lock is free, and the comments in `manager.go` and
`sessionguard.go` (which first said "holding its locks") were corrected
before the commit. So the item's "loses the transaction" stays true on
Postgres; what the stop no longer loses is the session — SETs, temp
tables, prepared statements, the backend.

## Tests

- `workspace/live_test.go` `TestLiveWorkspaceCancel`, rewritten. The
  engine table gained `set`/`get` (a setting read back as `dbc_kept`) and
  `stopKeeps` (Postgres). Cases: stateless, after a SET, in a transaction.
  Postgres: same backend pid after the stop, the SET survives, the tx case
  is `ErrTxAborted` with its note, a SELECT gets 25P02, ROLLBACK works, the
  row lock is free (`FOR UPDATE NOWAIT` from another session). MySQL: a
  stateful session is lost as before, its lock freed once the KILL lands.
  Helpers `rowLocked`, `waitUnlocked`. **All three Postgres cases fail with
  the handler line commented out.**
- `db/live_explain_test.go` `TestLiveExplainPostgresStopWrite`: a slow
  write (`… AND (SELECT true FROM pg_sleep(60))`) analyzed and stopped,
  cancel-lands vs grace 0, no transaction vs the user's. Always
  `ErrCanceled`, never "may not have been undone", never `ErrTxAborted`,
  the write never kept; Classify None / None / Drop / Lost; in the
  user-tx case the status is back to `'T'` and the user's insert is
  there. **The two grace-0 cases fail on the old `explain.go`** ("driver:
  bad connection").
- `workspace/workspace_test.go` `TestStopNotesSayWhatTheSessionKept`
  (unit): status "stopped after 1.5s", no lastErr, and the right note for
  a plain stop, a lost session, an aborted transaction.

Runs (postgres:17, mysql:8.4): the existing db live cancel/session/explain
tests passed unchanged; `go test -count=1 -p 1 ./...` with both DSNs, all
33 packages green (etl, tui, web included); `go test -race ./db
./workspace` with both DSNs green.

Real binary: `dbc web --no-open --listen 127.0.0.1:8471 --secret …` with a
scratch config on the Postgres container, driven over its API (open a tab,
connect, run, cancel, read the SSE stream with curl): `SET
application_name = 'dbc_e2e_kept'; BEGIN; UPDATE …`, then `SELECT
pg_sleep(60)`, Stop → "query stopped after 1.694s" and the
aborted-transaction note; `SELECT n …` → 25P02; `ROLLBACK`; `SHOW
application_name` → `dbc_e2e_kept`. (A grep filter on "tables" first hid
the note — the note itself says "temp tables".)

## Docs

- README, "Stopping a long query": what a stop costs the session on
  Postgres (kept; a transaction aborted until ROLLBACK; the 2 s fallback)
  and on MySQL (lost; a stateless one replaced quietly), and that a
  stopped EXPLAIN ANALYZE of a write leaves nothing behind.
- `ai_docs/todo/next-list.md`: N-271 to Closed; N-276 updated — its fix
  (cancel a statement-scoped context when a result is cut at `max_rows`)
  now costs the session nothing on Postgres.

## Next

Closed: N-271. Declined: None. Raised: None. Deferred: None. Promoted:
None. Moved: None. Updated: N-276. Full list: `ai_docs/todo/next-list.md`.
