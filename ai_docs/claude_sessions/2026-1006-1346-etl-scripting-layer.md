# ETL scripting layer: copy tables across connections

Session: `0297b8d8-8c67-4e33-aed6-0b67619aa0ef`

## Ask

From the user's cats-todo backlog: come up with a scripting layer that does
ETL work across possibly several connections. Look at the core db code of
dbx (`~/xfr2/dbx/core`). A simple first goal: copy a Postgres table on one
connection to another.

Later in the session: commit, merge into local `main`, and wrap the session
there.

## Findings before building

- `~/xfr2/dbx` has no `core` folder. The core code is in
  `~/projs/go/dbx/core` (`dbase`, `dbquery`, `dbtable`). `DBQuery` turns
  every value into a string with `fmt.Sprintf` and keeps the whole result in
  memory as `[]map[string]any`. That shape is fine for reports but loses data
  for a copy and doesn't scale, so only the idea was kept (query, then
  table-like rows, then output).
- dbc already had a Go scripting layer: yaegi (`script/engine.go`) running
  `func Run(s *sdb.S) error`, with `s.Query`/`s.Exec`/`s.DB`. The ETL layer
  extends it instead of adding a second scripting language. `s.Query`
  doesn't suit ETL: it caps rows at `max_rows` and renders values for
  display.

## What changed

### New package `etl`

- `engine.go`: `Engine` (Postgres, MySQL, SQLite, Bytdb) from the canonical
  driver name. Identifier/table quoting (a name that already contains a
  quote is used as given), placeholders, `truncateStmt`, `transactionalDDL`,
  and a type-family map used by `Create` across engines. Integer types match
  exactly, because a substring match took `_INT4` arrays and `INT4RANGE` as
  integers.
- `reader.go`: `Reader` streams typed rows (`Next`/`Row`/`Err`/`Close`) with
  no row cap. `[]byte` from a column that isn't a binary type becomes a
  string, since MySQL's text protocol returns most values as bytes.
- `writer.go`: `Writer` loads rows into one table inside one transaction.
  `Write`, then `Close` commits; `Abort` (a no-op after Close) rolls back,
  and so does any failed `Write`. `WriteOptions{Setup, Truncate, BatchSize}`.
  - Postgres: `COPY … FROM STDIN` in **text** format through an `io.Pipe`,
    with a goroutine holding `pgconn.CopyFrom` inside `conn.Raw`, and
    BEGIN/COMMIT on a dedicated `*sql.Conn`. Text format rather than binary
    so the server parses values the way it parses literals ("42" from MySQL
    loads into an int column). It reads `bytea` columns from the catalog so
    that `[]byte` goes in as hex only there.
  - Other engines: multi-row `INSERT` batches (500 rows, capped at 30000
    bind parameters), with the full-batch statement prepared once.
  - `canceled(ctx, err)` wraps a failure with `ctx.Err()`. On cancel pgconn
    closes the socket, so the error that surfaced was "use of closed network
    connection".
- `pgtext.go`: COPY text encoding (`\N`, escapes, NaN/Infinity, times with
  zone to the microsecond, bytea hex).
- `copy.go`: `Copy(ctx, src, table, dst, CopyOptions)` returns `CopyStats`.
  - Direct path (PG to PG, no Transform, no Args): the source's
    `COPY (SELECT …) TO STDOUT` is written into the destination Writer
    through `rawSink`. The row count comes from counting newlines, which
    also drives progress.
  - Row path: Reader, then Transform (nil skips the row), then Writer. The
    read has its own context and is canceled before `rd.Close()`. Without
    that, a destination failure made pgx drain the rest of the source
    (measured 487 ms against 6 ms on 2M rows).
  - `Create`: PG to PG of a table copies `format_type` types, NOT NULLs and
    the primary key from the catalog. Otherwise it uses broad types plus the
    source's key, read through `sourceKey`: SQLite `pragma_table_info`,
    `information_schema` on MySQL and bytdb. bytdb with no key fails with a
    hint. MySQL text key columns become VARCHAR(255).
  - `prepareDest` runs the CREATE inside the load's transaction on Postgres
    and SQLite, and just before it on bytdb and MySQL. bytdb refuses DDL in
    a transaction; MySQL would commit implicitly. MySQL's Truncate is a
    `DELETE` for the same reason.

### db, sdb, script

- `db.Manager.DriverOf(name)` gives the canonical driver of a connection,
  derived databases included.
- `sdb/etl.go`: `s.Copy(src, dst, table, sdb.CopyOpts)`, `s.Reader(conn,
  sql, args...)`, `s.Writer(conn, table, cols, sdb.WriteOpts)`, and type
  aliases `CopyOpts`, `CopyStats`, `Reader`, `Writer`, `WriteOpts`.
  `ProgressEvery` with no `Progress` callback prints
  `  src → dst: N rows`. `S.Release()` closes Readers and rolls back
  Writers the script left open.
- `script/engine.go` exports the new symbols to yaegi and defers
  `s.Release()` (it runs before the panic recover, so a panic also rolls
  back).
- `sdb.IsCanceled` also matches `context.Canceled`.
- `scripts/copy_table.go`: a sample script covering a whole-table copy, a
  filtered and transformed copy, and a join across connections with
  Reader/Writer.
- README: a new "ETL across connections" section.

## Bugs found while testing

1. **Primary key not carried over**: `indkey` is an `int2vector`, whose
   subscripts start at 0, so `array_position(indkey::int2[], …)` gave 0 for
   the first key column. Fixed with `unnest(indkey) WITH ORDINALITY`.
2. **The direct path was slower than the row path** (200k against 470k
   rows/s): `CopyTo` writes one row per `Write`, and each tiny pipe read
   became its own CopyData message and syscall. `writeRaw` now buffers to
   64 KiB. A 2M-row copy went from 9.9 s to 1.07 s; 500k rows end to end
   took 886 ms.
3. **Cancellation reported as a network error**: fixed with `canceled()`.
4. **A yaegi interpreter bug**: `m[k], _ = v.(string)` stores nothing and
   reports no error. Worked around in the sample and documented (N-127,
   memory `yaegi-map-commaok-bug`).

## Tests

- `etl/pgtext_test.go`: encoding, quoting, type families.
- `etl/copy_test.go` (no server needed): SQLite to bytdb with Create and the
  key carried over, Truncate re-run, Transform/skip/Where/Args/Columns and
  progress, rollback on a Transform error and on a duplicate key (SQLite and
  bytdb), Writer batching/short-row poisoning/Abort-after-Close, option
  errors.
- `etl/live_test.go` (opt-in `DBC_LIVE_PG_DSN`; creates `<db>_etl2` so there
  are two real connections). A mixed table of awkward types copies
  identically on the direct and row paths, with exact types and the key.
  Also covered: rollback on a CHECK failure on both paths, no table left by
  a failed Create, PG to SQLite to PG, and cancel of a 2M-row direct copy
  (table gone, no open transaction) followed by a full copy.
- `script/engine_test.go`: the API run through yaegi. That covers a closure
  as Transform, CopyOpts literals, a Reader feeding a Writer, and a dangling
  Writer rolled back by Release, including on a panic.
- The full suite passes; `-race` on etl and script with live Postgres
  passes. End to end through the `dbc script` binary with two Postgres
  connections: 500k rows copied with the md5 of all rows identical on both
  sides, and the cross-connection join produced correct regions.

## Git

Branch `todo/come-up-with-a-scripting-layer-t-1d2b`: `b648cf9` (ETL scripting
layer). Merged into local `main` with `--no-ff` (`7159131`) on top of
`5fbf3bf`. README auto-merged, and the full suite passes on main.

## Next

Closed: None. Declined: None. Raised: N-125, N-126, N-127.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
