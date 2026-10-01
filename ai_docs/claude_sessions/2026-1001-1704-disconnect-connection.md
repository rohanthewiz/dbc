# dbc web: disconnect a connection without removing it

Session: `c4a06961-4493-48f9-b06f-7c8434bf3678`

## Ask

"I want to be able to disconnect a connection without removing it." The
connections list's right-click menu offered only Connect, Edit…, Remove… and
Add a connection…. Leaving a connection meant picking another one.

## What changed

- **`workspace.Workspace.Disconnect`** (`workspace/connect.go`) leaves the
  active connection without picking another. Afterwards there is no active
  connection, no catalog, and no databases or schemas.
  - A connect still dialing is abandoned. `connGen` moves on, so it lands
    Stale. The sidebar's row counting and schema loading are canceled too.
  - It returns the connection left: the one being dialed, if there was no
    connection before it.
  - The Job closes the pinned session, rolling back an open transaction, as
    a switch does. It reads `active` when it runs, so a session pinned to a
    connection picked in the meantime is kept.
  - It is refused (`Busy`) while a run is in flight, rather than canceling
    it, and refused (`NoConnection`) when the tab is not connected.
  - The pool is not the workspace's to close, because the `db.Manager` is
    shared.
- **`db.Manager.Disconnect`** closes a pool and its derived ones, like
  `Drop`, and reports whether one was open. It leaves a shared in-memory
  SQLite pool open, because that is where the database's contents live
  (`demo-sqlite`).
- **`POST /api/v1/ws/:id/disconnect`** (`web/api.go`) does the following:
  - Sends a `"conn"` event with `active: ""` and status "disconnected".
  - Then, off the request, it closes the session. After that it closes the
    pool only if `hub.tabsOn(left) == 0`, counting tabs in every window,
    including tabs on the server's other databases.
  - While other tabs are still on the connection, the pool stays open and
    the log says so: "1 query tab is still on X: its connection stays open
    for it".
- **Page:**
  - The connections menu shows **Disconnect** in place of Connect on the
    row the tab is on or connecting to (`conns.js`).
  - `dbc.cmd.disconnect` asks first when the "session state" badge is up,
    in the same way that closing a tab does (`app.js`).
  - The header reads "not connected", muted (`.active-conn.none`).
  - The badge clears.
  - `activate()` no longer reconnects a reattached workspace whose
    `active` is "": a reload or tab switch keeps it disconnected. A new
    workspace, including one the server forgot after a restart, still
    connects as before.

## Decisions

- **Per tab, like Connect.** Disconnecting every tab on a connection would
  let one tab's gesture roll back another tab's open transaction. The
  connection to the server closes only when the last tab leaves it.
- **Refuse while busy** rather than cancel a run under its own session.
  Ctrl+K stops the run first.

## Verification

- **Tests:**
  - `TestDisconnectSparesInMemory` (db).
  - `TestDisconnect`, `TestDisconnectRefusedWhileBusy` and
    `TestDisconnectAbandonsConnect` (workspace).
  - `TestDisconnectKeepsTheConnection` (web): a second tab on the
    connection, the rollback note, the state, the list unchanged, a second
    disconnect refused, and a reconnect.
  - It adds a `stream.awaitLog` helper.
  - `go test -race ./...` passes with `CATS_*` stripped.
- **Browser (go-rod, scratchpad, headless Chrome):**
  - The active row's menu leads with Disconnect.
  - After it, the header reads "not connected", there is no active row, the
    tables list shows "no tables", and the status is "disconnected".
  - The menu then leads with Connect.
  - A reload stays disconnected.
  - Connecting to demo-sqlite from the menu works.
- **Not exercised:** a pool actually being closed against Postgres/MySQL
  (the tests use SQLite). The TUI has no Disconnect (N-083).

## Next

Closed: None. Declined: None. Raised: N-083.
Deferred: None. Promoted: None.
Updated: N-051, N-082. Full list: `ai_docs/todo/next-list.md`.
