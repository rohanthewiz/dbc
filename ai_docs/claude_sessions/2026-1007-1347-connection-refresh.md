# Refresh on the connection menu: re-read the current database's schemas

Session: `afec2b1a-2811-49f1-a8ae-136bf210aa41`

## Ask

"Please add a "refresh" menu item in the context menu of each connection,
that will attempt to re-read the schemas of the current DB."

## Decisions

No questions were asked up front. Judgement calls:

- **Both UIs.** dbc web's right-click menu on a connection row and the TUI's
  connections menu, with `r` in the TUI's Connections pane (next to `x`
  disconnect). The two UIs have been kept at parity throughout the project.
- **Refresh is a forced connect back to the active connection.**
  `SwitchPick` skips a connect to the connection already active with its
  catalog loaded. A connect that is not skipped already re-reads the
  databases, schemas and tables and drops the completion cache, so the
  refresh reuses that path rather than adding a second loader.
- **What it keeps:**
  - The session. The connection does not change, so there is no Release,
    and a transaction open on the pinned session stays open. This is what
    sets it apart from Disconnect then Connect, which would roll it back.
  - The schema pick (`refreshPickLocked`). `w.schema == ""` is ambiguous
    ("all schemas" picked, or a single-schema database), so the schema
    count tells the two apart.
  - The old list, if the tables cannot be read: stale beats empty. The
    whole refresh then lands nothing, not even the newly read schema list,
    so the sidebar stays one consistent picture. The exception is a driver
    with no catalog query, which never had a list; there the refresh lands
    as a connect does.
- **Row counts are re-counted.** `Refresh` calls
  `Manager.ForgetRowCounts`, so a sidebar with its counts on does not serve
  the 2-minute-cached numbers from before the change that prompted the
  refresh.
- **Refused** with no active connection (`NoConnection`) and mid-connect
  (`Busy`: the catalog is about to be replaced anyway). A connect started
  after a refresh supersedes it (connGen), and Ctrl+K cancels it like a
  connect.
- **Offered only on the tab's own row in the web.** The server refreshes
  the tab's connection. Other rows show Refresh greyed out with a reason
  ("Connect reads its schemas fresh"), following the menu's existing habit
  of showing Edit/Remove off with a reason rather than hiding them. On a
  derived connection ("ProdDr/analytics") the base row is marked, and
  Refresh re-reads the database the tab is on.
- **The check and the start share one lock hold.** `ConnectPick`'s locked
  half moved into `connectLocked`, so `Refresh` checks what is active and
  starts the re-read without releasing `w.mu` in between. Released, a
  connect to another connection could start in the gap, and the refresh
  would cancel it and pull the tab back.
- **TUI keeps the tables cursor on its table** (`relistTables`, by name)
  whenever a connect lands with `Changed` false, because a new table sorted
  above it would otherwise shift the cursor onto a neighbour.

## What changed

- `workspace/connect.go`
  - `Refresh`, `refreshPickLocked`, and `connectLocked` (the locked half of
    `ConnectPick`).
  - `landConnect(ev, gen, refresh)`: "refresh of X canceled/failed"
    wording, the keep-the-old-list branch, and a `refreshed X: 12 tables
    [in sales (of 4 schemas)]` note with status "refreshed"
    (`refreshedWhat`, `countOf`).
- `web/api.go`, `web/server.go`: `POST /api/v1/ws/:id/refresh`
  (`handleRefresh`). It announces `"connecting"` with `refresh: true`, so the
  page says "refreshing X…" and the row dials (Ctrl+K stops it). The
  outcome is the usual `"conn"` event, with `changed` false, so the results
  pane and the log stay put.
- `web/static/js/conns.js`: the Refresh row. `app.js`: `dbc.cmd.refresh`,
  the "refreshing" status, and the keys help.
- `tui/menu.go`: "↻ Refresh <conn>" under Disconnect, off with a reason
  mid-dial. `tui/app.go`: `r` in the Connections pane, plus the status-bar
  hint. `tui/run.go`: `refreshCatalog`; `connected` relists in place when
  `!Changed`. `tui/sidebar.go`: `relistTables`. `tui/help.go`: rows for `r`
  and right-click.
- `README.md`: the connections menu row, the `r` key, and the e2e list.

## Verification

- `go test ./...` (CATS_* stripped) is green, and gofmt is clean.
- New tests:
  - workspace: `TestRefreshRereadsTheCatalog` (a table created through the
    pool appears; the session stays pinned and stateful),
    `TestRefreshRecountsRows` (the cached 8 becomes 9),
    `TestRefreshRefusals`, `TestRefreshWithoutTablesKeepsTheList` (through
    `landConnect`), `TestRefreshPick` and `TestRefreshedWhat`.
  - web: `TestRefreshRereadsTheSidebar` (the connecting/conn events, logs,
    session kept, refused when disconnected).
  - TUI: `TestRefreshFromTheConnectionsPane` (`r` lists the new table, keeps
    the cursor on cats, and the menu row shows).
- e2e: new step "refresh a connection" in `web/e2e`. It creates and then
  drops a table from a separate `dbc` process and refreshes through the
  menu each time, and checks that the row the tab is not on shows Refresh
  off. The full suite passes in headless Chrome (27 steps; the Postgres
  schema-picker step was skipped with no `DBC_LIVE_PG_DSN`).
- **Not done:** a live Postgres run (N-150).

## Next

Closed: None. Declined: None. Raised: N-149, N-150.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
