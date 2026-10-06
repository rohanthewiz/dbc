# Opt-in, exact sidebar row counts (N-124)

Session: `96150f17-19d2-48bd-ab90-21cd2fcc3d79`

## Ask

This came from the user's cats-todo backlog. Stop showing table row counts
on every connect. Add a checkbox that turns them on, unticked by default.
Don't use the approximate count, which the user found far off. Use exact
counts from this query instead (it was given for one schema):

```sql
select table_schema, table_name,
  (xpath('/row/c/text()',
     query_to_xml(format('select count(*) as c from %I.%I', table_schema, table_name), false, true, '')
  ))[1]::text::bigint as row_count
from information_schema.tables
where table_schema = 'la_ea_temp' and table_type = 'BASE TABLE'
order by table_name;
```

## What changed

### db (`db/rowcount.go`)

- No estimates anywhere. `countRows` no longer reads `RowEstimatesQuery` or
  uses `exactCountLimit`, which is gone. Every count is a real `count(*)`.
- Postgres: the new `PgCountsQuery` is the user's query, generalised. It
  joins `unnest($1::text[], $2::text[])`, so it counts exactly the tables
  the sidebar lists, across schemas. `pgCounts` runs it. The query is all
  or nothing, so failures are handled like this:

```
pgCounts ─► ok ──────────────► counts
   ├─ ctx canceled ──────────► error (not cached)
   ├─ countBudget spent ─────► no numbers
   └─ other (no SELECT priv) ► per-table count(*) workers (as MySQL)
```

- MySQL, SQLite and bytdb: per-table `count(*)`, as before, but with no
  estimate stand-in.
- Limits: `countTimeout` 3 s → 30 s per table, `countBudget` 30 s → 2 min.
  Counting is asked for now, and cutting it off would hide exactly the big
  tables.
- Kept: `RowEstimatesQuery`, its tests, and `RowCount.Estimate`. The doc
  comments say the sidebar no longer uses them.

### workspace

- `showCounts` field, `ShowRowCounts(on) Job` and `RowCountsShown()`.
  `countsJobLocked` returns nil while the switch is off, which gates the
  counts after a connect, a schema pick and a recount in one place.
- Ticking on with tables listed starts a counting Job. Ticking off cancels
  the counting in flight, bumps `countGen` (so that counting lands Stale)
  and clears `rowCounts`.
- `RunDone.Wrote` is new. `recountLocked` now always calls
  `ForgetRowCounts`, so other workspaces on the Manager never get pre-write
  numbers from the cache, even when the writer's own counts are off.

### web

- A `rows` checkbox (`#row-counts` inside `label#row-counts-box.hchk`) sits
  in the Tables heading next to ERD. While a counting runs, the label pulses
  (`.counting`).
- `POST /api/v1/ws/:id/rowcounts {on}` → `handleRowCounts`. When it starts
  no counting (off, or nothing listed), it sends a `counts` event right away.
- `sideState.RowCounts` makes the box follow whichever tab is on screen.
  `countsEvent.On` was added. A `counts` event now goes out after every
  non-stale counting, failed ones too, so the pulse always clears.
- `deliver(RunDone)`: `ev.Wrote` (not `ev.Counts`) decides `recountOthers`.
- app.js keeps `t.counting` per tab. Ticking on sets it straight away; the
  response (when it reports no counting) or the `counts` event clears it.
  Background tabs handle `counts` too.

### TUI

- `#` in the tables list calls `toggleRowCounts`. There is also a "Show/Hide
  row counts" item on the tables menu, a help line, and `· rows` in the pane
  title while counts are on.

### README

- Rewrote the row-counts paragraph: counts are opt-in, exact, use one
  statement on Postgres with a per-table fallback, and have the new limits.
  Added a `#` row to the keys table.

## Tests

- Unit: `TestRowCountsAreOptIn` (workspace), `TestWriteRecountsOtherTabsWithTheWriterOff`
  (web). These tests now tick the switch first: `TestConnectCountsRows`,
  `TestRowCountsStaleAfterSwitch`, `TestRunThatWritesRecounts`,
  `TestRowCountsFollowTheConnect` (also checks that unticking clears the
  numbers), `TestWriteRecountsOtherTabsOnTheConnection` and
  `TestTablesSidebarRowCounts` (TUI `#` on and off, title). Full
  `go test ./...` passes.
- Live (postgres:17 and mysql:8.4 containers):
  - `TestLiveRowCountsPostgres`: a table with a faked `reltuples` of 5e6
    now counts as 2.
  - `TestLiveRowCountsPostgresPartitioned`: exact counts through the
    sub-partitions.
  - `TestLiveRowCountsMySQL`: passes.
  - New `TestLiveRowCountsPostgresFallsBackPerTable`: a role with INSERT
    only on one table makes `PgCountsQuery` fail, and `RowCounts` still
    counts the readable table.
  - All pass, as do the workspace/tui/web live tests.
  - `TestLivePooledCancelReachesServer` fails, and also fails with these
    changes stashed. Noted under N-097.
- e2e (`DBC_E2E=1`, also with the PG DSN): the `tables_sidebar` step checks
  that the box starts unticked with no count, that a tick shows `(3)` and
  clears the pulse, and that unticking removes the count. The whole suite
  passes. A screenshot confirmed the box sits neatly beside ERD.

## Next

Closed: N-124. Declined: None. Raised: N-124 (raised and closed here).
Deferred: None. Promoted: None.
Updated: N-097. Full list: `ai_docs/todo/next-list.md`.
