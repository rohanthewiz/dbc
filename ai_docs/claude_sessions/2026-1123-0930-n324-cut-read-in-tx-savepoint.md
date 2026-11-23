# N-324: a cut read inside the user's Postgres transaction is stopped under a savepoint

Session: `af3e5e34-cc52-4206-97b9-ee25d8936bb4`

## Ask

"Next list item N-324". A read cut at `max_rows` inside a Postgres
transaction the user opened still streamed the rest of its result. N-276
stops a cut read with a cancel, and a cancel aborts the transaction, so
`readSets` passed `keepTx` and such a read was read to its end:
`BEGIN; UPDATE …; SELECT * FROM events` over 100M rows kept 1,000 rows and
ran for minutes. The item (left open on 2026-11-18) asked the user to pick
SAVEPOINT, a cursor, or leave it. Then `/sw`.

## Two findings before asking

Read first: `stmtcut`, `readSets`/`sinkStop`, `pgTagTracer`, the pgx
`CancelRequestContextWatcherHandler` (v5.10.0), explain's savepoint wrapper,
`effectOf`.

- **The SAVEPOINT version need not change what errors do.** dbc can roll
  back to its savepoint only after a cut of its own, and leave any other
  failure alone. The item's "an error would no longer abort the
  transaction" can be avoided.
- **The cursor version is worse than the item said.** Postgres never runs a
  `DECLARE CURSOR` query in parallel. It also plans it for the first rows
  (`cursor_tuple_fraction`). A big aggregate inside a transaction would lose
  its parallel workers. The fraction can be pinned, but parallelism can't.
  A portal Execute with maxRows has the same problem (`execute_once` is
  false).
- pgx already makes the next statement safe after a cancel. Its handler
  sleeps 100 ms after the cancel request, and `rows.Close` waits for that
  (`HandleUnwatchAfterCancel`). So `ROLLBACK TO` right after a stop cannot
  be hit by the late cancel.

## The user's decision

Asked with three options: SAVEPOINT (recommended), Cursor, Leave it + note.
The user picked **SAVEPOINT**.

## What changed

**`db/readsavepoint.go` (new)**

```
SAVEPOINT dbc_read ─► the read ─┬─ 'T' ─────────────────────────────► RELEASE
                                ├─ 'E', dbc left the rows, ctx alive ─► ROLLBACK TO, RELEASE
                                └─ 'E' otherwise ────────────────────► ROLLBACK TO, RELEASE,
                                                                        failing DO … RAISE
```

- `saveRead` sends `SAVEPOINT dbc_read` only when all of these hold:
  - `ex` is a `*sql.Conn` and pgconn's status is `'T'` (no round trip to learn it);
  - the read takes no row locks (`locksRows`: FOR UPDATE / NO KEY UPDATE /
    SHARE / KEY SHARE at any depth, or a `pg_*advisory_xact_lock*` call);
  - it has no LIMIT / FETCH FIRST ≤ max_rows at depth 0 (`readBound`).
- `readSavepoint.end` runs after the rows close, under `WithoutCancel` with a
  10 s timeout, like explain's undo. The status after the read decides. Text
  is never parsed.
- The `'E' otherwise` branch needs the re-abort (`reabortStmt`, a DO that
  RAISEs). It was found live: inside the savepoint, the read's own error, or
  the user's Stop, aborts only the subtransaction. The block then refused
  statements, but it kept the transaction's work and its row locks until
  ROLLBACK. Without the savepoint, Postgres frees those at the error.
  `workspace` `TestLiveWorkspaceCancel` caught it ("the aborted transaction
  still holds its row lock").
- If cleaning up a successful read fails, the result is replaced by an
  error. A transaction left aborted must not look like a read that went well.
- `pgTxStatusOf(*sql.Conn)` is shared; `Session.pgTxStatus` now calls it.

**Elsewhere**

- `db/stmtkind.go`: `query` calls `saveRead`/`end` and closes the rows before
  `end`, since the connection takes one statement at a time. `readSets` and
  `sinkStop` take `keepTx`, which is false when a savepoint is in place.
- `stmtcut`: `Point.Left()` says Close was called. The `keepTx` doc names the
  savepoint.
- `sqlsplit`: `Dialect.Words(sql) []Word{Text, Depth}` lists code words with
  their paren depth. Strings, comments, quoted names and params are skipped.
- README `max_rows` paragraph: the savepoint, its costs, the re-abort and its
  log line. The two read-to-the-end cases are now locking reads in a
  transaction, and writes or state.

## Verification

- Unit: `TestLocksRows`, `TestReadBound`, `sqlsplit` `TestWords`,
  `stmtcut` `TestLeft`. `go vet ./...` and `go test ./...` green.
- Live (`db/live_cut_test.go`):
  - `TestLiveCutInTransaction` now runs the 10M-row read on both engines,
    with Grace 0. Within budget (~0.1 s), the INSERT is kept, `dbc_read`
    is gone (3B001), and the user's `SAVEPOINT mine` from before the read
    still works.
  - New `TestLiveReadSavepoint` checks three things. Its own error aborts the
    transaction and frees an UPDATE's row lock. An error past the cut
    (Grace 1 h) is rolled back. A cut `FOR UPDATE` keeps all 5,000 row locks.
- Each guard was disabled in turn, and each time a test failed:

  | Guard removed | What failed |
  | --- | --- |
  | no savepoint | 2.26 s over budget; 25P02 |
  | no rollback after a cut | 25P02 |
  | rollback after any error | no abort |
  | no RELEASE | savepoint left behind |
  | locking reads wrapped | 5000 of 5000 rows unlocked |
  | no re-abort | lock held, no 25P02, no ErrTxAborted in workspace |

- The live suites ran on throwaway containers: postgres:16 on 55433 and
  mysql:8.4 on 33307, both stopped afterwards. Packages: `db`, `etl`,
  `workspace`, `sdb/...`, `.`, `migrate`, `pipeline`, `tui`, `web`. All ok.
- Real binary, headless, with
  `BEGIN; UPDATE …; SELECT * FROM n324_events (3M rows); SELECT pad …; ROLLBACK`.
  The old build (from a worktree at HEAD) took 949 ms for the cut read; the
  new one took 213 ms. Both showed `touched` afterwards.
- staticcheck 2026.2.2 on `db`, `stmtcut`, `sqlsplit`, darwin and
  `GOOS=linux`: clean. Last CI run on main: success.
- Not done: the TUI and dbc web on screen. They go through the same
  `Session.Run`.

## Notes

- Costs the user accepted:
  - two round trips per plain read inside a Postgres transaction;
  - a cut read lets go of its share locks at the cut;
  - a failed read adds three statements and a second ERROR line in the
    server log.
- The first "before" build used `git stash`, which left the untracked new
  file behind and failed to build. A `git worktree` at HEAD worked.
- `gofmt -l` still lists `sqlsplit/folds.go`. It was untouched here and
  left alone.

## Next

Closed: N-324. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
