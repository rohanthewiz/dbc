# A connect lands when the connection opens; its catalog is read after

Session: `cd7ec3bd-c0bd-460e-a4fb-d2d219c64ea1`

## Ask

Two questions, with a log from a production connection:

> How about we lazily try to read the schema instead of trying to read in
> the midst of the initial connection?
>
> And how about the concept of levels of introspection?

    14:58:06 connecting to ProdDr
    14:58:13 schema table counts unavailable, listing schemas without them:
             statement canceled: context deadline exceeded … op[list schemas], cause[timeout]
    14:58:13 connected to ProdDr

Seven seconds of "connecting…", five of them the schema summary running
into its budget, with the connection open the whole time. The first
question was taken as a request and built; the second was answered with a
proposal and left for the user's yes (N-311). Then `/sw`.

## What changed

One commit, `8ffc279`.

**The split (workspace)**

- A connect used to be one Job: dial, then the server's databases, the
  schema summary (falling back to names) and the tables of one schema,
  and only then "connected". `connectLocked` now lands a connect proper
  (`kindConnect`) on the dial alone. `landConnect` moves the workspace
  onto the connection with nothing listed and makes `Connected.Load`, a
  Job that reads the catalog (`readCatalog`, the three reads lifted out
  unchanged) and lands a new `*CatalogLoaded` event.

      ConnectPick ─► Job: dial ─► *Connected{Load} ─ the UI runs Load ─┐
                                   (active moved,                      ▼
                                    sidebar "loading…")   databases, schemas, tables
                                                                       │
                                   *CatalogLoaded ◄────────────────────┘
                                   (sidebar filled; Counts, Routines)

- `Workspace.CatalogLoading()` says the gap is open (`loadCancel` set).
  The Load job is made under the same hold of `mu` that lands the
  connect, so the workspace is never connected, listing nothing and not
  known to be reading.
- The load runs under the connect's own `connGen`. Whatever supersedes
  the connect supersedes it: a later connect, a Refresh, a relist, a
  Disconnect each bump the generation and cancel it
  (`cancelCatalogWorkLocked`), and it lands `Stale`.
- A refresh and the relist after DDL are unchanged: one Job, one
  `*Connected` with its catalog, the old list kept up until the new one
  is whole. Their dial is a cached pool, so there was nothing to land
  ahead of.
- While the catalog loads:
  - statements run (the point);
  - `Cancel` stops the run first, then a dial, then the catalog read
    ("canceling the catalog read of X…"). The read lands `Canceled` with
    whatever it had read, the connection stays, and a refresh reads again;
  - `Refresh` is allowed and takes the read over, for the connect's pick
    (`refreshPickLocked` returns `loadPick`);
  - `PickSchema` is refused as busy: there are no schemas to resolve the
    pick against, and a pick resolved against none reads every schema;
  - a run's DDL relists (`relistLocked`), since the load may have read
    the tables before the DDL landed;
  - `SwitchPick` to the same connection does nothing, as with a catalog
    already loaded.
- Ctrl+C in the TUI is unchanged: `CancelConnect` still stops only a
  dial, so a background catalog read does not get in the way of quitting.

**TUI**

- `connected` chains `ev.Load`; a new `catalogLoaded` handler logs the
  notes and fills the list (`relistTables`), then starts Counts and
  Routines. A tab connected in the background starts its Load at once,
  as it does a relist (`tabs.go`).
- The Tables pane's empty text is `loading tables…` while
  `CatalogLoading`, not `(none yet)` or `no tables`.

**dbc web**

- `deliver` runs `ev.Load` and sends the `*CatalogLoaded` as a second
  "conn" event. `sideState.Loading` marks the first; `connEvent.Loaded`
  marks the second, so the page keeps the status bar (a statement may
  have run since "connected").
- The page's list says `loading tables…`. `showSide` leaves the database
  and schema pickers undrawn while `side.loading`, and does not call
  `dbPicker.landed()` until the catalog arrives. Without that the first
  event hid the pickers (no databases yet) and settled a Tab-pick's
  keyboard handoff against a sidebar with no boxes — the e2e suite's
  "postgres schema picker" step failed on exactly this, and passes now.

**Docs**: a README paragraph ahead of "On Postgres the sidebar is loaded
a level at a time", and the Ctrl+K row.

**Tests**

- `workspace/lazycatalog_test.go`: nine SQLite tests of the gap (lands
  before its catalog, counting starts from the load, superseded by a
  connect / a disconnect / a refresh, Cancel stops the read, Cancel
  prefers a run, the refresh pick mid-load, DDL mid-load relists), and
  one opt-in live test, `TestLiveConnectLandsAheadOfABlockedCatalog`.
- The workspace tests' many `w.Connect(x).Job().(*Connected)` became
  `landed(w.Connect(x))`, a helper that runs the Load and folds its
  lists, notes and jobs into the `*Connected`.
- web: `stream.awaitCatalog` reads a connect's two "conn" events and
  asserts their order (loading, then loaded).
- tui: `TestTablesPaneSaysLoadingUntilTheCatalogLands`.
- `TestLiveNavigatorPostgres` counted section headers as tables, stale
  since the sections landed (N-308, failing before this session too); it
  counts names now and passes.

## Verified

- `go vet ./...`, `go test -count=1 ./...` with `DBC_LIVE_PG_DSN` and
  `DBC_LIVE_MYSQL_DSN` set (postgres:17, mysql:8.4 in Docker): all 33
  packages pass.
- `web/e2e` with `DBC_E2E=1` and the live Postgres: passes (84 s) after
  the picker fix; it failed once before it, which is how the fix was
  found.
- Not looked at by hand in a terminal or browser against a slow catalog:
  N-312.

## Notes

- The live test stalls the catalog with `LOCK TABLE
  pg_catalog.pg_namespace IN ACCESS EXCLUSIVE MODE` held by another
  session. Two things found on the way: a lock on `pg_class` stalls
  logins too, and under the `pg_namespace` lock pgx cannot run even
  `SELECT 1` — it prepares what it runs, and preparing resolves
  `search_path` through `pg_namespace` (`pg_stat_activity` showed it
  waiting on `Lock/relation`). So the live test shows the connect landing
  and the load queueing, and no statement run under the lock; the SQLite
  test covers that. A first version of the test called `run` under the
  lock and hung until stopped; every step is now awaited with a deadline.
- The log's 5s is still spent on such a database — after "connected"
  now, before the sidebar fills — on every connect, refresh and relist.
  Removing it is what N-311 is for.
- Pre-existing gofmt nits in `web/server_test.go` and `tui/app_test.go`
  were left alone.

## Next

Closed: None. Declined: None. Raised: N-311 (levels of introspection, the
proposal awaiting the user's yes), N-312 (hand checks of the load gap).
Deferred: None. Promoted: None. Moved: None. Updated: N-308 (the stale
`TestLiveNavigatorPostgres` count is fixed). Full list:
`ai_docs/todo/next-list.md`.
