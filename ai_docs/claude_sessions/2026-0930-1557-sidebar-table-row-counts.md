# Row counts in the sidebar's tables list

Session: `d7cd778a-a7f8-4954-a565-048b3f4981fd`

## Ask

From the user's cats-todo backlog: "When I list tables in a schema lazily
show me the row counts of each table. Like `table_a (100)` - on hover we
would say "table_a with n row(s)". Perhaps we can do some level of caching
here too like a couple mins or so."

## What was built

Each table in the sidebar gets its row count after the list has drawn, in
both UIs:

- **Web:** `cats (8)`, with the count in a muted span. The row's tooltip
  starts with "cats with 8 rows" ("1 row" when singular), then the existing
  gesture help on the next line.
- **TUI:** the count goes in the list row's right-aligned muted `sub` slot
  (`cats … 8`), where views already show "view". It is not appended to the
  label because a long label is truncated from its end, which would cut the
  count first. The terminal has no tooltip.
- Formatting (`db.RowCount.Short` / `Sentence`, so both UIs say the same):
  exact with commas below 100,000, then `123K` / `1.2M` / `2B`. Estimates are
  prefixed `~`, and their sentence says "about … (estimated from the
  database's statistics)".

### Counting rules (`db/rowcount.go`)

- **Postgres / MySQL:** one query reads per-table estimates
  (`pg_class.reltuples` for relkind `r`/`p`, `information_schema.tables.table_rows`).
  At or above `exactCountLimit` (1,000,000) the estimate is used. Below it,
  or when the estimate is unknown (reltuples -1, NULL), a `count(*)` runs.
- **SQLite / bytdb:** no estimates exist, so every table is counted exactly.
- Each count has `countTimeout` (3 s), and the whole counting has
  `countBudget` (30 s). A count that times out or fails falls back to the
  estimate, or to no number.
- Views, including Postgres matviews (which `TableRefs` marks as views), are
  never counted.
- Networked drivers use 4 workers. The embedded ones use 1, since a shared
  in-memory SQLite pool has only one spare connection (`memSQLiteMaxOpen`).
- `CountQuery` always quotes: backticks on MySQL, double quotes elsewhere,
  `schema.name` when there is a schema.
- Counting runs on the pool, never on the pinned session.

### Cache (`Manager.RowCounts`)

- Kept per connection name in the `Manager`, so every web tab shares it.
  `rowCountTTL` is 2 minutes, measured from when the counting started.
- A cached counting is used only if it was asked for every requested table
  (`asked`, not `counts`, is checked, so views and failed counts don't force
  a recount). A table created since triggers a fresh counting.
- Single-flight: a concurrent asker for the same connection waits for the
  counting in flight. A counting whose caller canceled it returns an error
  and is not cached.
- `Manager.Drop` (used when a connection is edited or removed) forgets the
  connection's counts. The cache has its own lock (`rowCounter.mu`), which
  nests inside `m.mu` in `Drop` and is never taken the other way round.

### Wiring

- `workspace.Connected.Counts` is a new follow-up Job, like `Release`. It is
  set when a connect lands with a catalog. Its event is `*workspace.RowCounts`.
  The counts are stored in the workspace (`Workspace.RowCounts()`) and reset
  along with the catalog.
- The next `Connect` cancels the counting in flight (`countCancel`), and so
  does `Stop`. Counts that land after another connect are `Stale`.
- TUI: `connected` batches `job(ev.Release)` and `job(ev.Counts)`.
  `refreshTables` now resets the cursor and calls `fillTables`, which the
  counts event calls on its own so the selection stays put.
- Web: `deliver` runs `ev.Counts` on a goroutine and sends a `counts` SSE
  event with the whole `[]tabRef`. `tabRef` gained `rows` and `rowsHint`, so
  the state a reattaching page reads has the counts too. `app.js`'s
  `setRowCount` / `showCounts` patch rows in place by qname, which keeps the
  selection and focus. Counts for a connection the tab has left are dropped.
- README: a paragraph under the sidebar intro.

## Verification

- `go vet ./...` and `go test -race ./...` pass.
- New tests:
  - `TestRowCountWording`, `TestCountQueryQuoting` and
    `TestRowEstimatesQueryPerDriver` cover formatting and the queries.
  - `TestRowCountsSqlite` and `TestRowCountsBytdb` count for real, with a
    quoted odd name and the view left out.
  - `TestRowCountsCache` covers the TTL hit, a recount for a new table, a
    recount after expiry, and Drop forgetting the counts.
  - `TestRowCountsSingleFlight` checks that every concurrent caller gets the
    same map.
  - `TestRowCountsCanceledNotCached` checks a canceled counting is not
    cached.
  - `TestConnectCountsRows` and `TestRowCountsStaleAfterSwitch` cover the
    workspace.
  - `TestTablesSidebarRowCounts` (TUI) checks the count is on screen and the
    cursor is kept.
  - `TestRowCountsFollowTheConnect` (web) checks the `counts` event and the
    state.
- Live tests were **run** against postgres:17 and mysql:8.4 containers, and
  all 17 passed. `TestLiveRowCountsPostgres` covers exact counts, a quoted
  name, the estimate path (faked by writing `pg_class.reltuples = 5e6`) and
  views/matviews left out. `TestLiveRowCountsMySQL` covers exact counts, a
  backtick name and a view left out.
- The page was checked in headless Chrome with a scratch go-rod program
  against the built binary (demo-bytdb). The row read `cats (8)` and its
  title was "cats with 8 rows\n…". The selection-kept-on-patch path is
  covered by the TUI test only; on the page it is a narrow window.
- A first full test run "failed" in `workspace` with
  `flag provided but not defined: -u`. That came from the
  `env | grep | xargs env -u …` pipeline used to strip `CATS_*`, not from
  the code. Use explicit `env -u CATS_ENV -u CATS_PANE_ID
  -u CATS_CONTROL_SOCKET -u CATS_SOCKET_PATH go test …`.

## Next

Closed: None. Declined: None. Raised: N-072, N-073.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
