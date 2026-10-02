# SQL completion from the schema (TUI and dbc web)

Session: `44909f5c-6f06-42f1-a4e4-a0cb9df8df74`

## Ask

"Let's add intellisense to our query console based on table DDLs" (from the
cats-todo backlog). Added mid-way: "include Postgres keywords and typical
functions too". At the end: `/sess-wrap`.

## What was built

Schema-aware completion in **both** editors, from one Go engine. It follows
the rule the statement marker already follows (`POST /api/v1/stmt`): the
logic lives in Go, not in a JavaScript copy that would drift from the
terminal's.

```
editor buffer + caret
   │
   ├─ TUI: Model.openCompletion ──────┐
   └─ web: POST /api/v1/ws/:id/complete┤  (UTF-16 ⇄ byte offsets in web/complete.go)
                                       ▼
                       workspace.Complete  ── cache cold? → ready=false
                                       │      UI runs LoadCompletions off its loop
                                       ▼
                 sqlcomplete.Complete(schema *erd.Schema, driver, focus, buf, caret)
```

### `sqlcomplete/` (new, pure, tested without a database)

- `complete.go`: the request/result types, the statement window (via
  `sqlsplit.Split`/`IndexAt`), no suggestions inside strings, comments or
  quoted names, and context detection from the token before the caret.
  It also builds the candidate sets, ranks and filters them (prefix, then
  word boundary, then subsequence), and caps the list at 400 (`Incomplete`).
- `scope.go`: a tokenizer on top of `sqlsplit.Lex`. It reads the statement's
  table refs and aliases (FROM/JOIN/UPDATE/INTO/USING/TRUNCATE, comma lists,
  ONLY/LATERAL, derived tables) and its CTEs. CTE columns come from the
  explicit list or the SELECT list: aliases, `t.col`, and `*`/`t.*` expanded
  from the subquery's FROM. Also generates aliases for joins (`order_items`
  → `oi`) and renders DDL docs (`CREATE TABLE … -- → customers(id)`).
- `vocab.go`: the dialects. Postgres (shared by bytdb) has its keywords,
  clause keywords, types and about 130 functions with signatures: aggregates,
  window, string/regex, date/time, JSON/JSONB, array, FTS and admin. MySQL
  and SQLite get the common core plus their own functions. Quoting rules:
  Postgres folds case, so a capital needs quotes; MySQL uses backticks;
  reserved words are quoted. Tables in `public` (or the only schema) are
  inserted bare, others qualified.

The contexts are: statement start, tables (FROM ▮), JOIN (whole FK join
clauses first), ON (the FK condition between the table just joined and the
others), qualified (`o.▮`, `schema.▮`, `schema.table.▮`), expression (scope
columns, qualified when ambiguous; with no scope, a catalog of up to 3,000
columns offers all of them), after a comma (FROM list or expression list),
after `(` (INSERT target columns, or SELECT for a subquery), `::` and
`CAST(x AS ▮` (types), and a clause after a finished phrase.

Keywords follow the case being typed and are inserted **without** a trailing
space, so Enter after a fully typed keyword stays a new line (Monaco
"smart", and the TUI's equivalent). A function puts the caret between its
parentheses (`Item.Cursor`; 0 is read as "not set").

### `workspace/complete.go`

- `Complete(buffer, caret) (Result, ready)` never blocks; `LoadCompletions(ctx)`
  blocks (bounded by `CompletionTimeout` = `DiagramTimeout`), and concurrent
  callers wait on the load already in flight. Both run on the pool, not the
  session, like `Diagram`.
- The cache is keyed by connection plus `complGen`. `complGen` is bumped in
  `setCatalogLocked` (connect, schema pick, disconnect) and in `landRun` for
  any DDL verb (`create/alter/drop/rename/comment/attach/detach`) or any
  script, failed runs included.
- A failed load is remembered for 5 minutes (`complRetry`), so a too-big
  catalog is not re-read on every keystroke; meanwhile completion offers the
  vocabulary alone.

### dbc web

- `web/complete.go`: `POST /api/v1/ws/:id/complete`. On a cold cache it loads
  inside the request, then answers. `note` reports a failed load once.
- `editor.js`: `registerCompletion()` adds a provider for sql, pgsql and
  mysql, with `.` and `:` as trigger characters. It turns `cursor` into a
  `$0` snippet (escaping `$ } \`), maps kinds to icons, and shows DDL docs as
  SQL markdown. Editor options changed: `quickSuggestions` is on except in
  strings and comments, `wordBasedSuggestions: "off"` (the word-based noise
  was why it was off before), `acceptSuggestionOnEnter: "smart"`.
  `dbc.editor.warm(conn)` (called from `markActive` in `app.js`) preloads the
  schema on a connection change. The F1 help lists the keys.

### TUI

- `tui/complete.go`: the popup, placed under the word, flipped above when
  there is no room, with the selected item's doc box beside it. It opens on
  Ctrl+Space, after `.` or `::`, and on the second letter of a word, and
  follows the typing while open. ↑/↓/PgUp/PgDn move, Tab picks, Enter picks
  only when that changes the text, Esc closes. An app chord (Ctrl/Alt) or a
  move closes it, a click on a row picks it, and a click elsewhere closes it.
  A cold cache issues `LoadCompletions` as a tea.Cmd (`complLoadedMsg`) and
  reopens only if the caret still wants it.
- `complPopup.ver` ties the popup to `editor.version`: a history pick, tab
  switch or paste makes it stale, and `complLive()` closes it.
- `editor.Replace(from, to, s, cursor)` makes the pick one undo step.
- Added a "Suggest…" item (^Space) to the editor's right-click menu and
  `^Space suggest` to the startup key hints.

### Docs

The README has a new "Completion" section (what is suggested where, the
dialect vocabularies, quoting, keys), plus Ctrl+Space in both key tables.

## Verification

- `go vet ./...` and `go test ./...` are green. New tests:
  - `sqlcomplete` (16 cases on a fake Postgres catalog: alias columns,
    prefix range, FROM tables, schema qualifier and quoting, ambiguous-column
    qualification, FK join clauses and ON conditions, CTE columns including
    `*`, INSERT columns, literals and comments, statement boundaries, PG
    functions/types/CAST/ILIKE, the clause after a table, quoting, no schema,
    match quality)
  - `workspace` (cache cold, then loaded, kept across SELECT, dropped by
    CREATE TABLE and reloaded with the new table; no connection gives the
    vocabulary)
  - `tui` (opens on a dot and picks with Tab without moving focus,
    Ctrl+Space/Esc/space/Enter behavior, function caret, closes on SetText)
- `web/e2e` in headless Chrome passes all steps, including a new "editor
  completion" step: typing `c.` shows cats' columns in Monaco's suggest
  widget, `br` narrows it to breed, Tab gives `SELECT c.breed FROM cats c`.
  No JavaScript errors.
- A TUI frame was rendered and inspected: the popup sits under the caret,
  with the DDL doc box beside it.

## Design notes

- Lexical, not a parse: every table the statement names is in scope
  everywhere in it, subqueries included. This is a deliberate trade-off,
  documented in the package doc.
- `Item.Filter` lets a qualified column (`o.id`) still match the typed `id`,
  and it becomes Monaco's `filterText`.
- The sort key is `rank · match quality · label`. The server's order is kept
  in Monaco via `sortText`.

## Follow-up in this session: N-095

A schema pick no longer drops the completion cache. `Manager.Schema` reads
every user schema, so a pick changes only the ranking focus, which
`Complete` reads from `w.schema` on each ask. The drop moved out of
`setCatalogLocked` into `landConnect` (a reconnect still reads afresh) and
`Disconnect`. Covered by `TestCompletionCacheAcrossPickAndConnect`, which
fails on the old code, and by an extended `TestLiveWorkspacePickSchema`,
run against a throwaway database on a local Postgres 16 and dropped
afterwards.

## Follow-up in this session: N-096

bytdb now gets its own completion dialect. It keeps Postgres's keywords,
clauses and quoting, but offers only the functions bytdb actually
evaluates: aggregates, window, coalesce/nullif, lower/upper/length, the
now() family, gen_random_uuid, the array helpers, the sequence functions,
version/current_database/current_schema, and CURRENT_DATE/TIMESTAMP and
LOCALTIMESTAMP. Its types are limited to the casts that produce a real type
(any other name casts to text). The `pg_*_size` functions are left out
because bytdb stubs them to 0. `TestBytdbFuncsEvaluate` runs every offered
function against an embedded bytdb, and a probe showed the old list had
suggested functions bytdb rejects: trim, round, jsonb_agg, date_trunc.

## Next

Closed: N-095, N-096. Declined: None. Raised: N-094, N-095, N-096.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
