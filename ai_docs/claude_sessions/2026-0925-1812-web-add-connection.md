# Add connections from the web UI, with a connection test

Session: d365ba10-4183-4447-a792-cdf835382f89
Date: 2026-09-25

## Ask

"Do we have a UI for adding a new DBConn on the Web UI?" There wasn't one:
only `GET /api/v1/conns`, a sidebar rendered from the TOML config, and
nothing in the codebase that wrote the config back. There were two options.
(1) Write new connections into the TOML file, which loses the user's comments
and layout because BurntSushi/toml doesn't round-trip them. (2) Keep them in
dbc web's own store. The user chose option 2, with a "Test Connection"
button.

## Changes (`b0ca1b5`)

| Piece | Change |
|---|---|
| `config/config.go` | `Connection.Web` (`toml:"-"`, set only by the store merge). `Connections` is guarded by `connMu sync.RWMutex`. `AddConn`/`RemoveConn` are **copy-on-write** (they build a new slice, so a snapshot or an in-flight `range` never sees an element change). `Conns()` returns the current slice, and `ConnByName` takes the read lock. `ErrConnExists`. `ExpandDSN(name, dsn)` was factored out of `Load`, and the store merge reuses it (an unset `${VAR}` → a named warning). |
| `db/manager.go`, `workspace/workspace.go`, `web/server.go` | The runtime readers of the list go through `cfg.Conns()`. The TUI still reads the field directly: nothing changes the list in a TUI process. |
| `db/probe.go` (new) | `Probe(ctx, cc, timeout)`: `driverFor` + `sqliteDSN` + `openPool`, a ping under the timeout, then close, with nothing cached. For a SQLite/bytdb file that doesn't exist it returns OK plus a note and doesn't open anything (opening would create an empty db at a mistyped path). A held bytdb file → `ErrInUse`. dbc's own timeout firing → "no answer within Ns — is the host reachable…". |
| `web/store.go` | `SavedConn{Name, Driver, DSN, AIRows, Added}` and a `conns` table (`ai_rows BOOLEAN`). `Conns` (oldest first), `SaveConn` (insert, no upsert), `DeleteConn`. Memory-only fallback map. The DSN is stored **as typed**, `${VAR}`s unexpanded. |
| `web/conns.go` (new) | `mergeSavedConns` runs in `New`, after the file's connections and the demos. A name the file now takes, or an unknown driver, is skipped with a warning (to the terminal and `cfg.Warnings`, which a new window's page logs); the entry stays in the store. `POST /api/v1/conns/test` returns 200 `{ok, ms, note, error, warnings}`, and 400 only for a form that can never work. `POST /api/v1/conns` calls `cfg.AddConn` first (the atomic duplicate check → 409), then the store (a failure rolls back with `RemoveConn`), then broadcasts `conns`. `DELETE /api/v1/conns/:name` (`PathUnescape`d) is 400 for a file/demo connection, 409 while any tab in any window is on it or connecting to it (`hub.tabsOn` uses `Active()` + `Connecting()`), and otherwise does `RemoveConn` + `mgr.Drop` + `DeleteConn` + broadcast. `hub.broadcast` sends a window-level event to every window. |
| `web/api.go` | `connInfo.Saved`; `handleConns` → `connList()`. |
| `web/pages/workbench.go` | The Connections heading gets a **+** (`#conn-add`). Web-added items get `data-saved` and a tooltip. `conns.js` was added to the script list before `app.js`. |
| `web/static/js/conns.js` (new) | `dbc.conns.draw` (the same markup as the server; carries active/connecting classes across by name) and the form: DSN placeholder per driver, Test (sequence-numbered so a stale answer is dropped, buttons disabled while a request is out), Save (Enter; Ctrl/⌘+Enter tests), then `dbc.cmd.connect`. A right-click menu (Connect / Remove… / Add…), where Remove on a file connection shows why not. A remove confirmation. |
| `web/static/js/app.js` | Handles the window-level `conns` event, refetches `/api/v1/conns` in `resync`, adds `connect` to `dbc.cmd`, and adds a "Sidebar" group to the key help. |
| `web/static/css/app.css` | `.hrow`/`.hadd`, the accent dot for `[data-saved]`, `.connform`, `.connresult.{ok,warn,err}`. |
| `README.md` | An "Adding connections" paragraph in the dbc web section. |

## Verified

- New tests: `config/conns_test.go` (add/remove, snapshot unaffected,
  concurrent add/read under `-race`, `ExpandDSN`), `db/probe_test.go`
  (existing file OK, missing SQLite/bytdb not created, held bytdb →
  `ErrInUse`, unknown driver, `embeddedPath`), `web/conns_test.go` (probe:
  memory, missing file, refused Postgres without the password in the error,
  unset var warning, 400s; add → config expanded / store as typed / no DSN in
  the response / `conns` event / 409 dup / connect / 409 remove while on it /
  remove / 400 config / 404; escaped name `my db/2`; startup merge with a
  clash and an unknown driver, and list order).
- `go test -race ./...` green (CATS_* stripped); vet and gofmt clean.
- Headless Chrome (go-rod scratch harness, 34/34, no console errors): the
  form, placeholder per driver, Test good / refused (no password) / missing
  file (not created), save with Enter → listed with the dot and the tab
  switched, duplicate refused in the box, window B lists it and A redraws
  from B's add, remove refused while on it and then done in both windows,
  the file connection's Remove says why, the key help, and it survives a
  server restart.

## Notes

- rweb gives a handler no request context, so a Test whose dialog closes
  runs to its timeout. The answer is just never read.
- Harness pitfalls, saved to memory: rod input to a **background** tab hangs
  (`MustActivate` first), and a fresh connect's status is "connected", not
  "ready on".

## Next

Closed: None. Declined: None. Raised: N-056, N-057.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
