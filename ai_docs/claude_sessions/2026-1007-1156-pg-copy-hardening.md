# Script copy (etl) hardened against a live Postgres 17

Session: `93968aea-082a-4754-bc61-fd7d882ec8c0`

## Ask

> Pls do some more thorough testing of the script copy feature (I am mainly
> concerned with Postgres). Perhaps use postgres in docker

The feature is `s.Copy` / `s.Reader` / `s.Writer` in scripts (package
`etl`, wrapped by `sdb/etl.go`) and `dbc copy`, which runs through the same
`sdb.S.Copy`. A follow-up question asked how to run the same Postgres in
Docker, its credentials, and how to make another container and database.

## Setup

Throwaway `postgres:17` on host port 55432 (database `dbc`, user
`postgres`), the recipe in memory `live-db-tests`. The etl live tests make a
second database `dbc_etl2` on the same server, so source and destination are
two real connections. A `mysql:8.4` on 53306 joined for the final
regression run. Both containers ran with `--rm` and were stopped at the end.

The existing `TestLivePG*` (5 tests) passed first. Everything new is in
`etl/live_pg_edge_test.go`.

## What the probes found (all fixed)

1. **Session settings changed values silently.** The direct path hands the
   source server's COPY *text* to the destination unread. A source with
   `DateStyle = 'SQL, DMY'` writes 2024-01-02 as `02/01/2024`. A destination
   on the default MDY reads that as Feb 1, and a day over 12 fails the load
   (`date/time field value out of range: "15/03/2024"`).
   `extra_float_digits = 0` writes `0.30000000000000004` as `0.3`, and
   `IntervalStyle = sql_standard` changes how signs read. The row path had
   the same problem for every value pgx reads as text: date[] and float8[]
   arrays, ranges, intervals. A destination with odd settings was fine,
   because what we send is unambiguous.
   **Fix:** `pgPinOutput` (`etl/reader.go`) is
   `set_config('datestyle','ISO',true)`, `intervalstyle` `postgres`,
   `extra_float_digits` `3`, set inside a transaction. On Postgres,
   `etl.Read` runs its query in that transaction and commits when the
   Reader closes. That covers the row path, `describe` and scripts'
   `s.Reader`. `copyDirect` does `BEGIN` + pin + COPY TO + `COMMIT` on its
   checked-out connection. `'ISO'` alone sets only the output style, so
   the session's field order still applies to literals in the user's own
   `Where`. This was verified in psql.

2. **A copy within one database hung for good when the load emptied a
   table the read needed.** Cases: a table copied onto itself with
   `Truncate` (an in-place rewrite through a `Transform`), a `Query` or a
   view over the destination. The load's `TRUNCATE` holds ACCESS EXCLUSIVE
   until commit, the source read waits on it, and the load waits on the
   rows. Postgres can't see the cycle because it runs through our process.
   The probe proved it with a 5 s deadline, on both paths.
   **Fix:** `loadOptions` (`etl/copy.go`). When source and destination are
   the same Postgres database (`samePGDatabase`), Truncate becomes a
   `DELETE` in the load's Setup. "Same" means the same pool, or two pools
   whose `current_database()` and `extract(epoch FROM
   pg_postmaster_start_time())` match. The epoch is compared rather than
   text, because two sessions' TimeZones may differ. The read's snapshot
   doesn't see the uncommitted delete, so rewrites work. They are tested
   through one pool and two, for a table, a query and a view: 300k rows
   each, under a second. `dbc copy` still refuses a self-copy by name (no
   Transform, so pointless). Its comment no longer claims a hang.

3. **BC dates failed on the row path.** Go years count astronomically
   (year 0 = 1 BC), and `AppendFormat` writes `-0043-03-15`. Postgres
   rejects that as a malformed zone offset (`time zone displacement out of
   range`).
   **Fix:** `appendPGTime` (`etl/pgtext.go`) writes the BC year and adds
   ` BC`. It never rebuilds a time with the BC year, because the two
   numberings differ on leap years (1 BC is one). Also fixed: an offset
   with seconds (pre-standard-time local mean time, e.g. LA -07:52:58) now
   uses `Z07:00:00`, where `Z07:00` silently dropped the seconds. 5-digit
   years were checked too.

4. **`Create` from a Postgres `Query` wrote invalid DDL.** pgx's
   database/sql driver reports a type it doesn't register by OID, so
   `CREATE TABLE … "cash" 790` was a syntax error. The old comment
   assumed `""`. This covers money, timetz, enums, composites and
   extension types.
   **Fix:** `pgOIDTypeNames` looks the OIDs up on the source. pg_catalog
   types (and arrays of them) keep `format_type`'s name; user types fall
   through to `text`. `pgTypeName` maps any digit string, `record` and
   `unknown` to text. Unsized `bit` becomes `varbit` (bare `bit` is
   `bit(1)`), and `char` becomes `"char"` (pgx's `char` is the internal
   one-byte type).

5. **A `Query` ending in `-- comment`, or `; -- comment`, failed on the
   direct path.** `COPY (… -- c) TO STDOUT` comments out its own paren.
   `TrimSuffix(";")` missed a `;` followed by a comment.
   **Fix:** `copySource` normalizes the Query with `sqlsplit.Split`. One
   statement is required, and two now get a clear error. Both wrappers
   (`COPY (…\n)` and `describe`'s subquery) close the paren on a new line,
   which also covers a `Where` ending in a comment. A probe confirmed
   `sqlsplit` keeps `'it\'s; x'`, `E'…'`, `"a;b"` and backticks together.

6. **Go values a Transform hands back.** A `[]string` went out as
   `fmt.Sprint` (`[a b]`, a malformed array literal), and a
   `time.Duration`'s `String()` (`1.5µs`) is not interval input.
   **Fix:** `pgOtherText` / `pgPlainText` / `appendPGArray`:
   - Duration → `<µs> microseconds`
   - `driver.Valuer` → `Value()`
   - Stringer
   - pointers: `nil` is NULL
   - named byte slices (`json.RawMessage`) as bytes
   - slices/arrays → array literals: numbers and bools bare, everything
     else quoted with `\` and `"` escaped; nested slices are sub-arrays;
     a nil slice is NULL
   - maps/structs → JSON

   The common Reader types keep their non-allocating fast path.

## What passed as it was

- Direct path: every one of ~45 column types in the matrix, including
  enum, domain, composite, arrays, ranges, money, xml, tsvector and
  geometric types. Edge values: NaN/±Infinity, -0, min/max ints, `\.` lines
  in text, all 256 byte values, a 1 MB text and a 300 KB bytea.
- Quoted identifiers: mixed case, spaces, `"` in names, reserved words, a
  key in non-table order (`PRIMARY KEY ("select", "Id")`).
- Destination shapes:
  - generated column on the source (lands plain)
  - reordered destination with a default and an identity column
  - generated column on the destination: refused, with a clear error and
    nothing changed
  - FK-referenced table with Truncate: refused, both tables untouched; Copy
    never adds CASCADE
  - partitioned destination: rows routed
  - view, matview or partitioned parent as source
  - missing schema: clear error
- Failure part way through a 1M-row table, on both paths. A destination
  failing early stops in ~20 ms against 460 ms for the full copy, so the
  source isn't drained. A source failing mid-read, a Transform error or a
  short row, a cancel mid-way, a context already canceled: each leaves the
  old rows and no session active or in a transaction on either side.
- Progress counts are exact on the row path and land at or past each
  multiple on the direct path, with embedded newlines.
- 8 concurrent copies over shared pools under `-race`.

## End to end with the real binary

Fresh build, `HOME` and cwd in the scratchpad, `CATS_*` stripped, a
scratch `dbc.toml` with two connections (`pg1` → `dbc`, `pg2` →
`dbc_etl2`). Note the config key is `[[connection]]`, singular: the
plural form loads as "config has no [[connection]] entries". The
`dbc script` run covered:

- 500k rows direct, with progress lines
- a yaegi `Transform` closure returning a `[]string` (one element with a
  comma and quotes) and a map into jsonb
- a Reader feeding a Writer with a `time.Duration` and a nil-able
  `*string`
- an in-place rewrite on one connection

All correct. SIGINT mid-copy: `dbc copy` exits 130 ("copy canceled"); a
script catching `sdb.IsCanceled` exits 0. The destination kept only its
sentinel row both times. A full 4M-row direct copy into a keyed table took
6.2 s.

## Test pitfalls hit (in memory `live-db-tests`)

- A table over ¼ of shared_buffers is read by a **synchronized seq scan**
  that can start mid-table. "Fail at id 700" never fired, so the test
  counts rows instead.
- Recreating a table under the same name with different columns trips
  pgx's statement cache ("cached plan must not change result type").
  Each case gets its own table.
- Calling `livePG(t)` twice resets `etl_live` on both sides.

## Docs

- README "ETL across connections": Go values into Postgres, Truncate as
  DELETE within one database, the pinned settings, Query `;`/comments,
  query-path Create types, Reader text forms.
- The dbc skill's `scripting.md`, briefly.
- `CopyOptions.Truncate`/`Transform` doc comments, then `go generate
  ./sdb/sdbapi` (`api.json`; `TestAPIUpToDate` passes).

## Verification

- `go build ./... && go vet` clean, `gofmt` clean.
- `go test -race ./...` with both `DBC_LIVE_PG_DSN` and
  `DBC_LIVE_MYSQL_DSN` set: all 24 packages ok.
- `go test -race ./etl -run Live -v`: all 19 live tests pass (3 MySQL, 16
  Postgres, the 11 new ones included). The 2M-row direct copy still runs
  ~0.9 s without `-race`.

## Postgres in Docker (the follow-up question)

```sh
docker run -d --rm --name dbc-live-pg -e POSTGRES_PASSWORD=<redacted> \
  -e POSTGRES_DB=dbc -p 55432:5432 postgres:17
```

User `postgres`, database `dbc`, `127.0.0.1:55432`. With `--rm`, stopping
the container deletes it. For a separate instance, use another name and
port, `POSTGRES_USER`/`POSTGRES_PASSWORD`/`POSTGRES_DB`, and a volume
(`-v name:/var/lib/postgresql/data`). The env vars only apply on first
init. Inside a running container:
`docker exec … createdb -U postgres NAME`, or `CREATE ROLE … LOGIN` +
`CREATE DATABASE … OWNER …`. dbc reaches another database on the same
server as `conn/otherdb`.

## Files

- `etl/copy.go`: `copySource` via sqlsplit, `"\n)"` wrappers, the
  source pin in `copyDirect` (source COMMIT before the load's),
  `loadOptions`/`samePGDatabase`, `pgOIDTypeNames`, `pgTypeName`
  (OIDs, pseudo-types, `bit`, `"char"`), `isDigits`, doc comments
- `etl/reader.go`: `pgPinOutput`, the Postgres read transaction,
  Close commits/rolls back, and a failed commit at the end is the
  Reader's error
- `etl/pgtext.go`: `appendPGTime`, `pgTimeLayoutSecs`, `pgOtherText`,
  `pgPlainText`, `appendPGArray`
- `etl/pgtext_test.go`: BC/LMT/5-digit-year cases, `TestPGTypeName`,
  Go-value encodings
- `etl/live_pg_edge_test.go` (new): 10 live tests
- `copycmd.go`: self-copy comment
- `README.md`, `.claude/skills/dbc/scripting.md`, `sdb/sdbapi/api.json`

## Next

Closed: None. Declined: None. Raised: N-142, N-143, N-144.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
