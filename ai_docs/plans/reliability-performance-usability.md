# Reliability, performance and usability: a review of all of dbc, and the order to fix what it found

Raised 2026-11-10. The ask: take a deep look at dbc and come up with a plan to
improve reliability, performance and usability.

This is a plan. Nothing in it is built yet. Every item is on the living list
(`ai_docs/todo/next-list.md`) as **N-247 … N-305**, one entry per commit-sized
piece; the list holds each item's detail (where, what triggers it, the fix
direction), and this file holds the order, the reasoning, how each claim was
checked, and what was looked at and found sound. In the list the items end
with "(RPU plan, phase N)".

## The one-paragraph version

dbc is in good shape at its core: build, `go vet` and `go test -race ./...`
pass on all 33 packages that have tests, warm startup is about 20 ms, and the
parts that were hardened on purpose (the connection manager, the retry rule,
`etl.Copy`, run records, the scheduler, the saved-connections file) held up
under a second reading. What the review found is at the edges those passes
did not reach: a statement splitter that follows Postgres's comment rules on
every engine and so can cut a MySQL `DELETE` away from its `WHERE`; a
completion panic one keystroke away; a pipeline source that commits before
its sink has loaded; editor text that is saved only on a clean quit; and a
set of places where the cost of a frame, a keystroke or a truncated result
grows with data the user never sees. The plan is five phases: first the
bugs that lose data or kill the process (small fixes), then reliability
hardening, then performance, then usability, then the guardrails (fuzz
targets, benchmarks, lint, live tests in CI) that would have caught most of
phase 1.

## How it was checked

The review ran on 2026-11-10 against `main` at `2dece91` (v0.13.2, about
111k non-test lines of Go and 17k of hand-written JavaScript).

- **Run here:** `go build ./...`, `go vet ./...` and
  `go test -race -count=1 ./...` (all pass; `tui` and `erd` take about 46 s
  each). The headless CLI in a scratch HOME: error output, exit codes, the
  `max_rows` cut. Two-line repros of the MySQL `#` split, the trigger split
  and the completion panic.
- **Read here:** the claims behind N-251, N-254, N-255, N-256, N-257,
  N-258, N-261, N-265, N-270, N-273, N-274, N-276, N-288, N-289 and N-292
  were confirmed against the code named in each item.
- **Six area audits** (query core; TUI; web; ETL, pipelines, jobs and
  scripts; config, persistence and CLI; SQL parsing and completion) read
  their packages and reported with file and line. The SQL audit also *ran*
  its findings: repro programs, timing tables, and about 1.4 million fuzzed
  calls across `designer.Import`, the explain parsers, completion, resolve,
  rename, folds, `codehl` and `snippet`. The other five traced code and
  driver source (pgx v5.10.0, go-sql-driver/mysql v1.10.0) and ran nothing.

Each list item says which of these it rests on: *reproduced*, *read*,
*audit, run*, *audit, traced*, or *suspected*.

**Not checked:** nothing ran against a live Postgres or MySQL, so every
claim about driver behaviour (a `rows.Close()` that drains, a cancel that
closes the connection) is from the driver's source, not from a run. `dbc
web` was not rendered in a browser; N-300's topbar overflow is from the CSS
alone. No static analysis beyond `go vet` ran: the local `staticcheck` was
built with Go 1.24 and refuses a Go 1.26 module, and running a current one
was not permitted in the review session (N-304).

## Phases

### Phase 1 — data loss and crashes

Small fixes, each its own commit, in this order. The splitter comes first
because everything multi-statement depends on it and its worst case runs a
`DELETE` without its `WHERE`.

| ID | What | Where | Evidence |
|---|---|---|---|
| N-247 | MySQL `#` comments and per-dialect comment rules in the splitter | `sqlsplit/sqlsplit.go:490-520` | reproduced |
| N-248 | Compound bodies (triggers, procedures, `BEGIN ATOMIC`) split at their inner `;`; `DELIMITER` unknown | `sqlsplit/sqlsplit.go:507` | reproduced |
| N-249 | A UTF-8 BOM stays in the first statement | `sqlsplit/sqlsplit.go:77` | audit, run |
| N-250 | Completion panics when `.` is a statement's first token | `sqlcomplete/complete.go:357` | reproduced |
| N-251 | A pipeline's `sql.read` commits before the sink has loaded: a move loses rows | `pipeline/builtin_sql.go:151` | read |
| N-252 | Preview runs and commits a writing source | `pipeline/run.go:784-838` | audit, traced |
| N-253 | `sql.write` with `truncate` into a table the fragment reads hangs on Postgres | `pipeline/builtin_sql.go:271-275` | audit, traced |
| N-254 | The TUI saves editor text only at quit or a tab change; no SIGHUP handler; quit saves last | `tui/app.go:395-401` | read |
| N-255 | Consoles, the buffer and the history trim are written in place; `writeAtomic` has no fsync | `userdata/buffer.go:40` | read |
| N-256 | One statement over 1 MiB stops history loading from that line on | `userdata/history.go:99` | read |
| N-257 | Quit and a connection switch do not ask about an open transaction | `tui/app.go:939-975` | read |
| N-258 | A script result with a short row panics the grid | `tui/grid.go:305` | read |
| N-259 | `designer.Import` recursion has no depth cap: a stack overflow kills dbc web | `designer/importparse.go:1080` | audit, run |
| N-260 | dbc web's background goroutines have no `recover` | `web/hub.go:508-523` | audit, traced |
| N-261 | The web idle reaper cancels running statements and says nothing | `web/hub.go:273-281` | read |
| N-262 | The formatter splits operators it does not know and glues two signs into `--` | `sqlsplit/format.go:403,965` | audit, run |
| N-263 | A nested script run releases the outer script's Readers and Writers | `script/engine.go:154` | audit, traced |
| N-264 | `explain.Detect` takes an ordinary result for a plan | `explain/detect.go:252-296` | audit, run |

N-251, N-252 and N-253 are one piece of work in `etl` (a held read exported
for the stream path, and the same-database rules moved from `Copy` into the
Writer), landed as three commits: fixing N-251 alone makes N-253 hang
always instead of only on big tables, so N-253 goes in right behind it.

### Phase 2 — reliability hardening

| ID | What | Where | Evidence |
|---|---|---|---|
| N-265 | dbc web: a failed tab or console save is silent; no local draft for consoles; the pagehide save can exceed the keepalive quota | `web/static/js/app.js:3948,3995` | read / audit |
| N-266 | dbc web: a dropped stream event is never replayed; log lines live only in the DOM | `web/hub.go:164-176` | audit, traced |
| N-267 | Headless: no `--max-rows`; a failed stdout write exits 0; no word when no config was found | `main.go:953,970` | reproduced / audit |
| N-268 | Jobs: no backstop for a node that ignores cancel; a second Ctrl+C is swallowed | `jobs/job.go:234`, `main.go:1106` | audit, traced |
| N-269 | Jobs: overlap rules hold inside one process only | `jobs/engine.go:471` | audit, traced |
| N-270 | Migrations: no runner lock; MySQL reports a rollback its DDL already committed past | `migrate/run.go:262-315,445-462` | read / audit |
| N-271 | Stop on Postgres always destroys the session; a stopped EXPLAIN ANALYZE of a write reads as "session lost" | `db/manager.go:364-377`, `db/explain.go:215-224` | audit, traced |
| N-272 | `Session.stateful` misses state set by a "read" (`set_config`, advisory locks, `PRAGMA x = y`, `SELECT … INTO`) | `db/manager.go:554-565` | audit, traced |
| N-273 | Unknown TOML keys are ignored; a literal `$` in a password cannot be written | `config/config.go:374,779-801` | read / audit |
| N-274 | dbc web checks Origin on non-GET only | `web/auth.go:116` | read |
| N-275 | Completion requests can fill the browser's six connections while the schema loads | `web/complete.go:62-69` | audit, traced |

### Phase 3 — performance

N-276 is the one with a daily effect: `SELECT *` on a very large table
keeps "running" after its 1,000 rows are in hand, until the server has sent
the rest.

| ID | What | Where | Evidence |
|---|---|---|---|
| N-276 | A result cut at `max_rows` still streams the whole result; so does an early `Reader.Close` | `db/manager.go:700`, `etl/reader.go:211` | read (drain is driver behaviour, not run) |
| N-277 | Cell cost follows the value's length in both grids; 100 KB cells go to the browser whole | `tui/grid.go:968`, `web/grid.go:177` | audit, traced |
| N-278 | The TUI redoes buffer-sized work on every frame, mouse moves included | `tui/editor.go:846`, `tui/logpane.go:128`, `tui/chat.go:1011` | audit, traced |
| N-279 | Each keystroke re-lexes the whole buffer and copies it for undo | `tui/editor.go:961-987,298` | audit, traced / timed |
| N-280 | Completion builds every table's doc before filtering: 32–34 ms at 5,000 tables | `sqlcomplete/complete.go:733-747` | audit, run |
| N-281 | The formatter is O(tokens × nesting depth): 27 s on 289 KB | `sqlsplit/format.go:987-1004` | audit, run |
| N-282 | A result is held twice, an export four times; exports and sorts run inside `Update` | `db/manager.go:707-715`, `main.go:956` | read / audit |
| N-283 | Postgres row counts are all-or-nothing, and a timeout is cached as "no numbers" | `db/rowcount.go:506-516` | audit, traced |
| N-284 | dbc web: whole-buffer round trips per typing pause, `/stmt` per caret move, assets re-hashed per sign-in page, results kept 24 h | `web/static/js/app.js:3896`, `web/server.go:534` | audit, traced |
| N-285 | Every headless job or pipeline run parses every run record | `jobscmd.go:156`, `jobs/records.go:158` | audit, traced (cost suspected) |

### Phase 4 — usability

| ID | What | Where | Evidence |
|---|---|---|---|
| N-286 | Headless errors print as raw log lines; an unknown connection lists no names; a mistyped subcommand runs as SQL | `main.go:1116-1120` | reproduced / audit |
| N-287 | CLI discoverability: no `dbc conns`, no shell completion, no `DBC_CONFIG`/`DBC_CONN`, one exit code for connect and SQL failures | `main.go` | audit, grep |
| N-288 | Values lose detail on the way out: fractional seconds, dates, floats, bytea, CSV NULL, NaN in JSON | `db/manager.go:1098-1120`, `export/export.go` | read / audit |
| N-289 | `CALL`, `FETCH`, `EXECUTE` and MySQL `CHECK/ANALYZE TABLE` show "0 affected" instead of their rows | `db/manager.go:528-531` | read |
| N-290 | `Manager.Columns` is the one catalog read still cut at `max_rows` | `db/catalog.go:601` | audit, traced |
| N-291 | A failed query's message is only in the log; the grid keeps the previous result | `workspace/run.go:800-802`, `tui/layout.go:179` | audit, traced |
| N-292 | No find in the TUI's editor, grid or log; no key for sort | `tui/editor.go`, `tui/grid.go` | read (grep) |
| N-293 | About 130 unfiltered F1 rows, no command palette, fixed status-bar hints | `tui/help.go`, `tui/layout.go:595` | audit, traced |
| N-294 | Selections and log levels are colour-only, so they vanish under `NO_COLOR` | `tui/editor.go:921`, `tui/grid.go:959` | audit, traced |
| N-295 | Load errors drop Postgres's key, row and column detail | `etl/writer.go:146,607` | audit, traced |
| N-296 | A transform before the sink and no rows: `truncate` is skipped and the run reports success | `pipeline/run.go:539-551` | audit, traced |
| N-297 | No dry run on `dbc copy` or `dbc migrate`; a job re-run always starts at the root | `copycmd.go:72-81`, `migrate.go:40-52` | audit, traced |
| N-298 | Completion offers nothing inside an open quoted name, and the wrong things after `DROP COLUMN` and in `CREATE INDEX (` | `sqlcomplete/complete.go:272` | audit, run |
| N-299 | One reserved-word list for all engines: the designer and completion emit `rank`, `groups`, `transaction` bare | `designer/catalog.go:454-493` | audit, run (SQLite); MySQL by its list |
| N-300 | dbc web: modals do not trap focus, the grid has no roles, the status is not a live region, the topbar overflows at half width, a failed grid page has no retry | `web/static/js/ui.js:90-111` | audit, traced (overflow suspected) |
| N-301 | A plan with hundreds of sibling nodes draws as a 20000-pixel strip | `explain/picture.go:406-456` | audit, run |

### Phase 5 — guardrails

These are tests and CI, with no product change, so on the list they sit in
Validate.

| ID | What |
|---|---|
| N-302 | Fuzz targets for the splitter, formatter, completion and `designer.Import`. There are none; an ad-hoc run found N-250 in minutes. |
| N-303 | Benchmarks with budgets for split, lex, complete, format and one TUI frame. There are three benchmarks in the repo, all in `script`. |
| N-304 | A linter in CI, built with the module's Go. |
| N-305 | The live Postgres and MySQL suites in CI on service containers. They are opt-in and local today. |

N-302 is worth doing alongside phase 1 rather than after it: a fuzz target
written with the N-247/N-248 fixes is what keeps them fixed.

## Checked and found sound

So the next review does not re-read them.

- **Connection manager** (`db/manager.go`): single-flight open, a waiter
  retrying after the opener cancels, Close during a dial, in-memory
  anchors. `mu` is not held across I/O; `mu` and `sessMu` are taken in one
  order.
- **Retry rule**: `Classify`/`onSession` replays only on
  `driver.ErrBadConn` from a stateless session; no path replays a statement
  that may have been sent (N-272 is about what counts as stateless).
- **MySQL kill** (`db/mysqlkill.go`): no stray `KILL` after a stale-idle
  discard; double-kill is guarded.
- **Generation guards** on connect, schema pick, counts, routines and run
  landings; the completion caches and the row-count cache.
- **`etl.Copy`**: one-transaction load with create and truncate inside,
  the source committing after the load, rollback on a fresh context, no
  pipe deadlock on a server failure. Its hot paths (the Postgres text
  encoder, prepared and bind-capped INSERT batches) allocate nothing per
  row.
- **Fragment loop**: one goroutine, no channels, so no send/receive
  deadlock; sinks abort in a defer; file sinks use temp + fsync + rename.
- **Run records, signals, scheduler**: temp + rename, one writer per file,
  heartbeat → "interrupted", prune spares live records, a PID-reuse guard.
  SIGINT and SIGTERM both cancel and finalise the record. DST skip and
  repeat, miss grace, `catch_up` and the `O_EXCL` fire claim all behave.
- **Scripts**: panics are recovered for scripts, `go.*` nodes and plugin
  load and Check; the run context is cancelled at run end.
- **Saved connections and secrets** (`config/saved.go`, `secret.go`):
  locked read-modify-write, temp + fsync + rename, `0600`, authenticated
  encryption, fail-closed TLS. No DSN is attached to an error.
- **AI client**: every call has a timeout; rows go only under a
  connection's `ai_rows`.
- **TUI threading**: no command closure writes the Model; late events for
  a closed tab are dropped; Bubble Tea recovers panics in Update, View and
  Cmds and restores the terminal; tiny terminals and empty results are
  guarded; nothing reads disk in draw.
- **dbc web**: the Host, Origin and secret guard (N-274 is the one gap),
  constant-time compares, CSP; handler panics are recovered by rweb; grid
  paging and virtualisation; `resync()`; shutdown; multi-window claims;
  hashed, immutable assets.
- **Splitter and scanners**: Postgres tagged dollar quotes, `E''`, nested
  comments, unterminated strings and comments, a trailing statement with no
  `;`, CRLF. Split, Lex and Folds are linear (3.5 MB in 11, 20 and 45 ms).
- **Fuzzing so far**: `designer.Import` + `Analyze` took 604k mutated
  inputs with no panic; the four explain parsers 200k+; completion,
  resolve, rename, folds, `codehl` and `snippet` about 610k calls with
  N-250 the only finding.
- **Designer output**: literal and LIKE escaping per dialect; numeric,
  date and UUID values validated.
- **ERD**: 1,000 tables with 1,516 foreign keys lay out and route in 1.0 s.

## Noted, not raised

- A `./dbc.toml` in the working directory wins and its DSNs expand
  environment variables, so an untrusted checkout could name
  `${GITHUB_TOKEN}` in a DSN to its own host. A trust prompt would fix it;
  it is a design risk nobody has met, in a tool kept deliberately local,
  and it is left for the author to weigh.
- Static files are served uncompressed (Monaco is 3.7 MB). It only matters
  on a non-loopback bind.
- `pruneChats` re-parses every chat file after each answer, and dbc web's
  in-memory history is never capped. Both are small.
- The parallel fragment driver, commit-every-N with resume, retries with
  backoff and row-level error handling are already under "Later" in
  `ai_docs/plans/pipelines.md`.
- N-099 (noticing a dropped connection) and the minimal web auth stay as
  decided; nothing here reopens them.

## Working it

- One commit per item, the N-number in the subject, as the rest of the
  list is worked.
- N-251 to N-253, N-270, N-271, N-272 and N-276 touch sessions, cancel or
  transactions: run the live suites (`DBC_LIVE_PG_DSN`,
  `DBC_LIVE_MYSQL_DSN`) for those, and add a live test for the case each
  one fixes. N-271 changes what `workspace/live_test.go:407` pins.
- N-247, N-248 and N-262 change what a buffer splits or formats into: the
  repro inputs in each item are the first test cases.
- N-254, N-257, N-291, N-292 and N-293 change what the TUI shows: check
  them in the real binary (`tui/e2e`), with every `CATS_*` variable
  stripped.
