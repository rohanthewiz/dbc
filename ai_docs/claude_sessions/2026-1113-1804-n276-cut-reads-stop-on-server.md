# N-276: a read cut at max_rows is stopped on the server

Session: `7dfd29f1-1321-47f1-a540-935027484b16`

## Ask

"Next list item N-276 - (begins phase 3)". The RPU plan's phase 3 opens
with it. A result cut at `max_rows` still streamed the whole result: the
fetch loop breaks at the cap, then `rows.Close()` drains. pgx's
`ResultReader.Close` reads until the command concludes, and
go-sql-driver/mysql's `Rows.Close` skips rows to EOF. `limitedRows` had
the same shape at its bound, and so did an early `etl.Reader`
`Close`/`rollback` (pipeline Preview, `rows.limit`, a script's `break`).
`doneNote` logged "1000 rows" without "truncated". The item asked for the
drain to be measured on the live suites first, since it came from driver
source. Then `/sw`.

Commit: `8cab8a1` (one commit for the item).

## Measured first

Live containers (postgres:17, mysql:8.4, the usual recipe). Before the
change, 1,000 rows of a 3M-row `SELECT … repeat('x', 200)`:

| | pool | session |
|---|---|---|
| Postgres | 1.85 s | 1.81 s |
| MySQL | 3.46 s | 3.30 s |

At 10M rows it was 6.5 s and 10.8 s. The drain was real.

One part of the item was out of date. `Duration` already included the
drain, because `stamp` runs after `query` has closed the rows (code
changed since the plan, N-289's `readSets`). So the status bar showed the
true time, not milliseconds.

## Driver facts read before building

- **go-sql-driver/mysql v1.10.0** `mysqlRows.Close` calls `finish()`
  *first*, which stops the context watcher, then `skipRows()`. So a
  context cancel during the drain does nothing on MySQL. A cancel before
  Close makes the driver close the connection, which loses a pinned
  session. Only a server-side `KILL QUERY` can cut the drain and keep the
  connection.
- **pgx v5.10.0** keeps watching the statement's context until
  ReadyForQuery inside `ResultReader.Close`, so a cancel mid-drain works.
  Its `CancelRequestContextWatcherHandler` (dbc's since N-271) sends a
  cancel request. After it, `HandleUnwatchAfterCancel` waits out a
  100 ms sleep, so every cancel costs at least 100 ms, plus a second
  connection to the server.
- **database/sql**: `Tx.awaitDone` rolls a transaction back when the
  transaction's context ends. pgx's stdlib conn is not a
  `driver.Validator`, so that rollback also discards the connection. This
  is why the Reader gives only its query, not its transaction, the
  cancelable context.

## What was built

**`stmtcut/stmtcut.go`** (new package, shared by db and etl). Its package
comment has the engine table and the flow diagram.

- `Watch(ctx)` returns a child context that carries a `Point`, plus the
  Point itself. The driver side finds the Point with `From(ctx)` and
  records how the statement can be stopped:
  - `SetKill(f)`: a stop that keeps the connection (MySQL); it takes
    precedence.
  - `SetCancels(inTx)`: cancelling the context stops the statement
    (Postgres), plus whether it started inside a transaction.
- `Point.Close(rows, keepTx)`:
  1. starts a `Grace` timer (100 ms, a variable so tests can set 0);
  2. calls `rows.Close()`;
  3. if the timer had fired, waits for the stop to finish (a KILL still
     in flight must not land on the next statement) and reports
     `stopped`.

  `keepTx` refuses a cancel that would abort the transaction the
  statement ran in. `Release` frees the context.
- Why the grace: most cut results end soon after the cap. A stop costs a
  second connection, and on Postgres the 100 ms hold. A rest that arrives
  within 100 ms is simply read.

**db**

- `pgTagTracer.TraceQueryStart` calls `SetCancels(TxStatus != 'I')` on a
  Point. It reads pgconn's status from the last ReadyForQuery, so there
  is no round trip.
- `mysqlKillConn.QueryContext`/`PrepareContext` call `noteCut`, which sets
  a kill of `killQuery(id)`. The new `mysqlKillConnector.killQuery` and
  the existing `kill` now share `send`, a fresh connection from the
  connector, as N-089's KILL uses.
- `query(ctx, ex, name, stmt, cut, args…)`: `run` passes
  `cut = isRead(stmt, d)` for kindRows. `runMaybe` and `mysqlMaybeOn`
  pass false. With cut set, the statement runs under `Watch`'s context.
  Errors are still judged against the caller's context.
- `readSets(…, pt)`: a first set that hit the cap is closed through
  `pt.Close(rows, true)`. The further sets and `rows.Err` are skipped (a
  plain read has one set). The caller's own cancel is still returned, so a
  Stop pressed during the grace window still reads as stopped, and a
  session's guard still reaps it.
- `limitedRows` does the same at its bound. Its queries are pool catalog
  reads, never inside a transaction.

**etl**

- `Reader` gains `cut`, `plain` and `done` fields. `read` runs only the
  query under the Point; the transaction and the `pgPinOutput` pins keep
  the caller's context. It calls `SetCancels(true)` itself for Postgres:
  the transaction is the Reader's own, and a raw pgx pool also stops on a
  cancel.
- `Close`:
  - rows ran out → as before;
  - failed → stop, ROLLBACK;
  - stopped early on a `plainRead` → stop, then COMMIT only if no stop
    was sent, else ROLLBACK;
  - stopped early on a query that may write → read to the end and
    COMMIT, as Close always promised.
- `rollback`/`Abort` always go through the Point.
- `plainRead` is lexical, as db's `isRead` is: a read verb (select,
  values, table, show), no writing CTE, no INTO after the main verb.
- `copyRows`'s own immediate cancel is kept, with a comment explaining
  why.
- The `Close` doc that scripts see stays short: two public sentences. The
  table moved into the function body, because `sdb/sdbapi/api.json`
  carries the doc to the assistant (regenerated).

**workspace**: `doneNote` adds " (truncated at max_rows)".

## Found on the way

- **Postgres does not act on a cancel while it sends a write's RETURNING
  rows.** It runs the `DELETE` to completion into a tuplestore before
  sending the first row (`RunFromStore` has no interrupt check), and
  query cancels are not processed during client writes. So the first
  "never cut a write" test passed even with the `isRead` guard removed.
  It was rewritten around per-row stateful reads, which a wrong cut does
  stop part way: `set_config(…, false)` per row (a cancel aborts the
  implicit transaction and reverts the setting), and MySQL's
  `@n := @n + 1` (`@n` was left at 3197 of 300000). The DELETE check
  stays, documented as standing for the rule rather than proving it.
- **A late `KILL QUERY` is harmless on MySQL 8.4.** Sent to an idle
  connection, it does not stop the next statement: `dispatch_command`
  clears `KILL_QUERY` when a new command arrives
  (`TestLiveCutMySQLLateKill`).
- **A `KILL QUERY` inside a MySQL transaction ends only the statement.**
  The transaction keeps its insert until ROLLBACK.
- **macOS kills a binary that `cp` overwrote in place** ("Killed: 9", exit
  137, no output). One before/after round therefore measured nothing
  (0.03 s). The fix is to build each binary to its own fresh path. The
  first run of a fresh build is also ~0.7 s slower. Saved to memory.

## Tests

- `db/live_cut_test.go`:
  - `TestLiveCut`: pool, and a session that keeps its server id and
    SET; nothing left running per `pg_stat_activity` and processlist.
  - `TestLiveCutInTransaction`: Grace 0; the transaction keeps its
    insert until ROLLBACK.
  - `TestLiveCutOnlyPlainReads`.
  - `TestLiveCutMySQLLateKill`.
  - `TestLiveCutCatalogRead`: `limitedRows`.

  The big reads stream ~10M rows (Postgres with `generate_series` in the
  select list, a ProjectSet, so it streams; MySQL with a 7-way cross join
  of digit tables). The time budget is 2 s.
- `etl/live_cut_test.go`, on `db.Manager` pools, as `sdb.ETLConn` gives
  them:
  - `TestLiveReaderCut`: Close, held Close and Abort after 50 rows, both
    engines.
  - `TestLiveReaderCutKeepsAWrite`: an early-closed `DELETE … RETURNING`
    deletes all 200k rows.
  - `TestLiveReaderCutLeavesNoTransaction`: same backend pid after the
    stop; the pinned `extra_float_digits` is gone; no open transaction.
- Unit tests:
  - `stmtcut`, with a fake driver whose Close drains until stopped: no
    stop within Grace; a stop after it, which Close waits for; the stop
    cancels the statement's context, not the caller's; the stopper
    table.
  - `etl` `TestPlainRead`.
  - `workspace` `TestDoneNoteSaysTruncated`.
- Each guard was disabled in turn, and each time a test failed:
  - no stop at all: cut reads took 6.5–10.8 s;
  - `keepTx` ignored: 25P02;
  - writes and stateful reads cut: the setting came back empty, `@n` was
    3197;
  - the Reader's stop removed: 5.4–7.9 s;
  - the Reader's plain check removed: 200000 rows left.
- `go test -count=1 -p 1 ./...` with both DSNs: all 34 packages green.
  `-race ./stmtcut ./db ./etl ./workspace` (unit) green.

Real binary, headless, `-t csv` on a scratch config, 1,000 rows of a
10M-row read, second runs:

| | before | after |
|---|---|---|
| Postgres | 5.2–7.2 s | 0.31 s |
| MySQL | 7.7–7.9 s | 0.30 s |

## Docs

- README, `max_rows`: a cut read is stopped on the server after 100 ms,
  with a Postgres cancel request or a MySQL `KILL QUERY`, and the session
  is kept. Two cases are still read to the end: a read inside the user's
  Postgres transaction, and a statement that writes or leaves state. The
  log line says it was truncated.
- README, Reader: an early Close or Abort stops the query; a writing query
  is read to its end and committed.
- `ai_docs/todo/next-list.md`: N-276 moved to Closed; N-324 raised. The
  memory backlog note is updated.

## Next

Closed: N-276. Declined: None. Raised: N-324. Deferred: None. Promoted:
None. Moved: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
