# N-144: a Postgres "move rows" copy, and the source commits after the load

Session: `f7ad41db-b467-45cc-a1ea-f388db15f339`

## Asks

1. N-144 from the cats-todo backlog. A Postgres `Query` that is a
   `DELETE … RETURNING` fails on the direct path with a bare syntax error,
   because `describe` wraps it in `SELECT * FROM (…) LIMIT 0`. Either
   describe DML another way or refuse it up front with a clear message.
2. "/sw" (sess-wrap).

## Choice: describe DML another way

Refusing DML would have taken away something the row path already ran.
Describing it properly makes the direct path match the row path. Postgres's
extended protocol can describe a statement without running it, so the
`LIMIT 0` wrapper is not needed at all.

## Found on the way: the row path lost a failed move's rows

The Reader commits its transaction when `Next` runs out of rows. In
`copyRows` that happens before `w.Close()` commits the load. A load can
still fail at that point: a key violation in the last rows reaches the
client only as `COPY FROM` ends, and a deferred check fires there too. So a
`DELETE … RETURNING` copy with a Transform into a table that already held
one of the ids committed the delete, then rolled the load back, and the
rows were gone. The new live test reproduced it on the old code
(`source after a failed move = "": rows lost`). The direct path had the
same order (`COMMIT` the source, then `w.Close()`), but its describe error
kept DML from getting that far.

## Changes

- **`etl/copy.go` `describe`.** It now checks out a connection and calls
  pgconn `Prepare(ctx, "", query, nil)`: Parse, Describe, Sync, with no Bind
  or Execute. The statement is unnamed, so the next Parse replaces it, and
  pgx's named statement cache is not disturbed. Type names copy what pgx's
  database/sql driver reports (`TypeMap().TypeForOID` name upper-cased, else
  the OID), so `createDDL` types a Create from either path alike. It refuses
  `$n` parameters (Parse infers them and `COPY` would fail on them) and a
  statement with no columns.
- **Commit order, both paths.** The load commits first, then the source:
  - `copyDirect`: `w.Close()`. On failure, `ROLLBACK` the source;
    otherwise `COMMIT` it.
  - `copyRows`: sets `rd.holdTx`, so `Next` no longer commits at the last
    row. After `w.Close()` succeeds, `rd.Close()` commits. Every earlier
    return goes through the deferred `rd.rollback()`.
  - If the source's commit fails after the load committed, the error names
    `op=commit source` and `loaded=N`. The rows are then in both places,
    the harm that can be undone. That needs the source connection to drop
    between the two commits.
- **`etl/reader.go`.** The unexported `holdTx` field and `rollback()`
  method. `Read`'s public behavior is unchanged.
- **`returnsRows` in `copySource`.** A write (`insert`/`update`/`delete`/
  `merge`/`replace`) with no `RETURNING` in its main statement is refused
  from its text, before anything runs, on every engine. The verb comes from
  `sqlsplit.Verbs` and the keyword from `HasKeyword`, as `db.isQuery` does.
  Without this, a SQLite `DELETE FROM cats` ran and committed before the
  copy failed with "a Writer needs at least one column". `copyRows` also
  refuses a zero-column result (a CALL, a DO), with its transaction rolled
  back.
- **Docs.** `CopyOptions.Query` (it was "any SELECT") and `Copy`'s doc
  comment, a "move" paragraph and example in README's ETL section, a bullet
  in `.claude/skills/dbc/scripting.md`, and the regenerated
  `sdb/sdbapi/api.json` (`TestAPIUpToDate`).

## Tests

- **`etl/live_pg_edge_test.go` → `TestLivePGMoveRows`.** On both paths:
  `DELETE … RETURNING` and a writing `WITH` move 3 of 5 rows; a load that
  fails at its end on a duplicate key leaves all 5 source rows; a write
  without `RETURNING` (plain and after a CTE list) is refused with the
  source untouched and no table created. On the direct path, `$1` without
  Args is refused. It also checks that no session is left in a
  transaction on either side.
- **`etl/copy_test.go`.** `TestCopyOptionErrors` gains five refusals,
  including `returning` in a string, a comment and a CTE body.
  `TestCopyQueryThatWrites` is a SQLite refused `DELETE` (rows kept) and a
  `DELETE … RETURNING` move.
- The new tests fail on the original code (stash run). The direct
  "load fails at commit" case also failed with only the old commit order
  put back.
- The full `etl` suite with `-race` and `DBC_LIVE_PG_DSN` (postgres:17 in
  docker, port 55432) passes, and so does `go test ./...` with `CATS_*`
  stripped.

## Checked in the real binary

A scratch build ran with `CATS_*` unset, a scratch `HOME` and cwd, a
`dbc.toml` with one `pg` connection, and `dbc script move.go`, copying
between `pg` and `pg/dbc_etl2`:

- `DELETE FROM jobs WHERE done RETURNING *` → `copied 3 rows … (direct COPY)`.
- `DELETE FROM jobs RETURNING *` into an archive already holding id 1 →
  `duplicate key value violates unique constraint "jobs_archive_pkey"`. The
  source kept 1, 3, 5.
- `DELETE FROM jobs` → `etl: Query is DELETE without RETURNING, so it has
  no rows to copy`.

My first run let `Create` make the archive. A Create from a Query carries no
key, so the "collision" loaded fine. That was the script's mistake; the
rerun made the keyed table first.

## Trade-offs left open

- From MySQL (MariaDB), SQLite and bytdb a write commits as it runs, so a
  move from those engines is not protected (N-152).
- A same-database move whose load waits on the source's own uncommitted
  write (an archive FK into the table being emptied) now hangs until
  canceled, where it used to lose the rows (N-153).
- The source connection stays checked out until the load commits.

## Next

Closed: N-144. Declined: None. Raised: N-152, N-153. Deferred: N-146,
N-151 (the user's edit, found uncommitted and committed with this doc).
Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
