# N-153: a same-database move that waits on itself gets a lock_timeout

Session: `52cdae52-30a6-4360-aec4-e7ab0981b406`

## Asks

1. N-153 from the cats-todo backlog. A same-database move whose load waits
   on the source's own uncommitted write hangs until canceled. Examples
   are an archive with a foreign key into the table being emptied, and
   rows moved back into the table they came from. A `lock_timeout` on the
   load within one database would turn the hang into an error.
2. "/sw" (sess-wrap).

## The cycle

Since N-144 a Postgres source's transaction commits only after the load
has. That order is what keeps a failed move's rows in the source, so it
cannot change. Within one database it means the two transactions can
wait on each other through this process, where Postgres's deadlock
detector cannot see it:

    read (src tx):  DELETE FROM t … RETURNING ──row locks on t, uncommitted──┐
    load (dst tx):  INSERT into a table with an FK to t,                      │
                    or the moved rows back into t (a key match) ◄──waits──────┘
    read:           commits only after the load does

## Found on the way: the read can be the side that waits

The first run of the new live test hung on one case: direct path, rows
moved back, with Truncate. There the Writer opens before the source's
`COPY TO` starts, so the load's `DELETE FROM t` (Truncate within one
database) takes the row locks first. The source's DELETE then waits on
the load, and the load waits for rows. A timeout on the load alone never
fires there, so the read gets one too.

    load (dst tx):  DELETE FROM t (Truncate) ──row locks on t, uncommitted──┐
    read (src tx):  DELETE FROM t … RETURNING ◄──waits──────────────────────┘
    load:           waits for rows from the read

The row path passed that case, but only by luck of ordering. pgx's
`Query` returns once the row description arrives, which can come before
the DELETE has finished, so the load's DELETE could race it there too.

## What changed

- `etl/copy.go`
  - `Copy` works out `same` once: both ends are Postgres, Truncate or a
    Query is in play, and `samePGDatabase` says yes. It passes `same` to
    `copyDirect`/`copyRows`. `loadOptions` now takes `same` instead of
    the connections, and a plain copy between two databases skips the
    lookup as before.
  - With a Query, `loadOptions` puts `pgSameDBLockTimeout()` first in the
    load's Setup, so it covers the CREATE and the Truncate's DELETE as
    well as the COPY. `sourceSetup` gives the read the same statement.
    `copyDirect` runs it after `pgPinOutput` in the source's transaction.
  - `pgSameDBLockTimeout` is `SELECT set_config('lock_timeout', '<n>ms',
    true) WHERE current_setting('lock_timeout') = '0'`. It is LOCAL to
    the transaction and only applies when no timeout is set, so one the
    user or their role already has is kept. `sameDBLockTimeout` (30s) is
    a var so the test can shorten it.
  - A table or view source never gets a timeout: it only reads, so it
    cannot cause the cycle.
  - `lockTimeoutHint` explains a 55P03 from such a copy in the error's
    text, wrapped with `%w` so `errors.As` still reaches the `*PgError`.
    It goes in the text rather than a serr `hint` field because serr's
    `Error()` drops fields, and a script shows only the text. The first
    test run caught that.
  - The `CopyOptions.Query` doc says what happens.
    `sdb/sdbapi/api.json` was regenerated to match; `TestAPIUpToDate`
    caught it.
- `etl/reader.go`: `Read` is now a wrapper around an unexported
  `read(ctx, c, query, setup, args…)`, which runs `setup` after
  `pgPinOutput` in the read's transaction.
- `etl/live_pg_edge_test.go`: `TestLivePGMoveRows` gains six
  same-database cases: an FK into the source, rows moved back, and rows
  moved back with Truncate, each on both paths. Each runs under a 20s
  deadline with a 1s timeout, and checks for the explanation, that the
  source still has 1–5 and the archive is empty, and that no transaction
  is left open. One more case checks that an existing `lock_timeout` is
  kept.

## Trade-off

In a same-database Query copy, a wait on some other session's lock now
also gives up after 30s instead of waiting forever. That is why the
message says "probably". Setting one's own `lock_timeout` overrides the
30s.

## Verification

- With the fix turned off, all six new cases hung to their 20s deadline.
  With it, each fails after the 1s timeout and the rows are kept.
- `go test -race ./etl` with live Postgres (postgres:17 in docker) passed,
  and so did `go test ./...`.
- A script through the built binary moved rows into an archive with an FK
  into the source. It failed after 30.1s with the new message, and
  afterwards the source had 5 rows and the archive 0.

## Next

Closed: N-153. Declined: None. Raised: None. Deferred: N-152 (the user's
edit, found uncommitted and committed with this doc). Promoted: None.
Moved: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
