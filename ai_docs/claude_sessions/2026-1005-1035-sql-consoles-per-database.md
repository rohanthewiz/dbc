# SQL consoles per database (TUI and dbc web)

Session: `d1cc5e4b-262f-473f-af17-06b8ddec3c28`

## Ask

From the cats-todo backlog: "Currently we are maintaining history under one
tab, but I want to maintain running sql files (consoles) under per (db host -
database)." Read as the TUI's single editor buffer (`~/.config/dbc/buffer.sql`).
Follow-ups during the session:

1. Commit and push, then do the same for `dbc web`: tabs follow the same
   per-database consoles.
2. Allow several query consoles per database.
3. Namespace (database + name) by host as well.

Two commits, both pushed to `main`: `2f23f59` (TUI, one console per
database, flat files) and `c9c6082` (several per database, host-namespaced
layout, `dbc web`).

## What a "database" is: `db.ConsoleTarget`

`db/target.go`. It resolves where a connection lands, not its name:

- Postgres: `pgx.ParseConfig` → `host:port` and the database. A derived
  `<conn>/<database>` uses `cc.Database`.
- MySQL: `mysql.ParseDSN` → `Addr` and `DBName`, with the same override.
- SQLite: the absolute file (`file:` and `?options` stripped). In-memory is
  `Host "memory"`, keyed by connection name.
- bytdb: host `local`, the absolute DSN path.
- Unreadable DSN or unknown driver: `Host ""`, connection name as the
  database. Consoles fall back to per-connection rather than being lost.

The user and the connection name are left out: `prod` and `prod-ro` on one
database share consoles. The port is kept: two servers on one machine are
different.

## Layout: `userdata/console.go`

```
~/.config/dbc/consoles/<host>/<database>/<name>.sql
  localhost_5432/app/console.sql, console-2.sql, reports.sql
  local/scratch.db-0b5e9f12/console.sql
```

- `ConsoleDBOf(host, db)` gives the two directory levels.
  - A plain host (`^[A-Za-z0-9][A-Za-z0-9.-]*(:port)?$`) only swaps `:` for
    `_`. Anything else becomes slug + 8-hex hash, so a literal `a_5432` can't
    collide with `a:5432`.
  - The database is kept verbatim when it is a valid file name. Otherwise it
    becomes the slug of its base name + hash (embedded paths, spaces, slashes).
  - Host `""` → `unknown` (a real host always has a port).
- `ValidConsoleName` (`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`, no trailing dot)
  guards every level that comes from the browser.
- `ListConsoles` orders `console`, then `console-N` by number, then others
  alphabetically. `NextConsoleName` returns the lowest free name.
- A console gets a file only when it has text (`SaveConsole` skips an empty
  one with no file). A console emptied on purpose keeps its empty file.
- `ConsoleRev` is a content hash (`""` = no file), so any writer moves it
  without a counter to keep in sync.
- The first commit's flat files (`<slug>--<slug>-<hash>.sql`) are moved in
  as `console` by `ListConsoles` (`FlatConsoleFile`, `adoptFlat`), never over
  an existing console.

## TUI: `tui/console.go`

- At startup the editor opens the default connection's first console.
- The swap happens in `connected()` when a connect lands, not when it is
  picked: a failed or canceled connect leaves the console where it was.
- On landing on a different database, the editor saves its console and
  opens the one last open on that database this run (`lastConsole`), else
  its first.
- ⌥N (`newConsole`) and ⌥C (`nextConsole`). The editor's right-click menu
  lists the consoles (`consoleMenuItems`), and the Query title shows
  `Query · <name>`.
- `consoleText` is the text as last loaded or saved. `saveConsole` writes
  only when the editor differs from it, so a console the TUI merely had
  open does not overwrite a browser tab's save.
- Legacy `buffer.sql` seeds the starting console once: only when the
  consoles directory does not exist yet, which is then created. The file is
  left in place.

## dbc web

Server (`web/consoles.go`):

- `Options.ConsolesDir` is set by `webcmd.go`. `""` turns consoles off
  (tests, no home), and tabs then keep their own buffers as before.
- `sideState.Console` (`consoleRef`: host, database, label, names) rides
  every `conn` event and `wsState` via the new `s.sidebar(ws)`. That is the
  page's cue to swap.
- Routes:
  `GET/PUT/DELETE /api/v1/consoles/:host/:db/:name`,
  `POST …/:name/rename`, `GET /api/v1/consoles/:host/:db`.
- PUT is compare-and-write under `consoleMu`:
  - base == the file's rev → written. A window-level `console` event
    `{host, database, name, text, rev, win}` goes to every window.
  - Otherwise `{conflict, text, rev}` comes back with a 200 (the error
    envelope can't carry data) and nothing is written.
  - The file's own text is never a conflict.
- Rename and delete broadcast `consoles` `{names, renamed | deleted}`.
- The window release (`pagehide`) also carries the shown console's unsaved
  text.
- `Tab.Console` (the name) is stored in a new `tab_consoles` table, not as a
  column on `tabs`. bytdb has `ALTER TABLE … ADD COLUMN` but no
  `IF NOT EXISTS`, and the schema is re-run on every open (probed this
  session).
- `GET /api/v1/tabs` and the boot claim return `savedTab` (the Tab plus
  `consoleDb`), so the page can show a tab's console before it connects.
- `moveTabBuffers` runs once at startup (layout marker `consoles.moved`).
  Each saved tab with a connection and text gets a console of its own:
  - It reuses a console whose file already holds that exact text.
  - Otherwise it takes the next free name, and never overwrites a file.
  - The tab keeps its buffer, so an older dbc still opens it.

Page (`app.js` "consoles" section, `editor.js`):

- Documents are keyed per console (`docOf`: `c:<host>/<db>/<name>`), so two
  tabs of one window on one console share a Monaco model.
- `cons` holds rev, saved and last-seen text per console. `saveConsole`
  sends one PUT at a time per console (`saving`/`again`), so a second save
  doesn't go out with a stale base.
- On a conflict, `adopt` loads the file's text with `replaceDoc` (one
  `pushEditOperations`, so Ctrl+Z brings the user's text back) and logs
  "was changed elsewhere". Another window's `console` event is adopted only
  when there are no unsaved local edits.
- `followConsole` is chained per tab. On a database change it saves the old
  console, then shows the first console of the new database that no other
  tab of the window shows, else a new name. A pre-console tab with text
  seeds a new console instead.
- The tab strip shows the console name (`.qcon`) after the title. The tab's
  right-click menu lists the consoles (and which tab shows each), with new
  (Alt+N), next (Alt+C), rename and delete. Alt+N and Alt+C are bound in
  Monaco and on the document.
- `editor.js` gains `docKey`, `docText`, `replaceDoc` and `renameDoc`.

## Verification

- `go vet ./...` clean, `gofmt -l .` empty. `go test ./...` passes with
  `CATS_*` stripped.
- New tests:
  - `db/target_test.go`
  - `userdata/console_test.go`: layout, escapes, ordering,
    rename/delete, flat adoption, revisions.
  - `tui/console_test.go`: follow, shared database, failed connect, legacy
    seed once, ⌥N/⌥C and lastConsole, unchanged console not written back.
  - `web/consoles_test.go`: console on conn and state, consoles off,
    revisions/conflicts including a TUI write, broadcast, path checks,
    rename/delete events, tab console stored, buffer move once, file-store
    round trip.
- `web/e2e` (`DBC_E2E=1`): all steps pass, plus a new
  `consolesPerDatabase` step in real Chrome:
  - lite → lite2 → lite brings the text back.
  - The files land under `consoles/local/lite.db-<hash>/`.
  - Alt+N gives an empty new console, and Alt+C wraps round.
  - A file rewritten on disk is adopted, not overwritten.
  - `modAlt` was added to the harness.
- `tui/e2e` (`DBC_TUI_E2E=1`): passes.

## Next

Closed: None. Declined: None. Raised: N-101, N-102, N-103, N-104.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
