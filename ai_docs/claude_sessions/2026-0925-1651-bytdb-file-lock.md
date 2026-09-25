# bytdb v0.18.0: rely on its file lock (N-049)

Session: 3b84a216-8144-4e45-8723-3b3d35269a42
Date: 2026-09-25

## Ask

Next-list item N-049: "bytdb (v0.18.0) now takes a file lock so update
accordingly." Before, bytdb v0.16.0 took no lock, so two dbc processes (two
TUIs, or a TUI and `dbc web`) could both open one bytdb file — the demo's
`demo.bytdb` included — and both write its WAL. `web.bytdb` guarded itself
with its own flock in `web/lock_*.go`.

## What bytdb v0.18.0 does

- The lock lives in btypedb v0.8.0 (`lock.go`): `btypedb.Open` takes an
  exclusive, non-blocking lock on a `<path>.lock` sidecar and holds it until
  Close; `bytdb.ErrLocked` is an alias of `btypedb.ErrLocked`, so
  `errors.Is` matches either. The sidecar is left on disk after Close by
  design. The lock is **not reentrant**.
- The stdlib driver opens the engine in `OpenConnector`, i.e. inside
  `sql.Open`, not lazily at the first query, and holds it for the
  `*sql.DB`'s life (released when the connector closes, not when idle
  connections close). Within one process it shares one engine per absolute
  path, so two pools on one file never contend in-process.

## The catch

`web/store.go` locked `path+".lock"` = `web.bytdb.lock` — the very sidecar
bytdb now locks. flock is per open file description, so the store's own lock
would have made bytdb refuse the store's own open. Just bumping the version
would have broken `dbc web`'s persistence.

## Changes

| Piece | Change |
|---|---|
| `go.mod` | bytdb v0.16.0 → v0.18.0 (btypedb v0.7.0 → v0.8.0, indirect) |
| `db/manager.go` | `open` maps `bytdb.ErrLocked` from `openPool` to new `db.ErrInUse` via `inUse(name, err)`: "bytdb file in use by another process (another dbc — a TUI or dbc web — most likely has it open): …". The hint is in the message, not a serr `detail` field, because the TUI log pane and demo warnings print `Error()`, which omits fields. DSN left out of the fields (can carry options); bytdb's error records the path. Nothing is cached on failure, so it opens once the holder lets go. |
| demos | No code change: `openDemos` already drops a demo that fails to open with a warning — its comment even anticipated "the bytdb file held by a running TUI". Now it fires with the in-use message. |
| `web/store.go` | Own lock removed (`Store.lock` field gone); a held file shows up at `sql.Open` as `bytdb.ErrLocked` and is mapped to the existing `errLocked` ("the store is in use by another dbc web"), keeping the memory-only fallback. Doc comment explains why the store must not lock the sidecar itself. |
| `web/lock_unix.go`, `web/lock_windows.go` | Deleted (would self-conflict). `golang.org/x/sys` stays — `clip/rich_windows.go` uses it. |
| tests | `db`: `TestBytdbFileInUse`, `TestSeedDemosDropsHeldBytdbDemo`. `web`: `TestStorePersistsAndLocks` reworked. All hold the file with a bare `bytdb.Open` as the "other process" — a second `OpenStore`/pool in-process would share the driver's engine and see no contention. |
| docs | `ai_docs/plans/web-ui.md` (Phase 2 notes, file table) updated; N-049 closed in `ai_docs/todo/next-list.md`. |

## Verified

- `go test -race ./...` green (CATS_* stripped); `GOOS=windows go build ./...`;
  gofmt, vet clean.
- Two real processes on one scratch HOME, with a `dbc web` running:
  - second `dbc web`: warns the `demo-bytdb` demo is unavailable (in use)
    and that tabs/layout won't be saved; serves on SQLite.
  - `dbc -c demo-bytdb "SELECT 1"`: fails with the in-use error.
  - `dbc "SELECT 1 AS x"`: warns, falls back to the SQLite demo, prints 1.

## Next

Closed: N-049. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
