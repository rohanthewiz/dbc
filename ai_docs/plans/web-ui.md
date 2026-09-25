# `dbc web`: a browser UI for dbc

Raised 2026-09-25. The ask: a web UI for dbc, started as `dbc web`.

This is a plan. **Phases 1 (the `workspace` extraction), 2 (the `dbc web`
skeleton), 3 (the real editor and grid) and 4 (explain) are done**
(2026-09-25); everything from Phase 5 on is not built yet.

## The one-paragraph version

`dbc web` starts a local web server and opens the browser on it. The page is
the TUI's workbench, rearranged for a bigger canvas: connections and tables on
the left, a SQL editor, a results grid, the plan view, the log, and the
assistant. It is built the way the author's other Go web apps are (gonotes,
herdr-web — see *House conventions*): **rweb** for HTTP and SSE, **element**
for server-side HTML, vanilla JavaScript and CSS embedded in the binary with
`go:embed`, **serr** for errors, and nothing fetched from the network at run
time. Most of the work is not the web
layer. It is **pulling the TUI's application logic out of `tui.Model` into a
UI-agnostic `workspace` package**, so the terminal and the browser share one
set of rules for running statements, pinned sessions, cancellation, history,
explain and the assistant, rather than two copies that drift.

## Goals

- **Everything the TUI does, in a browser.** Run the statement under the
  caret, run all, cancel, pinned sessions (`BEGIN` … `COMMIT` across runs),
  multi-statement runs, the results grid (sort, hide, resize, inspect),
  copy and export, history, tables, explain (tree, flame, insights), scripts,
  and the AI assistant with its data rules.
- **Things a browser does better.** A real rich-HTML clipboard everywhere (the
  Clipboard API writes `text/html` itself, so "copy for Teams" works over SSH
  too), file downloads for exports, several query tabs side by side, a larger
  plan graph, and links you can bookmark (`/q/<tab>`).
- **Local and safe by default.** It listens on loopback, and only a browser
  holding the launch token can use it. Nothing is exposed to the network
  unless asked for explicitly.
- **One binary, offline.** No Node toolchain to build, no CDN at run time; the
  plan page already proves the pattern.

## Non-goals (for this plan)

- A multi-user, hosted service with accounts and roles. `dbc web` is a local
  tool for the person running it. Serving a team is a later, separate design
  (see *Later*).
- Replacing the TUI. Both stay first-class; the `workspace` extraction exists
  precisely so neither is second-class.
- A JavaScript framework or a build step (React, Vite, npm). The house style
  is server-rendered HTML with small vanilla modules, and dbc's UI is a
  handful of panes, not an app platform.

## Decisions to take to the user

These are the forks where the choice changes the work. The recommendation is
first in each row.

| Fork | Recommended | Alternatives |
|---|---|---|
| Who it serves | **One user, on loopback, protected by a per-launch secret** (herdr-web's model) | LAN/team server with logins — a much larger security design |
| Shared logic | **Extract a `workspace` package the TUI and web both use** | Re-implement the run/session rules in `web/` (fast now, drifts forever) |
| Live updates | **SSE for server→browser streams + JSON `POST`s for commands** | One WebSocket per tab carrying both directions |
| SQL editor | **Monaco, vendored and embedded, the `<textarea>` kept as source of truth** — gonotes' pattern, so one editor stack across the author's apps | CodeMirror 6 (~170 KB gz against Monaco's ~4 MB, but a second editor to know); a bare `<textarea>` with a highlight overlay |
| Web-only state (tabs, layout) | **bytdb store at `~/.config/dbc/web.bytdb`**; history and chats keep using `userdata` so TUI and web share them | Everything in `userdata` JSON files; or everything in bytdb (then the TUI must migrate) |
| Default port | **Fixed default `127.0.0.1:8450`, falling back to a free port** (clear of gonotes' 8444 and herdr-web's 8421) | Always a random port (bookmarks break every launch) |

## House conventions (from gonotes and herdr-web)

The author's two rweb apps were surveyed so `dbc web` looks like a sibling,
not a stranger. Taken from them:

| Convention | Source | In dbc web |
|---|---|---|
| Pages are structs with `Render() string`, panels are `element.Component`s rendered with `element.RenderComponents` | gonotes `web/pages/landing/*` | `web/pages/workbench/{page,sidebar,editor,results,plan,chat,log,status}.go` |
| Assets under `//go:embed all:static`, served at `/static/*` by extension, `?v=N` cache busting, long cache for `vendor/` | gonotes `web/static.go` | same |
| Vanilla JS as small IIFE modules, one `app.js` core with a central `apiRequest` wrapper | gonotes `web/static/js/` | same — plus an SSE client in `app.js` |
| JSON API under `/api/v1/…`, REST-ish, literal paths before `:id`, `{success, data, error}` envelope | gonotes `web/routes.go`, `web/api/notes.go` | same envelope; `error` carries serr's user message, never the DSN |
| Monaco loaded lazily, vendored by `scripts/vendor_monaco.sh`, the `<textarea>` the source of truth | gonotes `monaco_editor.js` | same, but **no CDN fallback** (dbc works offline, and the CSP stays `'self'`) |
| A generated per-launch secret, an HMAC-signed `HttpOnly` `SameSite=Strict` session cookie, `Authorization: Bearer` for headless clients, a same-origin `Origin` check | herdr-web `internal/gwauth` | the auth design below |
| `/api/v1/health` reporting the data dir | gonotes | same — a macOS wrapper (Phase 6) polls it |
| Integration tests against a real server: `ReadyChan`, `Address: "localhost:"`, `GetListenPort()` | gonotes `web/api/notes_test.go` | the web test harness |
| Cleanup after `s.Run()` returns, relying on rweb's own SIGINT handling | gonotes `main.go` | release sessions, close chats, save state after `Run` returns |

Two deliberate departures: gonotes binds every interface with CORS `*` and
keeps its JWT in `localStorage` — right for its multi-user sync server, wrong
for a tool holding database credentials. `dbc web` binds loopback, sends no
CORS headers, and keeps its session in an `HttpOnly` cookie script cannot read.

## What already exists and carries over

dbc is in good shape for this: most of its logic already lives outside the
TUI, and the TUI's drawing is cleanly separated from its data.

| Package | Reused as-is by the web UI |
|---|---|
| `config` | connections, limits, AI settings; gains a `[web]` table |
| `db` | `Manager`, `Session` (pinned sessions, `Classify` fault rules), `Explain`, catalog (`TablesQuery`, `TableIndex`, `Columns`) |
| `sqlsplit` | statement splitting and the statement at a caret offset — the browser sends the caret, the server picks the statement, exactly as the TUI does |
| `export` | every format; `ClipContent` / `HTMLFragment` feed the browser clipboard; `ToFile` becomes a download |
| `explain` | plans, insights, `Text`, `JSON`, and the whole interactive plan page (`HTML`) |
| `ai` | `Chat` is already UI-agnostic: `Events()` becomes an SSE stream; `BeginSignIn` (device flow) is natural in a browser |
| `userdata` | history, saved buffer, conversation archive — shared with the TUI |
| `script`, `sdb` | scripts run unchanged; `s.Show` / `s.Print` callbacks become SSE events |
| `theme` | the palette becomes CSS custom properties, as the plan page already does |

What does **not** carry over is the logic that lives on `tui.Model` today —
the rules, not the drawing:

- the run slot: one run at a time, `runGen` stragglers dropped, cancel
  (`beginRun`, `endRun`, `cancelRun`, `run`, `runDone`)
- the pinned session per active connection, retry-once / session-lost rules
  (`onSession`, `runOnSession`, `releaseSessionCmd`, `dropSession`)
- connect with cancel and supersede (`connectCmd`, `connGen`)
- what `Ctrl+R` runs: caret statement, selection, run all (`stmtsToRun`,
  `allStmts`) and history recording
- `lastStmt` / `lastErr` / `lastRes` bookkeeping, and the assistant's context
  built from them (`chatContext`, the data rule, hidden columns, sort order)
- explain (`explainStmt`, `maybePlan`, `planForChat`)

That is roughly 1,500 lines across `tui/run.go`, `tui/explain.go`,
`tui/chat.go` and `tui/app.go`, entangled with Bubble Tea messages. Phase 1
moves it.

## Architecture

```
            browser tab (one "workspace")                          dbc web process
┌──────────────────────────────────────────────┐        ┌──────────────────────────────────────────┐
│ page shell (element, server-rendered)         │  GET   │ web/                                      │
│ ├ editor (textarea → Monaco)                  │◄──────►│  routes, auth middleware, SSR pages       │
│ ├ grid (virtualized, vanilla JS)              │  POST  │  JSON handlers ─┐                         │
│ ├ plan (the explain page's JS, as a module)   │  JSON  │  SSE streams ◄──┤                         │
│ ├ assistant                                   │        │                 ▼                         │
│ └ log                                         │  SSE   │ workspace/  (NEW — shared with the TUI)   │
│                                               │◄───────│  Workspace: run slot, pinned session,     │
└──────────────────────────────────────────────┘        │  connect, history, explain, chat context  │
                                                         │        │           │            │         │
            terminal                                      │        ▼           ▼            ▼         │
┌──────────────────────────────────────────────┐        │     db/ explain/ export/ ai/ userdata/    │
│ tui/ (Bubble Tea) ── also a Workspace client  │───────►│                                           │
└──────────────────────────────────────────────┘        └──────────────────────────────────────────┘
```

### The `workspace` package

A `Workspace` is one person's working state against the databases: an active
connection, its pinned session, the run in flight, the last statement, error,
result and plan. It has no UI types in its API.

*As built (Phase 1), one change from the first draft:* work is not reported on
a channel the workspace owns. A start method (`Run`, `Explain`, `Connect`,
`RunScript`) does the immediate bookkeeping — claims the run slot, records
history, bumps a generation — and returns a `Start{Notes, Job}`; the **Job** is
the blocking half, which the caller runs wherever its UI runs blocking work.
The Job **lands its own outcome** in the workspace (under the workspace's
mutex) and returns an `Event` describing it. The TUI wraps a Job in a
`tea.Cmd` and the event comes back to `Update` as the message; the web layer
will run it on a goroutine and write the event to the SSE stream. Mid-run
events (a script's `s.Show` / `s.Print`) go to an `Options.Sink` callback.
Why: Bubble Tea already is an event loop with its own way of running blocking
work, and the TUI's test harness drives it synchronously — a workspace running
its own goroutines would have turned every TUI test into a race against a
channel. Refusals ("busy — … (Ctrl+K stops it)") are a typed `*Refusal` error
carrying a `Reason` (Busy → 409, the rest → 400) and the log line.

```go
type Workspace struct { /* cfg, mgr, hist; mu: active, catalog, run slot, last*, plan; sessMu: sess */ }
type Editor struct { Text string; Caret int; Selection string }
type Start struct { Tag string; Gen int; Notes []Note; Job Job }
type Job func() Event // *Connected, *RunDone, *ExplainDone, *SessionReleased

func New(cfg *config.Config, mgr *db.Manager, hist *userdata.History, opt Options) *Workspace // opt.Sink: ScriptShow, ScriptPrint

func (w *Workspace) Switch(name string) Start / Connect(name string) Start // supersede by generation
func (w *Workspace) RunEditor(ed Editor, all bool) (Start, error)            // Ctrl+R / Ctrl+Shift+R
func (w *Workspace) RunStmts(stmts []string, tag string) (Start, error)      // record, then run
func (w *Workspace) ListTables() (Start, error)
func (w *Workspace) ExplainEditor(ed Editor, analyze bool) (Start, error) / Explain(stmt, where string, analyze bool)
func (w *Workspace) RunScript(path string) (Start, error)
func (w *Workspace) Cancel() (Note, string)          // run, else connect
func (w *Workspace) ChatContext(q string, ed Editor, view GridView) (ai.Context, []db.TableRef)
func (w *Workspace) Stop() / Close()                  // cancel in-flight work / also release the session
// readers: Active, Catalog, TableIndex, Busy, RunTag, Ticking, LastStmt, LastErr, LastResult, Plan, Session, Connecting
func Pick(ed Editor) / PickAll(text string) / StmtRange(text string, caret int) // pure statement picking
```

The rules move with their comments intact — "ONE RUN AT A TIME", "A PINNED
SESSION per active connection", "CANCEL REACHES THE SERVER" — and the TUI's
existing tests became the safety net for the move. *As built:* every TUI
assertion passes unchanged; the tests' reads of the moved fields became
accessor calls (`m.lastRes` → `m.ws.LastResult()`), and the two tests that
reached into the session itself (dead stateless session retried, dead
stateful session lost) moved to `workspace`, which now owns that code.

The grid's view of a result (sort order, hidden columns) stays with each UI —
it is presentation — and is passed in where a rule needs it (the assistant's
data rule, "copy what you see").

### The `web` package

```
web/
  server.go         NewServer(cfg, mgr, opts), routes.go: rweb server and every route
  auth.go           per-launch secret, HMAC session cookie, Bearer, Host/Origin checks
  hub.go            workspaces by ID; idle reaping; one SSE stream per workspace
  respond.go        the {success, data, error} envelope and the serr → HTTP mapping
  pages/workbench/  element page + components: sidebar, editor, results, plan, chat, log, status
  api/              run.go, result.go, ai.go, meta.go — JSON handlers
  static/           css/app.css, js/*.js, vendor/monaco (embedded, vendored by script)
```

Routes, all under the token middleware:

| Route | Does |
|---|---|
| `GET /` | the page shell (server-rendered with element) |
| `GET /login`, `POST /login` | the secret, for a browser that did not come in through the launch URL |
| `GET /api/v1/health` | liveness and data dir (unauthenticated, reveals nothing else) |
| `GET /api/v1/ws/:id/events` | the workspace's SSE stream: run progress, results, log lines, chat tokens |
| `POST /api/v1/ws` | open a workspace (a browser tab) → id |
| `POST /api/v1/ws/:id/connect` | switch connection |
| `POST /api/v1/ws/:id/run` | `{buffer, caret, sel, all}` → accepted; progress on SSE |
| `POST /api/v1/ws/:id/explain` | `{…, analyze}` |
| `POST /api/v1/ws/:id/cancel` | Ctrl+K |
| `GET /api/v1/ws/:id/result?from=&n=&sort=&hide=` | a page of rows — the grid is virtualized, so 50,000 rows never cross the wire at once |
| `POST /api/v1/ws/:id/copy` | `{scope, format}` → `{text, html}` for `navigator.clipboard.write` |
| `GET /api/v1/ws/:id/export?format=` | a file download (`Content-Disposition`) |
| `GET /api/v1/ws/:id/plan.html` | the plan as the existing interactive page |
| `POST /api/v1/ws/:id/chat` … `/stop`, `/model`, `/signin` | the assistant |
| `GET /api/v1/conns`, `/tables`, `/history?q=`, `/chats` | sidebars and pickers |

Every JSON response is `{success, data, error}`. A refusal that is not a
failure — "busy: statement 2/4 is still running" — is `409` with the reason in
`error`, the same words the TUI logs.

### Why SSE plus POST, not a WebSocket

Every live update flows one way — server to browser: tick, statement done,
result ready, log line, chat token. Commands are discrete requests with a
clear success or refusal ("busy — Ctrl+K stops it"), which is what HTTP
status codes are for. SSE reconnects by itself, passes through proxies,
is trivially testable with `httptest`, and rweb's `SSEHub` already does the
fan-out and heartbeats. A WebSocket would earn its keep only for
keystroke-level collaboration, which is a non-goal.

### Sessions, tabs and connections

Each browser tab is a **workspace** with its own pinned DB session — so a
`BEGIN` in one tab does not leak into another, exactly as two TUI windows
behave today. Idle workspaces release their session after
`conn_idle_timeout` (the setting that already exists) and are forgotten
after a day; a closed tab's SSE disconnect starts that clock.

Two resource limits this touches, both real today:

- **In-memory SQLite** caps its pool at `memSQLiteMaxOpen = 3` (anchor +
  one pinned session + one pooled). Two tabs pinning sessions would starve
  the pool. `dbc web` raises the cap (it is a constant for the TUI's shape,
  not a law) and says so in a comment.
- **bytdb** was assumed to hold a file lock. *Found in Phase 2: it does
  not* (v0.16.0 takes none), so `dbc web` and a TUI — or two TUIs, as today
  — can both open the same bytdb file, the demo included, and both write its
  WAL. dbc web's own `web.bytdb` takes an advisory lock of its own (see
  Phase 2's outcome); the connection files are a follow-up.

## Security

A page that runs arbitrary SQL with stored credentials must not be
reachable by anything but its owner's browser.

- **Loopback only by default** (`127.0.0.1`). `--listen 0.0.0.0:8450` is
  allowed but prints a warning, and refuses without TLS
  (`[web] tls_cert`/`tls_key`) unless `--insecure` is also given.
- **A per-launch secret, herdr-web's scheme.** `dbc web` generates a secret
  with `crypto/rand` (or takes `--secret` / `DBC_WEB_SECRET`) and opens
  `http://127.0.0.1:8450/login?s=<secret>`. The login trades it for a
  stateless HMAC-signed session cookie — `HttpOnly`, `SameSite=Strict`,
  `Secure` under TLS, a 24 h TTL — and redirects to `/`, so the secret leaves
  the address bar at once. The signing key lives only in the process: a
  restart signs everyone out and nothing is written to disk. A browser that
  lost the cookie gets `/login`, which accepts the secret the terminal
  printed; `curl` and scripts send `Authorization: Bearer <secret>`,
  compared in constant time.
- **DNS-rebinding guard.** The `Host` header must be the bound address or
  `localhost`; anything else is 403 — a malicious page cannot rename itself
  onto 127.0.0.1.
- **CSRF.** `SameSite=Strict` already withholds the cookie cross-site; as a
  second lock, state-changing requests must carry an `X-DBC-CSRF` header
  (from a `<meta>` in the shell) and a same-origin `Origin`, which a
  cross-site form cannot forge. No CORS headers are ever sent.
- **No new secret storage.** DSNs stay in the config file, env-expanded as
  today. The browser never receives a DSN, only connection names and drivers.
- **CSP** on every page: `default-src 'self'`, no inline script except the
  shell's JSON data block (hashed), `connect-src 'self'`, and
  `worker-src blob: data:` for Monaco's workers (gonotes needed the same).

## Errors and logging

Errors follow dbc's existing rule, which is serr's: **wrap with context at
every frame, log once at the top.**

- Every layer wraps with `serr.Wrap(err, "key", value, …)` — `workspace`
  adds `conn`, `statement` (`2/4`) and `tag`; `db` already adds `conn`, `op`
  and `driver_err`; handlers add `route` and `ws`. Nothing below the handler
  logs.
- One place turns an error into a response (`web/respond.go`). It logs it
  with `logger.LogErr(err, "request failed")` — every accumulated field in
  one structured line — and answers with the envelope. The browser gets
  `serr.UserMsgFromErr(err, …)`: the user message where a layer set one with
  `SetUserMsg`, otherwise the same text the TUI shows in its log
  (`serr.StringFromErr` without the frame context), never raw fields a DSN
  could hide in.
- Status comes from the error, not the handler: `db.ErrCanceled` → `200` with
  `stopped: true` in the envelope (a stop is the user's choice, shown as
  stopped, not failed), a busy run slot → `409`,
  a bad request → `400`, an unknown workspace → `404`, anything else → `500`.
  A test pins the mapping.
- Errors on the SSE stream carry the same user message, so a failed statement
  reads the same whether it arrived as a response or an event.

## The frontend

Server-rendered with element: the shell, sidebars, dialogs and empty states
arrive as HTML. JavaScript owns only what has to be live — the editor, the
grid, the plan graph, the chat stream. Small IIFE modules, no bundler, as in
gonotes:

| Module | Responsibility |
|---|---|
| `app.js` | boot, the SSE connection, the command bus, keyboard map (the TUI's keys: `Ctrl+R`, `Ctrl+Shift+R`, `Ctrl+X`, `Ctrl+K`, `Ctrl+E`, `Ctrl+P` …, with browser-safe fallbacks noted where the browser reserves a chord) |
| `editor.js` | the `<textarea>`, upgraded to Monaco (SQL mode) once it loads; reports caret and selection offsets with each run; marks the statement that will run, as the TUI's gutter does |
| `grid.js` | virtualized rows fetched by page; sort, hide, resize, select a range; double-click inspects |
| `plan.js` | the explain page's graph, flame and insights, refactored out of `explain/assets/plan.html` into a module that page and the web UI both load |
| `chat.js` | transcript, streaming answer, `⤓ insert` into the editor, the context chip |
| `clip.js` | `navigator.clipboard.write` with both `text/plain` and `text/html` |

The look is the TUI's: the `theme` palette as CSS variables, the same
glyphs, the same layout — so a user moving between terminal and browser is
never lost.

## Phases

Each phase ends shippable and tested.

### Phase 1 — extract `workspace` (no UI change) — ✅ done 2026-09-25

Move the run slot, pinned session, connect, statement picking, history
recording, last-result bookkeeping, explain and chat-context building from
`tui` into `workspace`, behind the event API above. The TUI becomes a client
of it. **Done when** every existing `tui` test passes unchanged and the live
session tests (`db/live_*`) pass, with `workspace` unit tests for the rules
themselves (straggler drop, busy refusal, cancel, session lost, retry once).

*Outcome:* `workspace/` (connect, run, explain, session, chat context, events,
statement picking) with 17 tests; `tui/run.go` is now glue (turn keys into
calls, run Jobs as commands, draw events); `userdata.History` gained a mutex
so several web workspaces can share one. All tests pass, also under `-race`.
The live `db/live_*` tests pass against Postgres 17 and MySQL 8.4 (13/13).
To run them:

```sh
docker run -d --rm --name dbc-live-pg -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=dbc -p 55432:5432 postgres:17
docker run -d --rm --name dbc-live-my -e MYSQL_ROOT_PASSWORD=pw -e MYSQL_DATABASE=dbc -p 53306:3306 mysql:8.4
# wait until both accept connections, then:
DBC_LIVE_PG_DSN='postgres://postgres:pw@127.0.0.1:55432/dbc?sslmode=disable' \
DBC_LIVE_MYSQL_DSN='root:pw@tcp(127.0.0.1:53306)/dbc' \
go test -count=1 ./db -run Live -v
docker stop dbc-live-pg dbc-live-my
```

### Phase 2 — `dbc web` skeleton

The subcommand (`--listen`, `--no-open`, `--secret`), the rweb server, the
security middleware, the serr → envelope responder, the element shell,
embedded assets, the SSE hub, cleanup on Ctrl+C, the health endpoint, and a
`web.bytdb` store for tabs and layout.
A run returns a plain HTML table. **Done when** you can pick a connection,
type a query, run it, cancel a slow one, and the auth tests (no token → 401,
bad Host → 403, cross-site POST → 403) pass.

*Outcome (✅ 2026-09-25):* `dbc web [--listen ADDR] [--no-open] [--secret S]`
(`webcmd.go`), package `web/`:

| File | Holds |
|---|---|
| `server.go` | `Options`, `New`, the routes, `Run` (rweb's own SIGINT handling, then `Shutdown`), port 8450 with a free-port fallback, embedded assets with a content-hash `?v=`, `/theme.css` from the `theme` package, the idle reaper loop |
| `auth.go` | the guard middleware: Host check, Origin check on non-GET, session cookie or Bearer; `/login?s=` → cookie → 303 `/`; CSP and friends on every response |
| `hub.go` | one `workspace.Workspace` + one rweb `SSEHub` per browser tab; `launch` runs a Start's Job on a goroutine with a ticker; `deliver` turns landed events into `log`/`busy`/`tick`/`run`/`conn` SSE events; idle release and forget |
| `api.go` | the JSON handlers, the UTF-16 caret → byte offset conversion |
| `respond.go` | the envelope and `classify` (Refusal Busy 409 / other 400, `reqError`, `db.ErrCanceled` 200 stopped, else 500 logged) |
| `store.go`, `lock_*.go` | `web.bytdb`: tabs (buffer, connection) and layout (editor height), with a memory-only fallback |
| `pages/` | the element shell, the result table, the sign-in notice |
| `static/` | `app.css`, `app.js` (one module: boot/reattach, SSE switch, keys, splitter, autosave) |

Decisions and departures, each recorded here so later phases build on the
real thing:

- **Auth, kept small at the user's word** ("likely this will remain just a
  local tool"): per-launch secret → an `HttpOnly` `SameSite=Strict` cookie
  (a random per-launch value, not an HMAC-signed expiry; a restart signs
  everyone out), Bearer for scripts, Host check (DNS rebinding) and Origin
  check (cross-site POST). **Dropped from the Security section:** TLS and
  `--insecure` (a non-loopback `--listen` just warns), the `/login` form (a
  browser without the cookie is told to use the printed link), the
  `X-DBC-CSRF` header and its `<meta>`. The cookie name carries the port, as
  cookies are not port-scoped and two instances would sign each other out.
- **No inline script at all**, so the CSP needs no hash: the page reads
  nothing from the shell but the DOM. Console clean in Chrome.
- **Results are fetched, not streamed:** the `run` event says
  `hasResult`, and the page GETs `/api/v1/ws/:id/result` (rendered with
  element). A page that missed the event can still catch up; Phase 3's
  paged grid extends the same route.
- **Reattach:** the tab keeps its workspace id in `sessionStorage`, so a
  reload rejoins the same workspace — same pinned session, same open
  transaction. A 404 (forgotten, or a restart) opens a new one.
- **The badge is "session state", not "transaction open":**
  `db.Session.Stateful` errs toward yes (any non-read sets it, nothing but a
  new session clears it), so "transaction open" would lie after a plain
  `UPDATE` or a `ROLLBACK`.
- **Idle rules:** a tab with no stream for `conn_idle_timeout` has its
  session released (`ws.Close`, still usable after); after 24 h it is
  forgotten. Checked once a minute.
- **In-memory SQLite:** `db.Manager.SetMemoryPool(n)` raises the cap for
  open and future pools; dbc web asks for 16.
- **Found on the way:** rweb v0.1.31's `SSEHub` updates each client's drop
  counter under its *read* lock, so concurrent broadcasts race (`-race`
  caught it). Worked around with a per-tab send mutex; the fix belongs in
  rweb. And bytdb takes no file lock (see *Sessions, tabs and
  connections*); `web.bytdb` has its own flock/`LockFileEx` on
  `web.bytdb.lock`, and a second instance runs memory-only with a warning.

Verified: 16 `web` tests against a real server (the three auth tests,
Bearer, the login cookie, CSP on every response, run → SSE → result, the
caret with a multi-byte character, run all, refusals 400/404, busy 409 then
cancel → stopped, the session badge across runs and reattach, idle release
then forget, tabs/layout round trip, store persistence and lock) plus the
status-mapping table, all green under `-race -count=5`. End to end in
headless Chrome via go-rod: sign in through the link, run with Ctrl+Enter,
switch connection, stop a slow query, reload keeps workspace, badge and
buffer, 420 px has no horizontal scroll, console clean; SIGINT released the
open session and exited; a second instance fell back to a free port and a
memory-only store.

### Phase 3 — the real editor and grid — ✅ done 2026-09-25

Monaco with SQL highlighting and the TUI's keys; the statement-under-caret
marker; run all; multi-statement progress on SSE. The virtualized grid with
paging, sort, hide/show, resize, range select, inspect. Copy in every format
(rich HTML via the Clipboard API), export as downloads, history picker,
tables sidebar with preview. **Done when** the TUI's results-grid tests have
browser equivalents (see *Testing*).

*Outcome:*

| Piece | Where | As built |
|---|---|---|
| Editor | `static/js/editor.js`, `scripts/vendor_monaco.sh` | Monaco 0.52.2 (gonotes' version), vendored **trimmed** to what a SQL editor loads — loader, `editor.main`, the editor worker, the codicon font, the sql/mysql/pgsql tokenizers: 4.2 MB, not 14. The textarea stays the source of truth (mirrored on every edit); if Monaco fails, the textarea stays and the log says so. Dialect follows the driver; palette from `/theme.css` |
| Marker | `POST /api/v1/stmt` | the statement under the caret, found by the server's splitter (the one a run uses), in UTF-16 units; a gutter bar, debounced 150 ms |
| Grid | `static/js/grid.js`, `web/grid.go` | virtualized both ways (rows and columns); pages of 200 fetched with the sort; sort / hide / show / resize / fit / range / inspect, the TUI's gestures and keys (`y` `Y` `-` `+` `=` `g` `G` `Enter`, arrows, Shift extends) and words; hidden columns and hand-set widths survive a rerun with the same columns |
| Copy / export | `POST …/copy`, `GET …/export` | the page names the view (seq, sort, columns, rows); the server projects it and renders any export format. HTML goes on the clipboard as a real `text/html` flavor. A copy against a replaced result is a 409 |
| History | `GET /api/v1/history?q=` | Ctrl+P: the shared file, newest first, filtered; Enter inserts at the caret, never runs |
| Tables | `POST …/preview` | click selects, double-click / Enter previews `SELECT * … LIMIT 100` (recorded, editor untouched), right-click inserts or copies the name; the name must be one the catalog listed |
| Progress | `workspace.RunProgress` | a multi-statement run's status reads "all 4 statements · 2/4 1.3s", on the tick events and in the TUI's status bar alike |

Decisions and findings:

- **The view's arithmetic is shared, the view is not.** `workspace.SortRows`
  and `workspace.Project` are the TUI grid's sort and copy, moved; the TUI
  grid now calls them, and so does `web/grid.go`. The sort, hidden columns
  and range stay in each UI and travel with the request.
- **Results are paged JSON now**, not server-rendered HTML
  (`pages/results.go` is gone): the grid needs cells, the NULL flag (JSON
  `null`, read from `Raw`) and widths, not markup.
- **CSP: `style-src` gained `'unsafe-inline'`.** Monaco writes `<style>`
  elements, and the grid (and Phase 4's plan view) place cells with style
  attributes. `script-src` stays `'self'`, and the worker is a same-origin
  bootstrap file rather than a `data:` shim, so no `worker-src data:`.
- **Mac chords:** Monaco's `CtrlCmd` is ⌘, and macOS's emacs keys own the
  real Ctrl (Ctrl+P is cursor-up, Ctrl+K kills the line), so every chord
  is bound to both. Ctrl+X explains only with nothing selected; with a
  selection it is cut.
- **Double-clicks are counted by the grid**, per cell, not left to the
  browser: a press redraws the rows under the pointer, and a click whose
  press and release land on different nodes is no click to the browser.
- **Found by the browser run, fixed:** the column-resize handle straddled
  the header cell's clipped edge (half of it sorted instead); widths from a
  canvas-measured character were short (canvas does not resolve
  `ui-monospace`) — now measured in the grid's own font.
- Deferred: "✦ ask the assistant about this result/value" (Phase 5, with
  the chat pane).

Verified: `web` tests for every endpoint above — sort (numbers as numbers,
NULLs last both ways, the third state), pages and `max_display_rows` (the
cap bounds drawing, not a whole copy), range copy in display order with
`Raw` carried (NULL stays NULL) and a hidden column left out, the words
("2×2 cells as CSV", "the result (5 rows, 1 column hidden) as Markdown"),
the HTML flavor, stale seq → 409, bad requests → 400, export headers and
content for every format, history filter, preview (and its refusal of a
name the catalog did not list), the UTF-16 marker range, and the "· 2/3"
progress on the stream; `workspace` tests for `SortRows`, `Project` and
progress. All green, `-race` too. End to end in headless Chrome (go-rod):
Monaco loads with a clean console under the CSP, the marker follows the
caret, Ctrl+Enter from Monaco, header sort cycles, shift-click range →
right-click → CSV on the real clipboard, `y` / `Y`, the HTML flavor holds a
`<table>`, double-click inspects (JSON formatted), border drag resizes and
double-click fits, hide from the header menu survives a rerun, `-` refuses
the last column and `+` restores, Ctrl+P filters and inserts without
running, table double-click previews, Ctrl+E → CSV downloads, 1,000 rows
keep ~40 in the DOM, 420 px has no horizontal scroll.

### Phase 4 — explain, with everything the HTML plan page already does — ✅ done 2026-09-25

A Plan tab beside Results that is **the interactive plan page, feature for
feature** — not a reduced view of it. The page's script
(`explain/assets/plan.html`, ~600 lines of JS) is lifted into
`plan.js`, a module that takes the Document JSON (`Plan.JSON()`) and a root
element, so the standalone page (`Plan.HTML()`, `dbc explain --open`, the
TUI's `b`) and the web UI run **the same code** and cannot drift. The
standalone page keeps working offline and self-contained (its tests —
`TestHTMLIsSelfContained`, `TestHTMLStatementCannotBreakOut` — stay green): it
inlines the same module at build time via `go:embed`.

Parity checklist — every item here exists in the page today and must exist
in the web Plan tab:

| Area | Feature |
|---|---|
| Header | headline; engine and connection; the analyzed / "estimated plan · measured run" / "estimated — not executed" chip; the EXPLAIN command dbc sent (truncated, full on hover); engine notes; the statement, SQL-highlighted, in a fold (open when short) |
| Metrics | a button per metric the plan carries (time, cost, rows, …, and the "shape ≈" heuristic with its explanation), keys `1`–`4`; bars, colors and flame widths all follow it; the cool→hot legend ramp and "own share of …" label |
| Graph | top-down tidy tree of step cards (glyph, op, target, the metric's value and share, heat color by own share); edges whose thickness is log(rows flowing up); "Outer"/"Inner" edge labels only where they mean something; drag to pan, wheel to zoom at the pointer, − / + / Fit (`f`); refit on resize until the user pans or zooms; big plans start folded below depth 5, Enter / click folds; the selected step's path to the root highlighted |
| Keyboard | ←↑↓→ walk parent / first child / siblings (unfolding as needed), Enter folds, `f` fit, `g` graph, `1`–`4` metric, Esc zooms the flame back out |
| Flame | icicle by inclusive weight within the zoom root; click a block to zoom into it, click the root to zoom out; breadcrumbs; falls back to counting steps when the metric is zero everywhere |
| Step detail | time total / self with share bar; rows actual vs estimated with the misestimate factor; loops and parallel workers; rows removed by filter; cost startup → total and self with share bar; width; buffers hit / read; temp blocks; spill to disk; hash batches; workers planned / launched; table rows; share under shape/rows; every raw property; the step's own findings |
| Insights | severity glyph, title, detail, suggested fix; the suggested SQL with a Copy button; click a finding to reveal and select its step; count in the heading ("· 5 (2 to act on)") |
| Boot | opens on the step the most serious finding is about (else the root); `#flame` deep link opens the flame view |
| Theme | dbc palette as CSS variables, light/dark toggle |

What the web adds on top, because it is inside the workbench rather than a
file: `Ctrl+X` / `Ctrl+Shift+X` from the editor; re-explain (`e` / `a`) and the
**before/after comparison** the TUI logs ("vs the last plan of this
statement: …"); `⤓ insert` a finding's SQL into the editor (never run it), next
to Copy; "✦ ask the assistant about this step / plan"; copy plan as text and
the engine's raw output; "↗ open as standalone page" (`/api/v1/ws/:id/plan.html`,
the same page, savable and sendable); and a plan detected in a typed
`EXPLAIN`'s result (`RunDone.Plan`) opens the tab just as in the TUI.

**Done when** the checklist is ticked in a headless-Chrome run against the
fixtures `explain/html_test.go` already renders (Postgres analyzed, MySQL,
SQLite, bytdb), and the standalone page still passes its own tests.

*Outcome:*

| Piece | Where | As built |
|---|---|---|
| The view | `explain/assets/plan.js`, `plan.css` | the page's script, now `DbcPlan.mount(root, doc, opts) → {destroy, select, setTab, fit}`: every lookup scoped to `root` (parts are `data-p`, no ids), every rule under `.dbc-plan`, per-mount state, `destroy` unhooks its document key listener and ResizeObserver. Options carry the web's extras (`embedded`, `keys`, `copy`, `onInsert`, `compare`, `actions`); the standalone page passes only `hash` |
| Standalone page | `explain/assets/plan.html`, `explain/html.go` | a template that inlines `plan.css` and `plan.js` (+ a one-line boot) — still one self-contained file. `explain.PlanJS` / `PlanCSS` are exported for the web; `explain.ScriptHash()` is the CSP hash of the page's one inline script |
| Plan tab | `web/static/js/planview.js`, `web/plan.go` | `POST …/explain {…, analyze, again}` (Ctrl+X / Ctrl+Shift+X / Alt+X, ◈ Explain; `e` / `a` explain the plan's own statement again); SSE `explain`; `GET …/plan` → `{doc, compare}` mounted in the results pane beside ▦ Results; `y` / `Y` (`GET …/plan/text`) copy the text tree or the engine's output; `b` / ↗ Page opens `GET …/plan.html` (the standalone page, its own CSP: `script-src` = the hash only); ⤓ Save downloads it; ⤓ Insert beside a finding's Copy appends its SQL to the editor, never runs it; `p` flips between Results and Plan; a typed `EXPLAIN`'s result opens the tab |
| Shared words | `explain.SameSubject`, `explain.Compare`, `Plan.Findings`, `workspace.PlanNotes`, `workspace.PlanStatus` | moved out of the TUI (`tui/plan.go`, `tui/explain.go` now call them), so "explained in …", "vs the last plan of this statement: was 30.1 ms → 87 µs ▼100%" and the status line read the same in both UIs. The web keeps the replaced plan per tab as the "before" |

Decisions and findings:

- **The theme toggle is per view**: `data-theme` on the view's root, and
  the light palette is scoped to `.dbc-plan[data-theme="light"]`, so the
  plan can go light without the workbench (whose light mode is Phase 6).
- **The narrow layout follows the view's width**, a container query rather
  than a media query: inside the workbench the view is narrower than the
  window.
- **Ctrl+X explains only with nothing selected** (with a selection it is
  the browser's cut; on a Mac, Ctrl+X with a selection does nothing, as in
  any Mac text field, and ⌘X cuts). ◈ Explain and Alt+X always explain.
- The checklist's "EXPLAIN command (full on hover)" was not quite so in the
  page — its hover said only "The EXPLAIN dbc sent"; the hover now carries
  the full command, in both homes.
- Deferred to Phase 5, with the chat pane: "✦ ask the assistant about this
  step / plan".

Verified: `explain` tests (the page inlines the shared view, holds no
`</script`, has exactly one executable inline script, and `ScriptHash`
matches it; the existing self-contained and break-out tests unchanged);
`web` tests for explain → event → plan, again (the plan's statement, the
before kept), plan text and raw, a typed EXPLAIN opening the tab, refusals
(nothing, several statements, busy 409), the served page's CSP and
download name, and the shared assets served byte for byte; all green,
`-race` too; the live `db` tests pass against Postgres 17 and MySQL 8.4.
In headless Chrome, 283 checks: the whole parity checklist (header, chip,
command on hover, statement fold and highlighting, metric buttons and keys
1–4, ramp and legend, cards, edge widths, drag pan, wheel zoom, − + Fit, `f`,
boot on the worst finding, ←↑↓→ walk, Enter folds, path highlight, detail
for every step, insight click selects, flame, zoom, breadcrumbs, Esc,
`g`, theme toggle, `#flame` deep link, a 255-step plan folding below depth
5) on the five fixture pages AND in the web Plan tab on live Postgres
(analyzed), MySQL, SQLite and bytdb plans; then the web's extras: Ctrl+Shift+X
from Monaco, ⤓ Insert of a finding's `CREATE INDEX` (nothing ran), running
it, `a` → "was 30.1 ms → 87 µs ▼100%" in the log and as a header chip, `y` /
`Y` on the clipboard, `p` both ways, `b` opening the standalone page whose
inline script runs under the hash-only CSP with a clean console, a typed
`EXPLAIN QUERY PLAN` opening the tab, 420 px with no horizontal scroll, and
a clean console throughout.

### Phase 5 — the assistant and scripts

The chat pane on SSE (`ai.Chat.Events()` → stream), stop, model picker,
conversation archive shared with the TUI, the device-flow sign-in in the page
itself. The data rule, hidden columns and sort order exactly as the TUI sends
them — through `workspace.ChatContext`, so they cannot differ. Scripts:
picker, run, `s.Print` lines to the log and `s.Show` results to the grid.

### Phase 6 — polish and packaging

Multiple query tabs per window, layout persistence, keyboard help overlay,
light/dark, error pages, a cats plugin action ("dbc — web") beside the TUI's,
and optionally a macOS app wrapper the way gonotes has one. README section.

## Testing

- **`workspace`**: unit tests for every rule, using the in-memory SQLite and
  bytdb managers the `db` and `tui` tests already build, plus the existing
  opt-in live tests against Postgres and MySQL.
- **`web` handlers**: integration tests against a real rweb server, gonotes'
  way — `rweb.ServerOptions{Address: "localhost:", ReadyChan: ready}`, the
  port from `GetListenPort()`, a `request(method, path, body)` helper that
  decodes the envelope. Auth (no cookie → 401, bad Host → 403, cross-site
  POST → 403, Bearer works), routing, the serr → status mapping, SSE event
  order for a multi-statement run, cancel mid-run, download headers, CSP
  present on every page.
- **Browser**: a small set of end-to-end checks with headless Chrome (already
  used to verify the plan page): load, run, see rows, copy, explain. go-rod
  is in `~/projs/go/rod` if scripted interaction is wanted; screenshots for
  layout regressions at desktop and 420 px widths.

## Risks

| Risk | Handling |
|---|---|
| The `workspace` extraction breaks subtle TUI behavior | Phase 1 changes no behavior and ships alone; the TUI's 109 tests are the contract |
| Browser shortcuts collide (`Ctrl+R` reloads, `Ctrl+P` prints, `Ctrl+E` focuses the URL bar in some browsers) | `preventDefault` where the browser allows it; documented alternates (`Ctrl+Enter` to run, `Ctrl+Shift+Enter` run all) that are the de-facto web SQL keys |
| A long result floods the browser | server-side paging; the grid fetches only visible rows; `max_display_rows` still applies |
| A tab left open holds a transaction open | idle release after `conn_idle_timeout`, and a visible "transaction open" badge per tab |
| Someone exposes it on a network | loopback default, TLS required off-loopback, token always on |
| Monaco is a large vendored blob (~4 MB in the binary) | pinned version via `scripts/vendor_monaco.sh`, license beside it, lazy-loaded; the `<textarea>` stays the source of truth, so the page works before (or without) it |
| rweb has no graceful shutdown for open SSE streams | after `Run` returns: cancel every workspace's run, close chats, release sessions, save tabs — herdr-web's short grace period before exit if streams hang |

## Later (out of scope here)

- A team server: logins (OIDC), per-user connection grants, audit log,
  read-only roles. A different trust model, and a separate plan.
- Shared, linkable result snapshots and plans.
- Collaborative editing of a query buffer.
