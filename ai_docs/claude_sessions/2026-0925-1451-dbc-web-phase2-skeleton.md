# dbc web, Phase 2: the `dbc web` skeleton

Session: 5a02e9c7-9601-4da3-8f71-da27a046e3cc
Date: 2026-09-25

## Ask

"Do phase 2" of `ai_docs/plans/web-ui.md`: the subcommand (`--listen`,
`--no-open`, `--secret`), the rweb server, security middleware, the serr →
envelope responder, the element shell, embedded assets, the SSE hub, cleanup
on Ctrl+C, the health endpoint, and `web.bytdb` for tabs and layout. Done
when you can pick a connection, type a query, run it, cancel a slow one, and
the auth tests (no token → 401, bad Host → 403, cross-site POST → 403) pass.

Mid-session the user narrowed the auth: "Don't go too crazy on the auth,
likely this will remain just a local tool" (saved as a memory for later
phases).

## What was built

### `dbc web` (`webcmd.go`)

`dbc web [--listen ADDR] [--no-open] [--secret S]` (`$DBC_WEB_SECRET`).
Opens every demo up front (`demoAll`, like the TUI), opens `web.bytdb`
(warning and memory-only when that fails), shares the TUI's history file,
prints the login link and opens the browser via `tui.OpenURL`. A
non-loopback `--listen` warns. The query-only flags are refused.

### `web/`

| File | Holds |
|---|---|
| `server.go` | `Options`, `New`, routes, `Run` (rweb handles SIGINT/SIGTERM and returns; then `Shutdown` stops and releases every tab within 5 s), default `127.0.0.1:8450` with a free-port fallback (only when no `--listen`), embedded `static/` with a content-hash `?v=`, `/theme.css` from `theme.Default()`, inline SVG favicon, the idle reaper loop (every minute) |
| `auth.go` | `guard`: security headers (CSP `'self'` only, no inline script or style; nosniff; `Referrer-Policy: no-referrer`; `X-Frame-Options: DENY`), Host check (loopback names or the bound address, right port), Origin check on non-GET, then cookie or Bearer. `/login?s=` → `HttpOnly` `SameSite=Strict` cookie (a random per-launch value; name carries the port) → 303 `/`. Public: health, login, static, theme, favicon |
| `hub.go` | per browser tab: a `workspace.Workspace` + an rweb `SSEHub` (256 buffer, 20 s heartbeat); `launch` runs a Start's Job on a goroutine plus a 250 ms ticker; `deliver` turns landed events into `log` / `busy` / `tick` / `run` / `conn` / `connecting` / `result` SSE events; reap: no stream for `conn_idle_timeout` → `ws.Close` (session released, workspace still usable), 24 h → forgotten (404) |
| `api.go` | health, conns (never a DSN), tabs, layout, open/state/events/result/connect/run/cancel; UTF-16 caret → byte offset |
| `respond.go` | `{success, data, error, stopped}`; `classify`: Refusal Busy → 409, other refusals → 400, `reqError` → its status, `db.ErrCanceled` → 200 stopped, else 500 (logged with `logger.LogErr`, user sees the serr user message or the core error text, never fields) |
| `store.go`, `lock_unix.go`, `lock_windows.go` | `web.bytdb` (`tabs`, `layout`), one connection, upserts; an advisory lock on `web.bytdb.lock` (flock / `LockFileEx`); memory-only fallback with the same API |
| `pages/` | `Workbench` shell (topbar, sidebar with server-rendered connections, editor, splitter, results, log, status bar), `ResultTable` (cells classed `num` / `null` from `Raw`, `max_display_rows` honored), `SignIn` notice |
| `static/css/app.css`, `static/js/app.js` | theme variables, grid layout, narrow-screen layout; one JS module: boot/reattach (workspace id in `sessionStorage`), EventSource switch, Ctrl+Enter / Ctrl+R run, +Shift run all, Ctrl+K stop, Tab indents, table click inserts the name, splitter with saved height, debounced tab autosave (+ `keepalive` on pagehide) |

### Elsewhere

- `db.Manager.SetMemoryPool(n)`: raises the in-memory SQLite pool cap for
  open and later pools (applied under `mu` when a pool is cached, so a race
  with an open is not lost); ignored below 3. dbc web asks for 16.
- `go.mod`: `github.com/rohanthewiz/rweb v0.1.31` (its `Host()` reports the
  real header, which the rebinding guard depends on).

## Decisions and findings

- **Auth kept small** at the user's word. Dropped from the plan: TLS and
  `--insecure`, the `/login` form, the `X-DBC-CSRF` header and `<meta>`.
  Kept: secret → cookie, Bearer, Host check, Origin check.
- **Results fetched, not streamed**: the `run` event carries `hasResult`, the
  page GETs `/api/v1/ws/:id/result`.
- **Bug caught by the tests**: `go s.deliver(t, st.Job())` evaluates the Job
  on the request goroutine, so a run held its POST open. Now
  `go func() { s.deliver(t, st.Job()) }()`; same for `Connected.Release`.
- **Badge renamed "session state"**: `Session.Stateful` errs toward yes and
  never clears (a `ROLLBACK` leaves it set), so "transaction open" would be
  false. The reattach state carries it too (read only when not busy, since
  `Session()` waits on the session lock).
- **bytdb takes no file lock** (v0.16.0): the plan's "the second process gets
  dropped with a warning" was wrong. `web.bytdb` got its own lock; the
  connection files are N-049. Seen live: a second `dbc web` on the same HOME
  opened `demo.bytdb` silently.
- **rweb SSEHub race**: `broadcastToClients` writes `hubClient.dropped`
  under the read lock. `-race` caught it; worked around with a per-tab
  `sendMu`, the real fix is in rweb (N-050).

## Verification

- `go vet ./...`, `gofmt -l .` clean; `go test ./...` green (CATS_* stripped).
- `web`: 17 tests against a real rweb server — health public; no token →
  401 (API and page); wrong Bearer → 401; Bearer works and no DSN leaks; bad
  Host → 403 (even on health); cross-site POST → 403, same-origin 200; login
  cookie HttpOnly + SameSite=Strict, not the secret, page loads with it; CSP
  and Referrer-Policy on every response; run → SSE → result table; caret
  under a multi-byte character picks the right statement, run all; refusals
  400/404; busy 409 then cancel → stopped, slot free again; session badge
  across runs and in the reattach state; idle release then forget; tabs and
  layout round trip; store persists across reopen and a held store falls
  back to memory; `classify` table; `byteOffset`. Green under
  `-race -count=5`.
- `db`: `TestSetMemoryPool`.
- End to end in headless Chrome (go-rod from `~/projs/go/rod`, scratch
  HOME): sign in through the link (the Strict cookie survives the redirect),
  Ctrl+Enter runs the caret's statement (8 rows), switch connection from the
  sidebar, a recursive CTE ticks then Stop → "stopped after 767ms", `BEGIN`
  raises the badge, a reload keeps workspace, badge and buffer, a restart
  restores the tab's connection and buffer from `web.bytdb`, 420 px has no
  horizontal scroll (topbar fixed to wrap), console clean. SIGINT printed
  "stopping: canceling runs and releasing sessions…" and exited; a second
  instance took a free port and ran memory-only with a warning.

## Docs

- `ai_docs/plans/web-ui.md`: Phase 2 marked done with its outcome table,
  decisions and verification; the bytdb-lock premise corrected.
- Memory: `dbc-web-auth-minimal` (the user's auth guidance).

## Next

Closed: N-045. Declined: None. Raised: N-049, N-050.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
