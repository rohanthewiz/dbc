# TUI database/schema navigator and Disconnect, plus release fixes

Session: `97622ddc-e323-4e75-9a15-029039122b87`

## Ask

Do N-078 (the TUI has no database or schema picker) and N-083 (the TUI has
no Disconnect), "and whatever else in the Next list you would recommend to
include in the next release".

Picked for the release, beyond the two asked for:

- **N-082:** closing derived per-database pools. The new TUI database picker
  makes their growth worse.
- **N-073:** partitioned-table row estimates. The count timed out on exactly
  the tables that needed a number.
- **N-074:** `${VAR}` escaping in DSNs, a real parse failure.
- **N-080:** renames losing derived tabs and picks in dbc web.

Left out: N-072, N-075, N-079, N-081 and the verification-only items.

N-073, N-074 and N-080 went to three background agents in their own git
worktrees. Their commits were reviewed, then applied to main's working tree
with `cherry-pick -n`. The worktrees, branches and docker containers were
removed afterwards.

## What changed

### N-078: TUI navigator (`tui/navigator.go`)

- **Picker rows.** On a `db.Navigable` connection (Postgres), the Tables pane
  draws up to two rows above its list: `⛁ <database> ▾` (when the server
  listed its databases) and `◫ <schema> · N ▾` (when the database has more
  than one schema). Their rects are `layout.dbRow`/`schemaRow`, for
  hit-testing.
- **Opening a picker.** A click on a row, `d`/`s` in the Tables pane, or the
  table menu's new "Switch database…" / "Pick schema…" rows open a
  `pickModal`. That is a type-to-filter list (substring match, ignoring
  case) whose cursor opens on the row in use.
- **Databases.** Picking one is `setActive(conn)`. `conn` is the base for
  the DSN's own database (`db.DefaultDatabase`) and `<base>/<database>` for
  any other, the same mapping as the web's `sidebar()`.
- **Schemas.** "all schemas" comes first. Past `db.AllSchemasLimit` it is
  dim, and picking it says why. Each schema shows its table count, with the
  default marked. A pick is `Workspace.PickSchema`; the row reads
  "loading…" until `*SchemaLoaded` lands, which the TUI now routes.
- **Remembered picks.** The pick is remembered per connection for the run
  (`Model.schemaPicks`). `setActive` now uses `SwitchPick` with it, so going
  back to a database reopens its schema.
- **Connections list.** `refreshConns` and the connection menu mark the base
  row while the TUI is on a derived connection (`baseOf`).
- **No `WholeCatalog`.** The TUI no longer passes
  `Options.WholeCatalog: true`, so Postgres opens on the default schema as
  in the web. The option itself stays in `workspace`.
- **Table menu.** It opens off a row too: the table actions are then dim,
  with a reason, and the pickers stay live.

### N-083: TUI Disconnect

- **How to trigger it:** `x` in the Connections pane, or "⏏ Disconnect
  <conn>" as the first row of the connection menu (the toolbar chip and a
  right-click on Connections). The row is shown while the TUI is on or
  dialing a connection (`connOn`).
- **Confirmation:** a session that `Session()` reports stateful opens a menu
  first: "Stay connected" (the default cursor) or "⏏ Disconnect — roll it
  back".
- **`disconnectNow`:**
  - Calls `Workspace.Disconnect`.
  - Sets the status to "disconnected" and redraws the lists.
  - In a command, runs the release Job and then `mgr.Disconnect(left)`. The
    TUI's workspace is the only one on its Manager. An in-memory SQLite
    pool is still kept by the Manager.
- **When not connected:** the toolbar chip reads `○ not connected ▾`, the
  status bar ` ○ not connected`, and the empty Tables pane "not connected".

### N-082: close a derived pool once it is left

- **Workspace** (`workspace/connect.go`, `events.go`):
  - `Connected.Left` is the connection a changing connect moved away from
    ("" after a disconnect or on the first connect).
  - `Workspace.Derived(name)` says whether a name is `<conn>/<database>`.
- **Web** (`web/hub.go`): `deliver` runs the Release job, then `closeLeft`.
  - If the left connection is derived and `hub.tabsOn(left) == 0`, it calls
    `mgr.Disconnect` and logs "closed the connection to X: no tab is on it".
  - Configured connections' pools are never closed by a switch.
- **TUI:** `releaseThenClose` does the same. It skips the close if, by then,
  the sidebar is back on that connection or dialing it.

### N-073: partitioned-table estimates (`db/rowcount.go`, agent)

- The Postgres `RowEstimatesQuery` uses a recursive CTE over `pg_inherits`.
  A partitioned parent's estimate is the float8 sum of its leaf partitions'
  `reltuples`.
- Leaves that were never analyzed (-1) are left out of the sum. When no
  leaf has an estimate, the parent falls back to its own value.

### N-074: escaping `${VAR}` in DSNs (`config/dsnexpand.go`, agent)

- **Signature:** `ExpandDSN(name, driver, dsn)` now takes the driver, and all
  five callers pass it.
- **How it works:** references become NUL markers inside `os.Expand`. The
  marked DSN is then walked the way the driver's parser walks it, and each
  value is escaped for its spot:
  - **Postgres keyword/value, inside quotes:** `\` and `'` are backslashed.
  - **Postgres keyword/value, bare:** re-quoted when the value needs it.
  - **Postgres URL:** user, password, path and query values are
    percent-encoded.
  - **MySQL:** database and param values are encoded only where they would
    split.
- **Reviewed and sent back once.** The first version percent-encoded every
  `%`, which would have broken a password stored pre-encoded in the
  environment (`p%40ss`). Now a `%XX` that is already there is kept
  (THE %XX RULE in the file), so no config that works today breaks.

### N-080: renames in the web page (`web/static/js/app.js`, agent)

- `connRenamed` maps every name through `renamedConn`: `<old>/<db>` becomes
  `<new>/<db>`, unless the Connections list has a configured connection of
  that exact name (the server's `derivedFrom` rule).
- It moves tabs whether shown or not, `state.active` and every schema pick,
  in one layout write.
- It is idempotent by remembering the last rename applied. Comparing names
  is not enough for a rename like `a` to `a/b`.

### README

- The TUI navigator paragraph replaces "The terminal UI has no pickers yet".
- New Mouse rows (Connections right-click, the picker rows) and Keys rows
  (`x`, `d`/`s`).
- dbc web's Disconnect is now documented. It was missing.
- A line says that moving off a `<conn>/<database>` closes its connection.

## Decisions

- **The TUI opens on the default schema,** like the web, now that it has a
  picker. Before, it listed every schema up to 5,000 tables. Easy to revert.
- **Picker rows go inside the Tables pane** rather than a third sidebar pane.
  They take two rows, and the pane already owns the catalog.
- **Only derived pools are closed on a switch.** A configured connection's
  pool is one per Connections row, so it is bounded, and switching back to
  it should not dial again.
- **Disconnect confirmation is a menu,** reusing the menu system, rather than
  a new modal.

## Verification

- `go vet ./...` and gofmt are clean, and `node --check app.js` passes.
- `go test -race ./...` passes, with the live tests on (`DBC_LIVE_PG_DSN`
  against postgres:17, `DBC_LIVE_MYSQL_DSN` against mysql:8.4) and `CATS_*`
  stripped.
- **New tests:**
  - TUI: `TestDisconnectFromTheConnectionsPane`,
    `TestDisconnectAsksWhenTheSessionHoldsState`,
    `TestNoNavigatorOffPostgres`, `TestPickModalFilters`.
  - TUI, live: `TestLiveNavigatorPostgres`. It covers the picker rows, `s`
    with a filter, a click on the db row, the switch to a derived
    connection with the base row marked, the schema restored on the way
    back, the derived pool closed, and "all schemas". It fails with the
    pool close turned off.
  - Workspace: `TestDerived`, plus `Left` asserts in the switch and
    disconnect tests.
  - Web, live: `TestLiveDerivedPoolClosedWhenLeft` (two tabs on a derived
    connection; only the last one to leave closes it).
- **TUI frames,** rendered against live Postgres and checked by eye: the
  picker rows, the schema picker with counts and the default marked, and
  the connection menu with Disconnect first.
- **Agents:**
  - N-073: a live partitioned tree (~5M/~2M estimated). It fails on the old
    query.
  - N-074: 11 hard values through each DSN shape, parsed by
    `pgconn`/`mysql`, and a live Postgres connect with the password
    `q'uo\te @/#%`.
  - N-080: 34/34 headless-Chrome checks (go-rod, two databases, a server
    restart, a reload). The old page fails 12.
- **Not exercised:** the TUI navigator by hand in a real terminal (N-087),
  and a schema pick saved by a since-closed web window (N-085).

## Next

Closed: N-073, N-074, N-078, N-080, N-082, N-083. Declined: None. Raised: N-084, N-085, N-086, N-087.
Deferred: None. Promoted: None.
Updated: N-051. Full list: `ai_docs/todo/next-list.md`.
