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

**Next ID:** N-036

## Open

- **N-003** · raised `2026-0728-2022-multi-statement-headless` · value low
  Headless multi-statement output is collected and rendered at the end, not
  streamed per statement, so a long `text` run shows nothing until it
  finishes. Still true (`main.go:286`); `-o` and the single-document HTML/JSON
  formats want the collected shape, so streaming would be `text`-only.

- **N-004** · raised `2026-0728-2022-multi-statement-headless` · value low
  Headless: no `-tx` flag to wrap the whole buffer in one transaction, and no
  continue-on-error mode. Neither exists today.

- **N-005** · raised `2026-0728-2022-multi-statement-headless` · value medium
  Headless SQL comes only from the argument — no SQL file and no stdin
  (nothing in the tree reads `os.Stdin`). Note `-f` is already the output
  format flag, so a file input needs another spelling (`-i file`, or `-`
  meaning stdin). `dbc "$(cat f.sql)"` is the workaround.

- **N-006** · raised `2026-0728-2022-multi-statement-headless` · value low
  A TUI "run all" key that runs the whole buffer through the multi-statement
  path. The TUI runs the statement under the cursor (or the selection) by
  design; this is contingent on that design changing.

- **N-007** · raised `2026-0731-2016-feature-items-from-the-review-backlog` · value low
  Linter modernizations: the `f := f` loop copy at `ui/modals.go:169` is
  redundant under `go 1.26.1`, and the hand-rolled `min` (`h := n + 4; if h >
  20 { h = 20 }`) is at `ui/modals.go:179` and now also
  `ui/catsagents.go:199`.

- **N-008** · raised `2026-0731-2200-dbc` · value medium
  `INSERT … RETURNING` (and `UPDATE`/`DELETE … RETURNING`) shows
  `rows_affected`, not the returned rows. `isQuery` (`db/manager.go:297`)
  keys off the leading verb only, so this hits Postgres and bytdb alike. The
  fix is cross-driver: detect a `RETURNING` clause (via `sqlsplit`, so one in
  a string literal doesn't count) or fall back to Query when Exec is wrong.

- **N-009** · raised `2026-0807-2148-bytdb-v0.9.1-and-dual-demo-defaults` · value low
  The shipped scripts hard-code the connection name `demo` —
  `scripts/export_report.go`, `scripts/loop_params.go` and
  `testdata/show_two.go` — which is why the SQLite demo keeps the name `demo`
  instead of a symmetric `demo-sqlite`. Only matters if that rename is wanted.

- **N-010** · raised `2026-0807-2148-bytdb-v0.9.1-and-dual-demo-defaults` · value low
  With no config, every headless invocation seeds both demos
  (`db.SeedDemos`, `main.go:103`), so a bare `dbc "SELECT 1"` opens the bytdb
  file. Lazy seeding on first use of a connection is the fix if it ever
  matters.

- **N-011** · raised `2026-0913-2158-dbc-migrate-replaces-goose` · value medium
  Tag a release. The push half of this item is done (N-019). The repo has no
  tags at all, while `cats-plugin.toml` declares `version = "0.1.0"`, and the
  church docs point at `go install github.com/rohanthewiz/dbc@latest` (which
  works today only as a pseudo-version of `main`). A `v0.1.0` tag would line
  the three up.

- **N-025** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` · value low
  Retire the classic tview UI (`ui/`, `-ui classic`) once the Bubble Tea UI
  has earned it. Retiring it also removes the duplicates it keeps: history and
  buffer (now in `userdata/`), the host-palette mapping (now
  `theme.FromHost`), and N-007's lint items, which live only in `ui/`. The
  user's call, not a default — it is the way back if the new UI misbehaves.

- **N-027** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` · value low
  Keep assistant conversations: save each to `~/.config/dbc/chats/` and offer
  recent ones, as ced's `internal/chatstore` does (stdlib-only, portable).
  Today ⟲ new and quitting discard the transcript.

- **N-028** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` · value low
  Copilot sign-in from inside dbc. Today an unsigned-in user is told to sign
  in from another editor; ced shows the device-flow code itself by running
  the language server in LSP mode (`signIn` → `workspace/executeCommand`).
  Contingent on someone using dbc without ced or another Copilot editor.

- **N-030** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` · value low
  Linux rich copy offers text/html only (wl-copy/xclip set one type), so a
  terminal paste right after gets nothing or markup. Serving both needs
  owning the selection; only worth it if a Linux user trips on it.

- **N-031** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` · value low
  Grid column resize by dragging a header border, and hiding columns. Widths
  are auto-sized (capped at 40 cells) and the inspector shows a full value.

- **N-032** · raised `2026-0924-1458-assistant-schema-context` · value low
  Run the assistant's schema lookup (`db.ColumnsQuery`) against a live
  Postgres and MySQL. The Postgres query is the one bytdb runs in tests
  (`pg_attribute` + `format_type`), and MySQL's reads
  `information_schema.columns.column_type`, but neither has met a real
  server. A failure is graceful — the transcript says the lookup failed and
  the question goes without schema — so this is confidence, not a fix.

- **N-033** · raised `2026-0924-1725-conn-mgmt-session-discard-timeouts` · value low
  Opening a connection from the sidebar can't be canceled. `tui/run.go`
  `connectCmd` and the classic UI's `setActive` call `mgr.DB(name)`, which
  uses a background context, so only `connect_timeout` bounds the wait. With
  `connect_timeout = "0"`, an unreachable host leaves "connecting…" up until
  the OS's TCP timeout (over a minute). Fix: give the connect a context that
  Ctrl+K/Esc cancels, and pass it to `mgr.DBContext`.

- **N-034** · raised `2026-0924-1725-conn-mgmt-session-discard-timeouts` · value low
  When the server cuts a pinned session's connection, the first failure is
  usually a plain network error (the statement had already been sent, so
  pgx doesn't report `ErrBadConn`), not `db.ErrSessionLost`. That run fails
  with the driver's own words, and "session lost" appears only on the run
  after. Nothing is ever replayed, so this is about the wording, not
  correctness. A fix would treat network errors on a stateful session as
  lost too.

- **N-035** · raised `2026-0924-1725-conn-mgmt-session-discard-timeouts` · value low
  Test the session changes against a live MySQL and Postgres. The MySQL
  driver's session reset leaving an open transaction on a pooled connection
  was confirmed by reading `go-sql-driver/mysql` v1.10.0; the tests used
  SQLite and bytdb only. Worth checking against real servers: that closing a
  session ends its transaction, that the stateful and stateless dead-session
  paths behave, and `conn_idle_timeout`/`connect_timeout`.

## Roadmap

Wanted, but deliberately not next. Empty at seeding: the session docs never
marked an item as deferred, so sorting Open items into here is the user's
call.

- **N-029** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` · value medium
  Verify the Windows rich clipboard (`clip/rich_windows.go`) on a real
  Windows machine. It cross-compiles and vets, and its CF_HTML envelope is
  unit-tested, but it has never pasted into Teams from Windows.

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
