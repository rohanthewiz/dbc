# ERD on a catalog too big to read whole (N-105)

Session: `b598e383-4cb8-4c90-b5a7-567fa32bf895`

## Ask

N-105 from the next list: `Workspace.Diagram` still read every schema
(`Manager.Schema`), which can fail with `db.ErrCatalogTooBig` on a big
database. Completion already falls back to `Manager.SchemaIn`
(`2026-1005-1109-completion-big-catalog`); give the diagram the same
fallback with `sel.Schema` plus the schemas of the tables `sel` names.
Done in worktree branch `todo/n-105-scope-the-erd-s-schema-rea-8cbd`,
merged into local main.

## Change (`workspace/diagram.go`)

- `Diagram` now reads through `readDiagram`: the whole schema first; on
  `ErrCatalogTooBig` (Navigable drivers only, i.e. Postgres) it marks the
  connection in `Workspace.complScoped` and reads `SchemaIn` over
  `diagramScope(sel, path)`. Each read gets its own `DiagramTimeout`.
- `complScoped` is now shared with completion (both read the same
  `Manager.Schema`), so whichever meets the overflow first saves the other
  the failing whole read. Comments updated in `workspace.go` and
  `complete.go`.
- `diagramScope`:
  - everything in a schema (no Tables, Schema set) → that schema alone.
    The search path is not read nor added: `public` is often the biggest,
    and its tables would be dropped by `Select` anyway.
  - around named tables → `complScope(sel.Schema, path)` plus the schema
    of each `schema.table` name, as spelled and lower-cased (`Find`
    matches case-insensitively, the catalog query exactly).
  - neither → the search path (public when unknown).
- Documented limits of a scoped diagram: a `Depth` neighbour in a schema
  outside the scope is missed (`SchemaIn` drops keys leaving the scope),
  and a bare name unique in the scope but not in the database resolves
  rather than being refused as ambiguous.

## Tests

- `TestDiagramScope` (scope rules), `TestDiagramRememberedTooBig` (a
  connection marked too big still draws on SQLite, where scope is ignored).
- Live (`postgres:17` container, `DBC_LIVE_BIG=1`):
  `TestLiveWorkspaceCompletionBigCatalog` now also clears the too-big
  mark, draws a diagram of `public` past the bound (whole then scoped,
  ~335ms), checks the mark is set and a new `public` table is in it, and
  that a diagram around `dbc_live_wsbig.t1` fails too big (that schema
  alone is past the bound). The rest of the PG live tests in `./workspace`
  and `./db` pass.
- After the merge into main: `go test ./...` all pass.

## Next

Closed: N-105. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
