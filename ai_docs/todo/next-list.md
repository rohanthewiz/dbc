# dbc — Next list

The project's one living list of follow-ups. Sessions edit this file in place
rather than copying a Next list forward into each session doc; a session doc's
own `## Next` section only summarizes what it changed here
(`Closed: N-… · Raised: N-…`). Seeded 2026-09-24 by `/next-list seed` from all
ten session docs in `ai_docs/claude_sessions/`
(`2026-0728-1923` … `2026-0913-2158`).

## Conventions

- **IDs are permanent** (`N-001`, `N-002`, …) and never reused, even after an
  item is closed or declined.
- **`raised`** is the stem of the session doc the item first appeared in.
- **Age is computed, never stored**: the number of session docs since `raised`
  (an item raised in the newest doc has age 0). It counts sessions, not days.
- **Value** is the payoff of doing it, not the effort:
  - `high` — something is being worked around today, or a second independent
    consumer has arrived
  - `medium` — it blocks one named thing, or is a visible defect nobody has to
    route around yet
  - `low` — a gap nobody has bumped into, or contingent on something that does
    not exist yet
- **Open** is what we intend to pick up next. **Roadmap** is wanted, but not
  soon. **Non-goals** are what we are likely never to do, kept so they stay
  visibly declined.
- **Nothing leaves Open or Roadmap without a line in another section** — moved
  to Closed (with what closed it) or to Non-goals (with the reason). Moving
  between Open and Roadmap is fine.
- Open and Roadmap stay in ID order. Never renumber, never delete.

**Next ID:** N-060

## Open

- **N-044** · raised `2026-0925-1353-x11-clipboard-owner` · value low
  Verify the X11 clipboard owner (`clip/x11owner.go`) on a real X11 desktop:
  a GNOME/KDE terminal paste after an HTML copy gets the markup, Teams or
  LibreOffice gets the table, and a clipboard manager (Klipper, GPaste) does
  not keep a helper alive. Only Xvfb + xclip has exercised it so far.
- **N-047** · raised `2026-0925-1425-workspace-extraction-web-phase1` · value low
  Opt-in live `workspace` tests on Postgres/MySQL (same `DBC_LIVE_*` DSNs):
  retry-once, session lost, cancel mid-statement and connection switch through
  the workspace, not just `db`. Its own tests use in-memory SQLite only.
- **N-051** · raised `2026-0925-1554-dbc-web-phases-3-4` · value low
  Commit the `dbc web` browser checks as an opt-in test (go-rod against a
  built binary, skipped unless asked for, like the `db/live_*` tests). Phases
  2–6 were verified by throwaway scratch programs (283 checks for Phase 4,
  63 for Phase 5 — with a fake ACP agent binary on `PATH` — and 53 for
  Phase 6) that are gone after the session, so a regression in the page's
  JavaScript is caught only by hand. Phase 6's run found three real bugs
  (a disposed Monaco model, a console rejection, a rename redrawn away).
- **N-053** · raised `2026-0925-1641-dbc-web-phases-5-6` · value low
  A macOS app wrapper for `dbc web`, the way gonotes has one — the plan's
  optional last item of Phase 6, left out: `dbc web` and the cats "dbc —
  web" action already launch it, and a `.app` is a second packaging path to
  keep building and signing. `/api/v1/health` is there for a wrapper to
  poll.

## Roadmap

Wanted, but deliberately not next. Empty at seeding: the session docs never
marked an item as deferred, so sorting Open items into here is the user's
call.

- **N-029** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` · value medium
  Verify the Windows rich clipboard (`clip/rich_windows.go`) on a real
  Windows machine. It cross-compiles and vets, and its CF_HTML envelope is
  unit-tested, but it has never pasted into Teams from Windows.
- **N-058** · raised `2026-0925-1902-explain-plan-sharing-pdf-jpeg-mermaid` · value low
  The plan's PDF is the picture on a page, so its text can't be selected or
  searched. A vector PDF would need the Go fonts embedded as CID fonts, and
  the layout drawn through a second backend. Only worth it if someone asks to
  search or copy from a shared PDF (`y` copies the plan as text today).

## Non-goals

- **N-012** · declined `2026-0728-2000-stmt-under-cursor-and-query-cancel` —
  Highlighting the statement under the cursor in the editor: tview's `Select`
  would leave a destructive selection behind.
- **N-013** · declined `2026-0728-2022-multi-statement-headless` — modernc
  SQLite reports a stale `changes()` as `RowsAffected` for `BEGIN` (showed as
  "8 rows affected"). Pre-existing driver behavior, left alone.
- **N-014** · declined `2026-0913-2158-dbc-migrate-replaces-goose` — Running
  church's `db/migrate` on bytdb through `dbc migrate`: the files are
  Postgres-specific (`OWNER TO`, `BIGSERIAL`) and church's bytdb backend boots
  from `db/bytdb_schema.go`. Recorded so it is not attempted by accident.
- **N-015** · declined `2026-0913-2158-dbc-migrate-replaces-goose` — After
  `down-to 0`, a church migration's Down apparently leaves one table behind.
  A church concern, not a dbc one.

## Closed

Newest first. Everything closed before the list existed (2026-09-24) is
written up in the session docs themselves.

- **N-059** · raised `2026-0925-1902-explain-plan-sharing-pdf-jpeg-mermaid` ·
  closed 2026-09-25 — the shell's and the TUI's plan pictures can be light.
  `plan_theme = "light"|"dark"` in the config (normalized by `config.Load`;
  a typo warns and draws dark) sets it for `dbc explain -t pdf|jpeg|png` and
  the TUI's Save as PDF / JPEG. `dbc explain --theme light|dark` overrides
  it for one run, and is refused, before the explain runs, for a format with no
  palette. `theme.ByName` maps the name to `Default()` / `Light()` (only
  the built-ins: a cats host palette is the terminal's, not the reader's).
  dbc web is unchanged: it still follows the view. Tested in
  `explain_test.go` (flag > key > dark, PNG corner = the palette's Bg),
  `tui/explain_test.go` (a light JPEG), `config` and `theme`, and by hand
  with a built binary.

- **N-057** · raised `2026-0925-1812-web-add-connection` ·
  closed 2026-09-25, 2026-0925-1930-N-057-shared-connections — connections
  added in `dbc web` now live in `~/.config/dbc/connections.toml` (the user
  chose a dbc-owned TOML file over a second bytdb file), which every command
  merges in `main.setup` after the
  config file and the ad-hoc `--dsn` one (`config.LoadSaved`; a clash, an
  unknown driver or a nameless entry is skipped with a warning). Readers take
  no lock. `config.SavedStore` writes by a locked read-modify-write
  (btypedb's `AcquireLock` sidecar, polled up to 3s), then a temp file,
  fsync and rename, so a reader never sees half a file. The file is `0600`
  in the `0700` directory, and DSNs are stored as typed, with `${VAR}`s
  unexpanded. A file that fails to parse is never overwritten. `dbc web`
  moves any `web.bytdb` `conns` rows to the file at startup
  (`moveStoreConns`). A rename's saved-tab retag is now `Store.RetagTabs`.
  A second `dbc web` can add connections too; one the file already has is a
  409. A running `dbc web` reads the file only at startup. Tested in
  `config/saved_test.go` (including concurrent writers and waiting on a held
  lock) and `web/conns_test.go`, and end to end with real binaries: an old
  build's row moved, headless runs on it while `dbc web` ran, and a rename
  and a delete were seen by the next headless run.

- **N-056** · raised `2026-0925-1812-web-add-connection` ·
  closed 2026-09-25, 2026-0925-1831-N-056-edit-connection — a connection
  added in the browser is edited in place from the sidebar's right-click
  *Edit…* (`PUT /api/v1/conns/:name`). The
  form is the add form, filled from the sidebar (`ai_rows` now rides in the
  connection list); its DSN field starts empty, and empty means "keep the
  stored DSN" — for Save, and for Test via `from` (`keepDSN`). A kept DSN
  goes only with its own driver. `config.ReplaceConn` swaps the entry in
  place under the lock (a rename keeps its position; a clash or a removal by
  another window is caught there); `Store.UpdateConn` keeps `added` and, on a
  rename, moves the saved tabs' `conn` in the same transaction, and the
  "conns" event carries `renamed {from, to}` so every window moves its
  not-yet-shown tabs too. A rename, driver or DSN change is refused while a
  tab is on the connection (as a removal is); `ai_rows` alone is not, since
  the assistant reads it afresh. History and saved chats keep the old name.
  Tested in `web/conns_test.go` on both the memory and the bytdb store, and
  end to end in headless Chrome.

- **N-054** · raised `2026-0925-1641-dbc-web-phases-5-6` ·
  closed 2026-09-25, 2026-0925-1728-N-054-tab-claims — a window claims the saved tabs it shows
  (`web/claims.go`; the hub keeps key → window). Boot claims every free
  tab (`POST /api/v1/win/:id/tabs`); a second browser tab gets the rest, or
  a fresh tab of its own. Saves, deletes and layout writes carry `?win=`, and
  a tab another live window holds is refused with a 409. A claim lasts while
  its window is live: a stream attached, or detached less than 15 s ago (a
  reload, a reconnect). pagehide sends one `…/release` carrying the last
  save, so a closed browser tab frees its tabs at once. A duplicated browser
  tab (sessionStorage copied, so the same window id) is refused at boot and
  opens its own window. A tab taken while its window was away is marked ⊘
  and no longer saved there. Layout writes keep the `tabs` order and `plans`
  keys of tabs another window holds. Tested in `web/claims_test.go`, and
  end to end in headless Chrome (two windows, a reload, a duplicate, a close
  and reopen, a lost tab).
- **N-055** · raised `2026-0925-1641-dbc-web-phases-5-6` ·
  closed 2026-09-25 — a tab's `planOpen` is saved in one layout key,
  `plans` (the keys of the query tabs showing their plan, comma-separated),
  rather than a column: bytdb has no `ADD COLUMN IF NOT EXISTS`, and the
  order and active tab already live in the layout. Rewritten whole, so a
  closed tab drops out. `planview.js` reports each results/plan switch
  (`dbc.cmd.onPlanPane`, silent during a tab switch's reset); `app.js`
  writes the key when it changes, on a switch, and with the order. A
  reload reattaches the same workspace, so its plan loads and reopens.
  Also fixed along the way: a background run with a plain result kept the
  tab on its old plan, where the foreground switches to the grid. Checked
  in headless Chrome (reload on the plan, reload on the grid, a background
  tab's plan across a reload, a background run, closing a flagged tab).
- **N-052** · raised `2026-0925-1554-dbc-web-phases-3-4` ·
  closed 2026-09-25, 2026-0925-1708-numeric-column-widths — a numeric column (`export.NumericColumns`) is now sized
  from every row, by `workspace.WidestNumeric`: a `len()` per cell, since a
  number's text is ASCII. Text columns keep the 500-row sample. It measures
  the widest text rather than using min/max, because a float's text is not
  monotonic in its value (`0.123456789` is wider than `1000`). The web grid
  now caches its widths per result in `resultView` rather than measuring on
  every page fetch. `TestGridSizesNumericColumnsFromEveryRow` in `tui` and
  in `web` (1,000 rows: `1000` gets width 4, a text value past row 500 is
  still unmeasured); both fail on the old code.
- **N-050** · raised `2026-0925-1451-dbc-web-phase2-skeleton` ·
  closed 2026-09-25, 2026-0925-1656-rweb-ssehub-race — rweb's `SSEHub` bumped each
  client's drop counter under its read lock, so concurrent broadcasts
  raced. Fixed in rweb v0.1.32 (`3169f85`): the counter is an
  `atomic.Int32`, keeping broadcasts parallel rather than serializing them
  on the write lock; `TestSSEHubConcurrentBroadcast` fails under `-race`
  without it. dbc upgraded and dropped the per-window `sendMu` from
  `web/hub.go` (the item said `tab.sendMu`; it lived on `window`). On
  v0.1.31 without the mutex `web`'s tests race; on v0.1.32 the full suite
  is clean under `-race`.

- **N-049** · raised `2026-0925-1451-dbc-web-phase2-skeleton` ·
  closed 2026-09-25, 2026-0925-1651-bytdb-file-lock — two dbc processes
  shared one bytdb file. bytdb
  v0.18.0 now locks it itself (an exclusive lock on a `<file>.lock`
  sidecar, taken in `sql.Open` and held for the pool's life), and dbc
  upgraded to it. `db.Manager` turns the lock error into `db.ErrInUse`
  ("another dbc — a TUI or dbc web — most likely has it open"), so a held
  demo is dropped with that warning and a held configured connection says
  so where it was opened. `web.bytdb`'s own flock (`web/lock_*.go`) is gone:
  it took the very sidecar bytdb now locks, and the lock is not reentrant,
  so it would have refused the store's own open. Checked by
  `TestBytdbFileInUse`, `TestSeedDemosDropsHeldBytdbDemo`,
  `TestStorePersistsAndLocks`, and two real `dbc web` processes on one HOME.

- **N-048** · raised `2026-0925-1425-workspace-extraction-web-phase1` ·
  closed 2026-09-25, 2026-0925-1641-dbc-web-phases-5-6 — dbc web Phases 5 (`d7dd3f0`) and 6 (`51bcf15`). Phase 5: the
  assistant pane on the window's stream (`web/chat.go`, `chat.js`: lazy
  agent, server-side transcript, stop, model/agent switch, the shared
  archive, device-flow sign-in in the page; context through
  `workspace.ChatContext`, a stale grid view with hidden columns refused),
  "✦ ask" from the grid menu, inspector, Monaco's menu, the plan header and
  each step, and scripts (`web/scripts.go`: by name, never a path). Phase
  6: windows holding query tabs (one stream and assistant per window, a
  workspace per tab, Alt+T/W/1–9, per-tab editor model, grid view and
  plan), layout persistence, light/dark from `theme.Light`, the F1 key
  list, error pages, the cats "dbc — web" action, the README section. The
  macOS wrapper was left out (N-053). Raised N-053, N-054, N-055.

- **N-046** · raised `2026-0925-1425-workspace-extraction-web-phase1` ·
  closed 2026-09-25, 2026-0925-1554-dbc-web-phases-3-4 — the plan page's script is now
  `explain/assets/plan.js` (`DbcPlan.mount`, scoped, per-mount state) with
  `plan.css` under `.dbc-plan`; the standalone page inlines both and dbc
  web's Plan tab loads the same files. The parity checklist passed in
  headless Chrome on the fixture pages and on live Postgres, MySQL, SQLite
  and bytdb plans in the web tab, plus the web's extras (explain from the
  editor, again with before/after, copy text/raw, open/save the page,
  insert a finding's SQL). Phase 3 (Monaco, virtualized grid, copy/export,
  history, preview) shipped in the same session.

- **N-045** · raised `2026-0925-1425-workspace-extraction-web-phase1` ·
  closed 2026-09-25, 2026-0925-1451-dbc-web-phase2-skeleton — `dbc web`
  Phase 2 is built: `dbc web [--listen] [--no-open] [--secret]`, package
  `web/` (rweb server; guard middleware with Host and Origin checks, the
  per-launch secret traded for a SameSite=Strict cookie at `/login?s=`,
  Bearer for scripts; the envelope and serr → status mapping; the element
  shell and result table; embedded assets; one workspace and SSE stream per
  browser tab with reattach, idle release and forget; cleanup on Ctrl+C;
  health; `web.bytdb` for tabs and layout, advisory-locked), and
  `db.Manager.SetMemoryPool` for the in-memory SQLite cap. Auth was kept
  small at the user's word (no TLS, login form or CSRF token). The plan's
  Phase 2 outcome lists the decisions. Checked by 17 tests under `-race` and
  end to end in headless Chrome. Raised N-049, N-050.

- **N-030** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-25, 2026-0925-1353-x11-clipboard-owner — Linux rich copy served text/html only. Premise
  narrowed first: Wayland was already fine (wl-copy offers text/plain and the
  X11 text atoms for any text/* type); the gap was X11, where xclip lists only
  text/html and GTK/Qt terminals pasted nothing. dbc now owns the X11
  CLIPBOARD itself (`clip/x11owner.go`, pure-Go `jezek/xgb`): a detached
  re-exec of the binary serving text/html, UTF8_STRING, text/plain(;charset),
  TEXT and Latin-1 STRING, with INCR for large copies, until another client
  copies. xclip remains the fallback. Verified in Docker/Xvfb by
  `clip/live_linux_test.go` (opt-in `DBC_CLIP_LIVE=1`) and a headless
  `dbc script` export whose clipboard outlived the process.

- **N-011** · raised `2026-0913-2158-dbc-migrate-replaces-goose` ·
  closed 2026-09-25, 2026-0925-1330-tag-v0.1.0-release — Tag a release. Annotated tag `v0.1.0` on `aec93c8`
  (main, even with `origin/main`) is pushed, matching `version = "0.1.0"` in
  `cats-plugin.toml`. Before it was pushed, build, vet and the full test suite
  passed, and go.mod has no `replace` directives. `go list -m
  github.com/rohanthewiz/dbc@latest` now resolves to `v0.1.0` rather than a
  pseudo-version of `main`. When `cats-plugin.toml`'s version changes, tag to match.

- **N-043** · raised `2026-0925-1203-live-session-and-timeout-tests` ·
  closed 2026-09-25, 2026-0925-1252-pgx-ping-cut-pooled-conns — Postgres pools are now opened through `pgxstdlib.OpenDB` with `OptionShouldPing(pgShouldPing)` (`db/manager.go`). pgx's 1s rule stays. On top of it, a non-blocking `MSG_PEEK` on the socket (`db/sockpeek_unix.go`, the same check go-sql-driver/mysql runs, minus consuming the byte) asks for a ping when bytes or a FIN arrived while the connection was idle. There is no extra round trip. It was not always-ping, because that would cost a round trip per pooled statement, which is heavy for a script on a remote server. `TestLiveCutPooledConnReplaced` now reuses the killed connection both at once and after 1s. Against Postgres 17 the at-once case failed 3/3 without the option (57P01) and passed 5/5 with it. MySQL 8.4 passes both. `TestSockQuiet` covers the peek on loopback TCP. A malformed DSN now fails at parse, still with the password masked.

- **N-042** · raised `2026-0925-1145-readme-example-and-live-schema-lookup` ·
  closed 2026-09-25, 2026-0925-1212-postgres-matviews-in-catalog — `TablesQuery` for Postgres adds `pg_matviews` rows with `UNION ALL`, typed `MATERIALIZED VIEW`, and filtered by `has_table_privilege(..., 'SELECT')` so they follow the same rule as `information_schema.tables`. bytdb has no `pg_matviews`, so it gets its own case with the old query. `TestLiveColumnsPostgres` now covers a matview; ran against Postgres 17.

- **N-035** · raised `2026-0924-1725-conn-mgmt-session-discard-timeouts` ·
  closed 2026-09-25, 2026-0925-1203-live-session-and-timeout-tests — ran against Postgres 17.11 and MySQL 8.4.11 in throwaway containers; no product change needed. `db/live_session_test.go` (opt-in, same DSN variables) checks: Close ends the transaction, row lock, SET and temp table, and the server drops the connection; the drivers' own pool reset keeps SET/temp tables on both, and MySQL keeps an open transaction (the reason Close discards); killed and idle-timed-out sessions classify Drop/Lost, then Retry/Lost, with the pgx `IsClosed` branch doing the catching; an SQL error keeps the session; `conn_idle_timeout` closes idle pooled connections and spares a session; `connect_timeout` spares long statements. `TestConnectTimeoutBoundsOpen` now covers MySQL too. Raised N-043.

- **N-032** · raised `2026-0924-1458-assistant-schema-context` ·
  closed 2026-09-25, 2026-0925-1145-readme-example-and-live-schema-lookup — ran against Postgres 17.11 and MySQL 8.4.11 in
  throwaway containers; no change needed. `db/live_test.go` keeps it
  repeatable (opt-in: `DBC_LIVE_PG_DSN`, `DBC_LIVE_MYSQL_DSN`) and walks the
  assistant's path — `TablesQuery` → `TableIndex.Mentioned` → `Columns` —
  comparing types exactly: modifiers, `text[]`, a schema-qualified enum, a
  dropped column, a view, a partitioned parent, quoted mixed case, one name in
  two schemas; MySQL `int unsigned`, enum values, `decimal(5,2)`,
  `datetime(3)`, `json`, a view. Found on the way: matviews are not listed
  (N-042).

- **N-041** · raised `2026-0925-1111-rename-sqlite-demo-to-demo-sqlite` ·
  closed 2026-09-25, 2026-0925-1145-readme-example-and-live-schema-lookup — regenerated from the binary. The example was
  broken, not just stale: its INSERT failed on `demo-bytdb` (no id given), and
  the old output lacked the Bengal and Sphynx rows. It now inserts `id 9` and
  orders `n DESC, breed`; both demos print the same.

- **N-028** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-25, 2026-0925-1129-copilot-sign-in-from-dbc — Copilot sign-in from inside dbc. `ai/signin.go` runs
  the language server in LSP mode (`--stdio`; the transport gained
  Content-Length framing next to ACP's NDJSON) and drives GitHub's device
  flow: `signIn` → code → `workspace/executeCommand` (≤15 min). ACP's own
  `authenticate` was not used: it is an OAuth code flow that needs a browser
  on the dbc host, which fails over SSH. A handshake or turn refused for lack
  of auth is now `ai.ErrAuthRequired`. The pane offers ⎆ sign in under it,
  puts the code in the transcript, copies it, opens the page, and reconnects
  on success. A question refused with the handshake (even mid schema lookup)
  goes out after sign-in. The transcript menu has "Sign in to Copilot…" at any
  time. Checked against the real server (1.526.0): signed in →
  `AlreadySignedIn`; with an empty `XDG_CONFIG_HOME` → ACP refuses with
  `Authentication required (-32000)` and `signIn` returns a real device code.
  Not checked: entering a code end to end (it would mint a real token).

- **N-009** · raised `2026-0807-2148-bytdb-v0.9.1-and-dual-demo-defaults` ·
  closed 2026-09-25, 2026-0925-1111-rename-sqlite-demo-to-demo-sqlite — the rename was wanted: the SQLite demo is now
  `demo-sqlite` (`config.DemoSQLite`), symmetric with `demo-bytdb`. The
  three scripts, the README (demo table, `-c`, script examples) and the two
  test harnesses that run shipped scripts (`newTestManager`, the TUI's
  `newTestModelInHost`) follow it. No alias: `-c demo` and user scripts that
  name `"demo"` now fail with "unknown connection". The rename alone does not
  make the scripts run on either demo — their `?` placeholders are SQLite's,
  bytdb takes `$1`. Checked through the binary with no config:
  `dbc script scripts/loop_params.go` lands on `demo-sqlite`.

- **N-038** · raised `2026-0924-1958-assistant-conversation-archive` ·
  closed 2026-09-25, 2026-0925-1058-delete-live-assistant-conversation — the transcript's right-click menu has a **Delete this
  conversation** row (disabled, saying why, on an empty pane). It removes
  the live conversation's file if it has one, then clears the pane through
  `resetChat` — the half of `newChat` after the save — so nothing writes the
  file back, and the next question starts a fresh agent session rather than
  one that still remembers the deleted conversation. A file that will not
  delete leaves the pane untouched. As with saved conversations, the menu
  row is the deliberate second step; there is no keyboard shortcut.

- **N-006** · raised `2026-0728-2022-multi-statement-headless` ·
  closed 2026-09-25 — `Ctrl+Shift+R` (and `Alt+R`, since a terminal without
  the kitty keyboard protocol sends Ctrl+Shift+R as plain Ctrl+R) runs every
  statement in the buffer, wherever the caret or selection is. The design it
  was waiting on did not have to change: `Ctrl+R` keeps its one-statement
  default, and run-all is a second key onto the path a multi-statement
  selection already took (`runStmts` → `run`: in order, on the pinned
  session, stopping at the first failure, last result shown, each statement
  recorded in history). The editor's right-click menu has a `▶ Run all N
  statements` row, disabled with a reason when the buffer holds one
  statement or the selection already covers all of them. It is not on the
  toolbar and has no ⌘ twin.
- **N-040** · raised `2026-0925-1026-headless-streaming-tx-keep-going` ·
  closed 2026-09-25, 2026-0925-1042-headless-script-streaming — a headless
  script now streams each `s.Show` result to stdout in a block format
  (`scriptStreams`: no `-o`, `export.Streamable`), so its blocks interleave
  in order with the `s.Print` lines. `RenderBlock` takes `n <= 0` as an open-ended total and banners it `#i` (`export.Pos`);
  `blockStream` with `total: 0` names its blocks `result #i` in notes and
  errors, and its `show` drops later results and cancels the script's
  context after a failed write, which is reported as "render failed" rather
  than "script canceled". Every streamed script result has a banner, even a
  lone one, since the stream cannot know a second will not follow. That
  changes a one-`Show` script's `text`/`markdown` output, which used to
  render bare. `-o` in a block format writes `export.RenderOpen`, the same
  `#i` document, so `> file` and `-o file` agree. HTML and JSON still
  collect via `RenderAll`. Checked through the binary on a scratch SQLite
  file with a script that sleeps between shows: each block appeared a
  second apart, right after its log line.
- **N-004** · raised `2026-0728-2022-multi-statement-headless` ·
  closed 2026-09-25, 2026-0925-1026-headless-streaming-tx-keep-going —
  both exist. `--tx` runs dbc's own `BEGIN` before the
  buffer and `COMMIT` after it. On the first failure or a Ctrl+C it runs
  `ROLLBACK` instead (`endTx`, under a context of its own, since Ctrl+C has
  killed the run's) and notes it on stderr; a failed COMMIT exits 1 as
  "commit failed". `-k`/`--keep-going` reports each failure as it happens
  (`statement=2/3`), goes on, and exits 1 at the end with "N of M
  statements failed". It still stops for a cancel or a dead connection
  (`Session.Classify` ≠ `FaultNone`). Refused as usage errors (exit 2):
  `--tx` with `-k` (all-or-nothing, and Postgres rejects everything after an
  error in a transaction); `--tx` over a buffer with its own transaction
  control (`txControl`: BEGIN, START TRANSACTION, COMMIT, END, ROLLBACK but
  not ROLLBACK TO, ABORT, PREPARE TRANSACTION), since a second BEGIN is an
  error on SQLite and an implicit COMMIT on MySQL; and both flags on
  `script`/`migrate` (`refuseFile` → `refuseQueryFlags`). MySQL's implicit
  commit before DDL is documented in the README, not detected.
  `runStatements` now takes `runHooks` and returns `runOutcome`, which keeps
  each result's statement position. New `export.RenderRun(rs, at, total, f)`
  numbers banners and HTML headings by statement, and picks the document's
  shape by statement count, not by how many succeeded. That also fixed two
  existing quirks of a failed run: collected banners said `2/2` where the
  error said `statement 3/5`, and `-t json` for a multi-statement run that
  failed after one success printed a bare row array instead of the envelope
  array. Checked through the binary on a scratch SQLite file and a scratch
  bytdb file: a failed `--tx` run kept 0 rows, a good one kept both.

- **N-003** · raised `2026-0728-2022-multi-statement-headless` ·
  closed 2026-09-25, 2026-0925-1026-headless-streaming-tx-keep-going —
  a headless multi-statement query streams: each result
  is written to stdout as its statement finishes. It streams in every block
  format (text, markdown, csv, tsv), not only `text` — their multi-result
  document is just the blocks joined by a blank line, so writing them one
  at a time gives the same bytes. HTML and JSON (one document around every
  result), `-o` (one file, a "wrote N rows" total) and a single statement
  (renders bare, no banner) still collect. `export.RenderBlock` renders one
  block and `RenderAll` is now built from it, so the two shapes cannot
  drift. `runStatements` takes an `onResult` callback; `blockStream`
  (`main.go`) writes each block and its truncation note together. One
  visible difference when a statement fails: streamed banners count against
  every statement (`2/5`), where the collected shape counts only the ones
  that ran (`2/2`) — gone since N-004, which numbers both by statement. Checked through the binary with a 1.7s recursive CTE as
  statement 2: block 1 printed at once, block 2 when it finished; `-t json`
  still printed at the end. Scripts still collect (N-040).

- **N-039** · raised `2026-0924-2007-returning-rows-not-rows-affected` ·
  closed 2026-09-24, 2026-0924-2029-lazy-demo-seeding-with-main-verb — a
  `WITH … INSERT/UPDATE/DELETE` now runs as an Exec and reports rows
  affected. New `sqlsplit.Verbs` walks a WITH at paren depth 0
  (lexically, like the rest of the package) and returns the verb after the
  CTE list (`Main`), where that verb starts (`MainAt`), and the leading
  verb of each CTE body (`CTEs`). A parenthesized main statement, a nested
  WITH, `[NOT] MATERIALIZED` and Postgres `SEARCH`/`CYCLE … SET` are
  handled. If it cannot find a main verb, `Main` stays `with`, which keeps
  the old Query path. `isQuery` goes by `Main` and looks for `RETURNING`
  only in the main statement, so a CTE's own `RETURNING` does not turn an
  `INSERT … SELECT` into a Query. `isRead` is false when `Main` is a write
  or when any CTE body writes. That covers Postgres's
  `WITH d AS (DELETE … RETURNING *) SELECT …`, which marks the session
  stateful even though it comes back as rows. `FirstKeyword` now uses
  `Verbs`' helpers. Tested on SQLite (session, and headless through the
  binary). bytdb itself rejects a write after a CTE list (its parser wants
  `SELECT` there), so it is a bytdb limit, not a dbc one.

- **N-010** · raised `2026-0807-2148-bytdb-v0.9.1-and-dual-demo-defaults` ·
  closed 2026-09-24, 2026-0924-2029-lazy-demo-seeding-with-main-verb — a
  built-in demo is seeded on its first open, by `Manager.open`, not up front.
  `config.Connection.Demo` (`toml:"-"`, set only by `demoFallback`) marks
  the demos. `setup` now takes a `demoOpen` mode. The TUI opens every demo
  (`SeedDemos`: prune and fall back, as before). A headless query or
  migrate with no `-c`/`--dsn` opens only the active demo
  (`OpenDefaultDemo`: the other demo is tried only if that one fails, so the
  fallback survives). `dbc script` opens nothing up front. So
  `--demo sqlite` and `-c demo` runs no longer touch the bytdb file. The bare
  default run still opens it, because bytdb is the default demo. Found on
  the way, and the bigger fix: with no config, `SeedDemos` seeded every
  connection, including the ad-hoc `--dsn` one. So `dbc --driver … --dsn …`
  ran `CREATE TABLE IF NOT EXISTS cats` and `DELETE FROM cats` on the
  user's database. This was reproduced against a scratch SQLite file, which
  lost its row. Only `Demo` connections are opened or seeded now
  (`TestDemoSeedingLeavesUserConnectionAlone`).

- **N-008** · raised `2026-0731-2200-dbc` ·
  closed 2026-09-24, 2026-0924-2007-returning-rows-not-rows-affected — a write
  with `RETURNING` shows its returned rows, not `rows_affected`. `isQuery`
  now also counts a statement whose leading verb is `insert`/`update`/
  `delete`/`replace`/`merge` and that carries a bare `RETURNING`, found by the
  new `sqlsplit.HasKeyword` (the word inside a string, a quoted identifier, a
  comment, or a dollar-quoted body doesn't count, and neither does
  `t.returning` or `:returning`). The fall-back-to-Query idea was dropped:
  drivers run the write and discard the rows without complaint, and a retry
  would run it twice. `Session` now marks itself stateful on any statement
  that is not a plain read (`isRead`) instead of on `IsExec`, so an
  `INSERT … RETURNING` in a transaction still counts. Tested on bytdb
  (insert and update) and SQLite (session).

- **N-027** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-24, 2026-0924-1958-assistant-conversation-archive — assistant conversations are kept. `userdata/chats.go` ports ced's `internal/chatstore` (stdlib + serr): one JSON file per conversation in `~/.config/dbc/chats/`, user-only, written via temp file + rename, newest-first listing that skips unreadable files, capped at the last 30. Each records the agent, model and active connection; the title is the first line of the first question. The TUI saves after every answer (and on an agent exit), on `⟲ new`, and on quit; a pane that was never asked anything saves nothing. The empty pane lists the five most recent as clickable rows (the time, message count and — when it differs from the active one — the connection, shed in that order on a narrow pane), with `all N recent…` and a right-click **Recent conversations…** list for the rest. Reopening saves the live one, starts a fresh agent session, reloads the transcript into the same file, and says in the transcript that the agent has no memory of it. Deleting a saved one: right-click its row (pane or list) → **Delete conversation**, or `d` twice on it in the list (the first press marks the row and says what the second will do; any other key, moving the cursor, or Esc disarms). The live conversation is never offered for deletion, since its next save would write it back.

- **N-037** · raised `2026-0924-1908-assistant-hidden-columns-grid-sort` ·
  closed 2026-09-24, 2026-0924-1908-assistant-hidden-columns-grid-sort — the assistant gets rows in the grid's order after a header sort, as copies do. `ai.Context` gained `Order` (the grid's permutation, only the prefix that could be sent, cloned, since `applySort` rewrites it in place and a submit's context waits on its schema lookup), `SortedBy` and `SortDesc`. The prompt says "The user sorted the result in the grid by age, descending (NULLs last), so the rows below are in that order, not the query's", so the model does not read the order into the SQL. The note reads `3 of 8 rows (sorted by age desc, 1 column hidden)`. With no rows sent, the sort is not mentioned.

- **N-036** · raised `2026-0924-1823-grid-column-resize-hide` ·
  closed 2026-09-24, 2026-0924-1908-assistant-hidden-columns-grid-sort — decided: hidden columns stay out of what the assistant gets, as they stay out of copies and exports. Hiding is how a user says "not this one", and a sensitive column is what gets hidden before sharing. Getting it wrong the other way costs one `+`. The hidden columns' names still go ("The user hid these columns in the grid, so they are left out above: …"), since names are schema. `ai.Context.Hidden` carries the grid's hidden result columns; `ai.Build` leaves them out of the rows it sends and of the column-names line. The note (chip and transcript) says `3 of 8 rows (1 column hidden)`, worded like the copy log. The context chip now wraps to a second row instead of truncating, since the end of the note was cut off at the default pane width.

- **N-031** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-24, 2026-0924-1823-grid-column-resize-hide — grid columns resize and hide. Drag a header border to resize (highlighted `┃` on hover), double-click it or `=` to fit to content past the 40-cell auto cap, `<`/`>` by keys. Hide from the right-click menu (the cursor's column or the range's) or `-`; the menu lists hidden columns by name to show again, `+` shows all; the last column cannot be hidden. The grid now works in display columns (`grid.cols`, like `order` for rows), so the cursor, hit-testing, copies and the file export take what is on screen; `║` in the header marks a gap, the strip counts hidden columns, and a whole-result copy's log line says how many were left out. Hidden columns and hand-set widths survive a re-run with identical columns. The file export now also follows the grid's sort order, as its clipboard button already did.

- **N-005** · raised `2026-0728-2022-multi-statement-headless` ·
  closed 2026-09-24, 2026-0924-1810-cli-urfave-file-stdin-input — `-f`/`--file` reads the SQL from a file (`-f -` = stdin), and piped stdin with no SQL argument runs headless too; the output format moved to `-t`/`--format`. The CLI moved from Go's `flag` to `urfave/cli/v3`: every flag has a long name (`--conn`, `--out`, …), flags may follow the SQL or a subcommand, more than one SQL argument is an error, `-f` on `script`/`migrate` is refused, and `-f csv` points at `-t csv`.

- **N-034** · raised `2026-0924-1725-conn-mgmt-session-discard-timeouts` ·
  closed 2026-09-24, 2026-0924-1744-conn-cancel-dead-sessions-retire-tview-ui — `Session.Classify` asks the driver whether the connection is still usable after any error (`driver.Validator` for MySQL/SQLite/bytdb, `IsClosed` for pgx, which has no Validator), so a connection cut mid-statement is caught at once: stateful → `ErrSessionLost`, `ErrBadConn` on a stateless session → retry once, possibly-sent on a stateless session → drop without retrying. Tested with a fake driver (`db/fault_test.go`); the pgx branch waits on N-035.

- **N-033** · raised `2026-0924-1725-conn-mgmt-session-discard-timeouts` ·
  closed 2026-09-24, 2026-0924-1744-conn-cancel-dead-sessions-retire-tview-ui — each connect runs under its own context: Ctrl+K cancels one still dialing, Ctrl+C cancels instead of quitting, quitting abandons it; a newer pick cancels an older connect and a generation counter drops the older outcome, which could previously land last and switch the connection back.

- **N-025** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-24, 2026-0924-1744-conn-cancel-dead-sessions-retire-tview-ui — the classic tview UI is removed: `ui/` (~5,300 lines), the `-ui` flag, `DBC_UI` and `envOr`; `go mod tidy` dropped tview, tcell, `gdamore/encoding`, `x/term`. The history and buffer copies went with it (`userdata/` keeps the same on-disk format), as did its host-palette copy (`theme.FromHost` is the only one now).

- **N-007** · raised `2026-0731-2016-feature-items-from-the-review-backlog` ·
  closed 2026-09-24, 2026-0924-1744-conn-cancel-dead-sessions-retire-tview-ui — moot: every lint site (`ui/modals.go`, `ui/catsagents.go`) went with the classic UI (N-025).

- **N-026** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-24, 2026-0924-1458-assistant-schema-context — schema context for the assistant. `db.TableIndex` word-matches the statement (lexed, so strings and comments don't count) and the question against the catalog the sidebar already holds; the chip forecasts "schema of …" live; on Enter `Manager.Columns` runs one catalog query per driver (`db.ColumnsQuery`: `pg_attribute`+`format_type` for Postgres and bytdb, `column_type` for MySQL, `pragma_table_info` for SQLite) and `ai.Build` sends one line per table, up to 8 tables and 80 columns each, without `ai_rows`. bytdb views went without columns until bytdb v0.16.0 (bytdb's N-017); dbc now pins v0.16.0, where they report their columns and `information_schema.tables` lists them, so `db.TablesQuery` uses the Postgres query for bytdb too. Live-server check is N-032.

- **N-024** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-24, 2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy — `tui` is the default; `-ui classic` / `DBC_UI=classic` keeps the tview UI. Retiring it is N-025.

- **N-023** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-24, 2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy — the `tui` package: Bubble Tea v2, drawn on a cell canvas so draw and hit-test share one layout; click-to-focus, drag selection in editor and grid, right-click menus, wheel, scrollbars, draggable pane borders, header sort, cats glue ported.

- **N-022** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-24, 2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy — `71281f6` (`ai` package: ACP client, Copilot default, rows only with `ai_rows`) and the assistant pane in `tui/chat.go` (streaming, model/agent picker, context chip, ⤓ insert, Ctrl+K stop).

- **N-021** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` ·
  closed 2026-09-24, 2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy — `20be4e4`: `clip` writes text and HTML flavors together (macOS NSPasteboard via JXA, Windows CF_HTML, Linux wl-copy/xclip) and `export.HTMLFragment` renders a self-styled table; the new UI's right-click and ⧉ Copy menus offer HTML table, Markdown, CSV, TSV, JSON for a range or the whole result.

- **N-002** · raised `2026-0728-2000-stmt-under-cursor-and-query-cancel` ·
  closed 2026-09-24, 2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy — the new status bar drops key hints in tiers rather than truncating (`tui/layout.go`, `drawStatus`), and Stop moved to the toolbar. The classic UI keeps the old behavior until it is retired (N-025).

- **N-001** · raised `2026-0728-1923-dbc-tui-db-client` ·
  closed 2026-09-24, 2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy — the Bubble Tea UI's inspector: double-click a cell or press Enter (`tui/modals.go`, `inspectModal`); JSON is pretty-printed, the value wraps and scrolls, with copy and ask-the-assistant buttons.

- **N-020** · raised `2026-0807-2148-bytdb-v0.9.1-and-dual-demo-defaults` ·
  closed 2026-09-24, 2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy — "`~/Library/Caches/dbc/demo.bytdb` is real; deleting it
  is safe." A note, not a task; README (`README.md:65`) documents the demo
  file's location and that it persists.
- **N-019** · raised `2026-0913-2158-dbc-migrate-replaces-goose` ·
  closed 2026-09-24, 2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy — Push dbc so `go install …@latest` delivers `migrate`.
  `main` is even with `origin/main` at `f35f520`. The tagging half stays open
  as N-011.
- **N-018** · raised `2026-0731-1930-low-hanging-fruit-review-and-fixes` ·
  closed 2026-09-24, 2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy — Persist the editor buffer across sessions. Done in
  `5104d9e` (2026-07-31, `ui/buffer.go`), though no session doc writes it up
  and `ai_docs/fable_recommends.md` still shows it without a ✅.
- **N-017** · raised `2026-0728-1923-dbc-tui-db-client` · closed 2026-09-24 —
  Per-connection query history. Query history landed in `72b487a`: each entry
  records its connection and the `Ctrl+P` filter matches on the connection
  name. History is one file across connections, not scoped per connection; if
  scoping is wanted, raise it as a new item.
- **N-016** · raised `2026-0728-1923-dbc-tui-db-client` · closed 2026-09-24 —
  gopls flags the module when the editor workspace is rooted elsewhere. Editor
  setup advice (open dbc as its own project), not a change to dbc.
