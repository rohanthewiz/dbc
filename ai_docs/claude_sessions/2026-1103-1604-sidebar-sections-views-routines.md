# Sidebar sections: tables, views and routines

Session: `d7841054-1847-4250-9b27-a4dc25405bb4`

## Ask

From the cats-todo backlog: "I want to also see, in addition to tables,
views and routines." Mid-session: "But don't introspect for views and
routines until they are asked for."

## Starting point

Both already existed, half hidden. Views were mixed into the Tables list
with a dim "view" tag, and routines sat behind a toggle (TUI `f`, web
"Routines" heading word) that swapped the list for them. When asked, the
user picked **grouped sections**: one list with headed, foldable Tables,
Views and Routines sections, and the DDL of a view as well as a routine.

## What was built

### Fold model (`workspace/sections.go`)

- `Section` (`tables`, `views`, `routines`), `ParseSection`,
  `SectionOpen`, `OpenSection`. As a tab starts, Tables is open and Views
  and Routines are folded.
- The Routines fold *is* the existing `ShowRoutines` switch, so routines
  are read only once the section opens (the lazy read the user asked for).
  Tables and Views folds read nothing: both come in the one catalog read,
  so they are only per-tab memory (`tablesFolded`, `showViews` on
  `Workspace`).
- `Workspace.ViewDDL(db.TableRef)` → a `*ViewDDL` event. It refuses a
  table, or a name the catalog no longer lists.

### View DDL (`db/viewddl.go`)

- Postgres and bytdb read `pg_class` + `pg_get_viewdef(oid, true)`, and the
  CREATE is rebuilt around it: `CREATE OR REPLACE VIEW`, or
  `CREATE MATERIALIZED VIEW` for relkind `m`. Names are quoted only where
  needed (`pgIdent`). bytdb has no `information_schema.views`, but it does
  serve pg_class relkind `v` with `pg_get_viewdef` (checked against
  bytdb v0.21.2's source and a scratch db).
- MySQL uses `SHOW CREATE VIEW` (column 2). SQLite uses `sqlite_master.sql`.
- Names go in as literals or quoted identifiers, never bare.

### TUI (`tui/sections.go`, plus edits)

- Section headers are pickable rows holding `sectionRow`, marked `▾` or `▸`
  with a count (Routines has no count until read). Rows are indented under
  their header with a blank mark. An empty open section shows a note
  ("no views", "loading routines…").
- Keys: `t`, `v` and `f` fold the three sections. Enter, or a single click,
  on a header folds it. `D` shows a view's DDL, which is also in the
  right-click menu. The DDL viewer (`ddlModal`) now takes a qname and a
  heading, so routines and views share it.
- The find (`/`) keeps a header only while its section has a match. The
  cursor lands on the first name, never a header, on a fresh list, a first
  relist and each re-narrowing.
- `tableOnly` now refuses only on a routine's row. The pane title counts
  names, not headers (`countNames`). Help text is updated.

### Web

- `sideState` gains `tablesOpen` and `viewsOpen`.
- New endpoints: `POST /api/v1/ws/:id/section` (routines goes through
  `ShowRoutines` with its "routines" event) and `POST …/viewddl`. The "ddl"
  event carries `view` for a view.
- `app.js drawTables` builds the three sections (`tableRow`, `routineRow`).
  A header click, Enter or Space folds it, the arrows skip notes, `D` shows
  a view's DDL, and the menus fold sections (`foldItems`). Row counts patch
  only table rows.
- The heading's Tables/Routines words became a plain "Catalog". The find
  box reads "name".
- The CSS adds `li.shead` headers and indented rows, and drops the
  `(view)` suffix.

## Interpretation left open

The view *names* still ride in the one catalog query, which is how the
folded Views header can show a count. Splitting them out would take views
away from completion, `erd --views` and the designer until the section
opens. This was raised as N-244 rather than done.

## Testing

- New tests: `db.TestViewDDLQuery`, `TestPgViewDDL`, `TestManagerViewDDL`
  (bytdb and SQLite through the Manager);
  `workspace.TestSectionsAndViewDDL`; `web.TestSidebarSections`;
  `tui.TestTablesPaneSections`.
- Older TUI tests that indexed `m.tables.items[0]` or clicked row Y+1 now
  use `tableNames`/`nameIndex` helpers (`tui/sections_test.go`) and row
  Y+2. `TestLiveRoutinesPanePostgres` was rewritten for the section flow.
- Web e2e: a new step, "sidebar sections: tables, views, routines", passes,
  and `pgRoutines` was rewritten for headers (live Postgres only, not run).
  The full suite passed on its third run. The first two failed at
  `editor_menu`, which passes alone, as does `build_badge`, which failed on
  a clean `main` worktree. Raised as N-245.
- `go vet ./...` and full `go test ./...` pass with CATS_* stripped.
- Not run: the live Postgres and MySQL tests, because Docker was down
  (N-243).

## Notes

- `db/indexes_test.go` and `sqlsplit/folds.go` show in `gofmt -l`. Both
  were already that way and are untouched.

## Commits

- The work, this doc and the next-list edit, together.

## Next

Closed: None. Declined: None. Raised: N-243, N-244, N-245.
Deferred: None. Promoted: None. Moved: None. Updated: N-087.
Full list: `ai_docs/todo/next-list.md`.
