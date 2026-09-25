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

**Next ID:** N-041

## Open

- **N-004** · raised `2026-0728-2022-multi-statement-headless` · value low
  Headless: no `-tx` flag to wrap the whole buffer in one transaction, and no
  continue-on-error mode. Neither exists today.

- **N-006** · raised `2026-0728-2022-multi-statement-headless` · value low
  A TUI "run all" key that runs the whole buffer through the multi-statement
  path. The TUI runs the statement under the cursor (or the selection) by
  design; this is contingent on that design changing.

- **N-009** · raised `2026-0807-2148-bytdb-v0.9.1-and-dual-demo-defaults` · value low
  The shipped scripts hard-code the connection name `demo` —
  `scripts/export_report.go`, `scripts/loop_params.go` and
  `testdata/show_two.go` — which is why the SQLite demo keeps the name `demo`
  instead of a symmetric `demo-sqlite`. Only matters if that rename is wanted.

- **N-011** · raised `2026-0913-2158-dbc-migrate-replaces-goose` · value medium
  Tag a release. The push half of this item is done (N-019). The repo has no
  tags at all, while `cats-plugin.toml` declares `version = "0.1.0"`, and the
  church docs point at `go install github.com/rohanthewiz/dbc@latest` (which
  works today only as a pseudo-version of `main`). A `v0.1.0` tag would line
  the three up.

- **N-028** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` · value low
  Copilot sign-in from inside dbc. Today an unsigned-in user is told to sign
  in from another editor; ced shows the device-flow code itself by running
  the language server in LSP mode (`signIn` → `workspace/executeCommand`).
  Contingent on someone using dbc without ced or another Copilot editor.

- **N-030** · raised `2026-0924-1422-ui-revamp-mouse-ai-assistant-rich-copy` · value low
  Linux rich copy offers text/html only (wl-copy/xclip set one type), so a
  terminal paste right after gets nothing or markup. Serving both needs
  owning the selection; only worth it if a Linux user trips on it.

- **N-032** · raised `2026-0924-1458-assistant-schema-context` · value low
  Run the assistant's schema lookup (`db.ColumnsQuery`) against a live
  Postgres and MySQL. The Postgres query is the one bytdb runs in tests
  (`pg_attribute` + `format_type`), and MySQL's reads
  `information_schema.columns.column_type`, but neither has met a real
  server. A failure is graceful — the transcript says the lookup failed and
  the question goes without schema — so this is confidence, not a fix.

- **N-035** · raised `2026-0924-1725-conn-mgmt-session-discard-timeouts` · value low
  Test the session changes against a live MySQL and Postgres. The MySQL
  driver's session reset leaving an open transaction on a pooled connection
  was confirmed by reading `go-sql-driver/mysql` v1.10.0; the tests used
  SQLite and bytdb only. Worth checking against real servers: that closing a
  session ends its transaction, that the stateful and stateless dead-session
  paths behave (including `Session.Classify`'s pgx branch, which asks
  `pgx.Conn.IsClosed` and has no test without a live connection), and
  `conn_idle_timeout`/`connect_timeout`.

- **N-038** · raised `2026-0924-1958-assistant-conversation-archive` · value low
  No way to delete the live assistant conversation. Saved ones can be
  deleted (right-click a row, or `d` twice in Recent conversations), but the
  one on screen is never offered, since its next save would write it back.
  A "Delete this conversation" row in the transcript's menu would remove its
  file and clear the pane without saving.

- **N-040** · raised `2026-0925-headless-streaming` · value low
  A headless `dbc script` still collects what it `s.Show`s and renders it
  at the end, while its `s.Print` lines stream to stdout in `text` — so the
  log runs ahead of the results it describes. Streaming them (N-003's
  `blockStream`) needs a banner that does not know the total, since a
  script's result count is not known up front (`#2` instead of `2/5`).

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

- **N-003** · raised `2026-0728-2022-multi-statement-headless` ·
  closed 2026-09-25 — a headless multi-statement query streams: each result
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
  that ran (`2/2`). Checked through the binary with a 1.7s recursive CTE as
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
