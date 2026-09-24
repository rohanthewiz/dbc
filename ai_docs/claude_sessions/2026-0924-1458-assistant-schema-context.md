# Assistant schema context (N-026), Windows item to Roadmap

Session: 3aeffd39-14dc-4c46-9631-a0a32f86926d
Date: 2026-09-24

## Ask

1. `/sl` — load the last session doc and the living next list.
2. "In the next list move any Windows (OS) items to the Roadmap, then do
   N-026" (schema context for the AI assistant).
3. Add the bytdb view-columns gap found along the way to bytdb's roadmap.

## What happened

### Next list: Windows to Roadmap

Only N-029 (verify the Windows rich clipboard on a real machine) is
Windows-specific; it moved from Open to Roadmap unchanged.

### N-026: the assistant now sends the schema of the tables involved

Design, in three layers:

- **`db/catalog.go`**
  - `TableRef` / `TableRefs(rows)` read `TablesQuery`'s rows.
  - `TableIndex` (built once per connect from the catalog the sidebar
    already loads) with `Mentioned(sql, prose)`: a *word* match, not a
    parse. SQL words come from `sqlsplit.Lex`, so strings, comments,
    numbers and params don't count and quoted identifiers are unquoted in
    place (`"main"."cats"` → `main.cats`). Prose is split on word
    boundaries only (an apostrophe is not a string). A dotted word matches
    only as `schema.table` (last two parts of `db.schema.table`), never
    falling back to its last part — `c.name` is alias.column. Order of
    first mention, statement before question. `Display` qualifies names
    only when the catalog has more than one schema (same rule as the
    sidebar).
  - `ColumnsQuery(driver, tables)`: Postgres **and bytdb** share
    `pg_attribute ⋈ pg_class ⋈ pg_namespace` with
    `format_type(atttypid, atttypmod)` (declared types, not
    `USER-DEFINED`/`ARRAY`), pairs as an OR chain rather than a row-value
    IN; MySQL `information_schema.columns.column_type`; SQLite
    `sqlite_master ⋈ pragma_table_info`. Names inlined as escaped literals
    (placeholder syntax differs per driver; MySQL also doubles `\`).
  - `Manager.Columns(ctx, conn, tables)` runs it on the pool (never inside
    the user's open transaction) and returns columns parallel to `tables`,
    nil for a table the catalog no longer describes.
- **`ai/prompt.go`**: `Context.Tables []Table{Name, View, Columns}`.
  `Build` writes one line per table under "Tables involved, from the
  database's catalog", before the query; capped at 80 columns/table
  ("… and N more"). The note lists `schema of a, b` (or `schema of N
  tables` past three). Tables without columns are noted but not written —
  that is how the chip forecasts before the lookup. Schema goes **without
  `ai_rows`**: it is shape, not contents (added to the data-rule comment).
- **`tui/chat.go`**: `chatContext(question)` fills table names from
  `m.tableIdx` (≤ 8 tables); the chip passes the composer text, so it reads
  "with: schema of owners, cats" while typing. Submit became two-step:
  `chatSubmit` → (tables?) `schemaCmd` (3s timeout) → `chatSchemaMsg` →
  `finishSubmit`; the turn is marked in flight from the Enter, the message
  is tagged with the chat generation (dropped after ⟲ new), a lookup error
  is shown and the question goes without schema, and a dead agent during
  the lookup just clears the busy flag. No cache: columns change under
  ALTER, and one catalog query per question is cheap.
- README's "What is sent" and the pane's empty-state hint document it.

### bytdb gap recorded

bytdb's `pg_attribute` and `information_schema.columns` iterate
`userDescs()` only, so views (listed in `pg_class` with synthetic oids) have
no columns — still true at bytdb HEAD (v0.15.0), not just dbc's pinned
v0.9.1. Recorded as bytdb's **N-017** (Roadmap) in
`~/projs/go/bytdb/ai_docs/todo/next-list.md` (uncommitted there; that
repo's `ai_docs/todo/` was already untracked). dbc's N-026 closing line
cross-references it.

## Verification

- `go test -race ./...` green.
- New tests: `TestMentionedTables`, `TestColumnsQueryPerDriver`,
  `TestColumnsOnSqlite` (incl. a view and a missing table),
  `TestColumnsOnBytdb`; ai `TestSchemaGoesWithoutAIRows`,
  `TestSchemaWithoutColumnsIsOnlyNoted`, `TestSchemaIsCapped`; tui
  `TestAssistantSendsTheQuerysSchema`,
  `TestAssistantFindsTablesNamedInTheQuestion` (chip forecast + prompt),
  `TestAssistantContextOffSendsNoSchema`,
  `TestAssistantSkipsATableThatIsGone`. Existing assistant tests now also
  exercise the lookup path (their query names `cats`), including the
  queued-during-handshake one.
- **Not verified on a live Postgres or MySQL** (none running locally) —
  raised as N-032.

## Gotchas worth remembering

- A bare `cat > file` with no heredoc blocks on stdin and hangs the shell
  call until timeout; it got moved to the background and had to be stopped.
- bytdb v0.9.1 rejects `varchar(n)`/`numeric(p,s)` in CREATE TABLE
  ("unknown column type"); scratch schemas for it should use
  `text`/`int`/`float`.
- The first chat turn carries the preamble, so "question only" prompts
  still start with it — assert with a suffix.

## Next

Closed: N-026. Declined: None. Raised: N-032.
Deferred: N-029. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
