# Share browser-added connections with every dbc (N-057)

Session: 152141f5-1378-4b43-be16-eb88ec0ff8b7
Date: 2026-09-25

## Ask

Next-list item N-057. Connections added in `dbc web` were invisible to the
TUI and to headless runs, which read only the config file. They lived in
`web.bytdb`, and a running `dbc web` holds that file's lock, so the TUI
couldn't open it alongside.

There were two options. (1) A dbc-owned TOML file,
`~/.config/dbc/connections.toml`. (2) A separate bytdb file, opened briefly
for each read or write. Option 2 follows the usual "use bytdb" default. But
every dbc start would then open an engine and contend for its lock with a
`dbc web` in the middle of a write, and the file wouldn't be human-readable.
**The user chose the TOML file.**

The user asked mid-session how passwords are handled. DSNs are stored as
typed, so a `${VAR}` stays unexpanded and each process expands it from its
own environment. An inline password is stored as typed, in a `0600` file
inside the `0700` `~/.config/dbc`, the same protection `config.toml` has. A
DSN never goes back to the browser. Two stronger options were offered and not
taken up yet: an inline-password hint in the form, and the OS keychain.

## Changes (`403094c`)

| Piece | Change |
|---|---|
| `config/saved.go` (new) | `SavedConn` (name, driver, dsn, ai_rows, added) as `[[connection]]` tables, so an entry can be copied to `config.toml`. `SavedFile()` → `~/.config/dbc/connections.toml`. `ReadSaved` (a missing file is none). `Config.LoadSaved(path, knownDriver)` merges and records warnings: a file it can't read is a warning, not a failure. `Config.MergeSaved` skips a name already defined, an unknown driver (the check is passed in, since config is a leaf package) or an entry with no name or driver. Only a merged entry's unset `${VAR}` is reported. `SavedStore` (`Add`/`Update`/`Delete`/`Get`/`List`) runs one locked read-modify-write per change. The lock is btypedb's `AcquireLock` sidecar (`connections.toml.lock`), polled every 10ms for up to 3s. The file is written to a temp file (0600 via `os.CreateTemp`), fsynced and renamed over the old one, under a header saying comments aren't kept. A file that fails to parse fails the edit instead of being overwritten. `Update` keeps the entry's place and its `Added`. With no home directory the store is memory-only. |
| `config/config.go` | The `Connection.Web` comment now points at the saved file. |
| `main.go` | `setup` calls `cfg.LoadSaved(config.SavedFile(), db.Driver-check)` after `addAdHocConn`, so the config file's names and `dsn` win a clash. Every command goes through it: TUI, headless, script, migrate, explain, web. |
| `webcmd.go` | Passes `Conns: config.OpenSaved(config.SavedFile())`. The description names both files. |
| `web/server.go` | `Options.Conns *config.SavedStore` (nil → memory). `Server.saved`. `New` no longer merges (the caller has); it runs `moveStoreConns`. |
| `web/conns.go` | `mergeSavedConns` is replaced by `moveStoreConns`, which copies each `web.bytdb` `conns` row to the file and merges it. A name the file already has just has its row dropped. If `Add` fails, the row is kept for the next start and still merged, as before. Add, edit and delete write `s.saved`. The file refusing a name the config allowed means another `dbc web` added it, which is a 409. An edit whose old name is gone from the file is a 404. A rename calls `store.RetagTabs`; if that fails it's a warning, not an undo. `unsavedWarning` → `unsavedConnWarning`, keyed on the saved store. A second `dbc web` (memory-only `web.bytdb`) still saves connections. |
| `web/store.go` | The `conns` table is now legacy, read once for the move. `Conn` and `UpdateConn` are gone. `RetagTabs(from, to)` has a memory twin. `SaveConn` is kept so tests can make an old-style store. |
| `web/static/js/conns.js`, `README.md` | The "Adding connections" paragraph covers the new file, every command seeing it, hand edits, `${VAR}` per process, `0600`, and the move. |
| `go.mod` | btypedb is now a direct dependency. |

## Verified

- New tests in `config/saved_test.go`:
  - round trip: order, `Added`, file `0600` and dir `0700`, the header, `${VAR}` unexpanded, no temp file left behind;
  - edits: duplicate, a rename keeps place and `Added`, rename clash, missing entry, delete twice, emptied file;
  - a broken file is not overwritten, and a reader gets a warning instead;
  - the memory-only store;
  - `LoadSaved`: a clash with the file, an unknown driver, a hand-duplicated name, no driver, an unset var only for merged entries, no file at all;
  - demo mode plus saved connections;
  - 16 concurrent writers over one path from separate stores lose nothing;
  - a writer waits on a held lock.
- In `web/conns_test.go`:
  - `TestStoreConnsMoveAtStartup`: rows moved in order with DSN as typed and `Added`; skipped ones kept in the file; a row the file already has is dropped; the table is emptied.
  - `TestConnAddClashInFile`: 409, and the config is rolled back.
  - `TestConnEdit` now runs on a real `connections.toml` in its persistent variant.
- `go test -race ./...` green (with `CATS_*` stripped); vet and gofmt clean.
- End to end with real binaries in a throwaway HOME:
  1. An old build's `dbc web` saved a connection into `web.bytdb`.
  2. The new headless run couldn't see it yet.
  3. The new `dbc web` moved it and added one with `${DBC_E2E_DB}`. The file was `0600`, with the var unexpanded.
  4. With `dbc web` running, headless runs on both worked.
  5. After a rename in the browser, the next headless run found the new name and not the old one.
  6. After a delete, the connection was gone.

## Notes

- A running `dbc web` reads the file only at startup (plus under the lock
  for each change it makes). A connection another `dbc web` adds shows up at
  the next start.
- Harness pitfall: in a non-interactive bash script, background jobs start
  with SIGINT ignored, so `kill -INT` doesn't stop a `dbc web` started with
  `&`. `set -m` didn't fix it here either. The stragglers were stopped with
  TERM by PID.

## Next

Closed: N-057. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
