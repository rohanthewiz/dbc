# Show a table's information_schema.columns, ready to copy

Session: `9d52695d-e4b0-4ece-8dfc-cb87112a8672`

## Ask

From the user's cats-todo backlog: "I should have a quick way of seeing and
copying the information schema columns of a table."

## What was built

**Show columns** on a sidebar table, in both the TUI and `dbc web`:

- the right-click menu on a table: `Show columns`, after `Preview rows`
- the `c` key on a selected table in the tables list

It runs one statement that returns the table's `information_schema.columns`
rows into the results grid. The grid's existing copy menu then copies them as
TSV, CSV, Markdown, an HTML table or JSON. This reuses the grid instead of
adding a separate copy path.

It works like the double-click preview: the statement is recorded in the
history, since it is what the user would have typed, but it is not put in
the editor. The history is also the way to widen it to `SELECT *`. The tag
is `columns <name>`, and focus moves to the grid.

### The query: `db.InfoColumnsQuery(driver, TableRef)` (`db/catalog.go`)

The select list is the same on every driver: `ordinal_position`,
`column_name`, `data_type`, `is_nullable`, `column_default`,
`character_maximum_length`. These are the columns that all four drivers
provide. bytdb's `information_schema.columns` (v0.18.0, `sql/syscat.go`) has
exactly these plus `table_catalog`, `table_schema`, `table_name` and
`udt_name`. Every column is aliased to its own name in lower case, because
MySQL 8 reports `information_schema` column names in upper case.

| Driver | Source |
|---|---|
| Postgres | `information_schema.columns` `UNION ALL` a `pg_attribute` branch limited to `relkind = 'm'`, because the standard view leaves out materialized views while the sidebar lists them. Branch types are cast (`::int`, `::text`) so they match the info-schema domains. `ORDER BY 1`. |
| bytdb | `information_schema.columns` alone (no matviews) |
| MySQL | `information_schema.columns`, `table_schema = DATABASE()`, backslash-doubled literal |
| SQLite | `pragma_table_info('<name>')` renamed into the same shape (`cid + 1`, `"notnull"` → `YES`/`NO`, NULL length) |

`ColumnsQuery`, which feeds the assistant, is left as it was. It serves
another purpose: several tables at once, and DDL-style types from
`format_type` / `column_type`.

### Resolving the name: `TableIndex.Lookup(qname)`

The sidebar sends its display name: `schema.name` on a connection with
several schemas, bare otherwise. `Lookup` accepts either spelling. The
index's maps are lower-case, but a Postgres catalog can hold both `cats` and
`"Cats"`. So an exact-case match wins if it is unique. A case-insensitive
match is used only when it is the only candidate. A bare name in two schemas
is refused, not guessed.

### Wiring

- `workspace.ShowColumns(qname)` (`workspace/run.go`): resolves the name
  against the active catalog. A name that isn't there is refused (`Invalid`,
  400 on the web), because it would be inlined into SQL. It then builds the
  query and calls `RunStmts`.
- TUI: `tableColumns()` in `tui/sidebar.go`, the menu item in `tui/menu.go`,
  and the `c` key in `tui/app.go`'s `focusTables` case. `c` doesn't clash
  with anything: the list's own keys are movement and Enter.
- Web: `POST /api/v1/ws/:id/columns` → `handleColumns` (`web/grid.go`),
  shaped like `handlePreview` and reusing `previewReq`. In `app.js`: the
  `columns(name)` call, the menu item, a plain `c` in the list's keydown
  (chords like ⌘C are ignored), and the row tooltip mentions it.
- README: the tables row of the mouse table, plus a row for `c`.

## Verification

- `go vet ./...` and `go test ./...` pass.
- New tests:
  - `TestInfoColumnsQueryPerDriver` checks quoting and the Postgres matview
    branch.
  - `TestTableIndexLookup` checks case collisions, bare names in two schemas,
    qualified names and nil.
  - `TestInfoColumnsOnSqlite` and `TestInfoColumnsOnBytdb` run the query for
    real and check the shared column names, via the `infoRows` helper.
  - `TestShowColumns` (web) covers the route, the grid result, the history
    entry, and 400 for an injection-shaped name and for an unknown one.
  - `TestTablesSidebarShowColumns` (TUI) covers the `c` key, the result, the
    editor left untouched and focus on the grid.
- Added to the opt-in live tests but **not run**, because the Docker daemon
  was down: `TestLiveColumnsPostgres` runs it on a table and on the matview,
  and `TestLiveColumnsMySQL` on a table (N-062).
- The web page side was checked only with `node --check`, not in a browser
  (N-063).
- The demo `cats` table's columns are `id, name, breed, age, adopted`. The
  web test had guessed `owner_id` at first.

## Next

Closed: None. Declined: None. Raised: N-062, N-063.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
