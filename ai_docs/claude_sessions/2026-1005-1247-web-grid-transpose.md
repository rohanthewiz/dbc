# Web grid: transpose a result (rows vertical) and copy it for Teams

Session: `5a7e89b3-ab5d-4f7f-947c-e599859be5e6`

## Ask

From the cats-todo backlog: "I want the ability to transpose a result set
so rows are vertical. Allow me to export at least the html js version of
this (for copy into say Teams)." Done in `dbc web` (the HTML/JS UI); the
TUI is left as a follow-up (N-111).

## What it does

- `t` in the grid, the new **⇄ Transpose** toggle on the results bar
  (`aria-pressed`), or the grid menu's "Transpose (each row a column)"
  turns the grid on its side, as psql's `\x` does: one column per record
  (headed by its display number), one line per result column (its name
  in the sticky gutter). `t` again turns it back. The info line says
  "transposed".
- Copies and exports follow the orientation. **Table for Teams** pastes
  the transposed table with the names as row headers (`<th scope="row">`
  in the header's colors); one record comes out `column | value`.
  ⤓ Export (HTML page, CSV, Markdown, JSON, …) downloads it transposed.
  Plain `y` / `Y` copy values on their side. Menu heads say "transposed"
  when the output will be.
- Sorting, hiding, inspect, ranges all work as upright. A click on a name
  sorts; a click on a record number selects that record; arrows follow
  the screen (→ next record, ↓ next column); `=` fits the record width.
- A tab's orientation is in its grid snapshot, and survives a rerun.

## Design

- **Only the picture turns.** The cursor, range, sort and hidden set stay
  in data coordinates (row = record, col = result column), so no grid
  feature needed a second implementation. `drawFlip`, `hit`,
  `ensureVisible` and the arrow keys swap axes (`web/static/js/grid.js`).
- **Uniform record width** (widest shown column's capped auto width), so
  the records in view are found by division: a 50,000-row result is
  50,000 columns transposed.
- **Server**: `viewReq.Transpose` (`transpose` in the copy body, `t=1` on
  export). `slice` projects as upright; then `export.Transpose(r, first)`
  turns the piece, `first` being the display number of its first row so
  "row 17" in the copy is row 17 in the grid. Plain copy uses
  `export.PlainCellsTransposed` (values only).
- **`model.Result.Transposed`** marks a turned result; only the two HTML
  renderings (`HTMLFragment`, `htmlDoc`) read it, to draw column 0 as row
  headers. `summary` counts the statement's rows, not the lines, for a
  transposed result (the page's meta line said "2 rows" for a 5-row
  result before the fix).
- Fixed on the way: `hit` judged the gutter on content x, so with the
  grid scrolled sideways a click on a row number hit the cell under it.
  Now judged on screen x against the grid's left edge, both orientations.

## Files

`export/transpose.go` (new), `export/transpose_test.go` (new),
`export/fragment.go`, `export/export.go`, `model/result.go`,
`web/grid.go`, `web/grid_test.go`, `web/static/js/grid.js`,
`web/static/js/app.js` (keys help), `web/static/css/app.css`,
`web/pages/workbench.go` (the button), `web/e2e/web_test.go`, `README.md`.

## Verification

- `go test ./...` green (CATS_* stripped).
- New handler test `TestGridCopyAndExportTransposed`: transposed CSV of a
  range with grid numbers, plain copy, one-record HTML (`value`, row
  headers, NULL kept), export HTML page (5 rows, hidden column absent).
- New e2e step "transpose the grid" (`cd web/e2e && DBC_E2E=1 go test`):
  `t` toggles, names in the gutter, →/↓ move as on screen, `y` copies
  `Mia`, a record-number click selects the record and `y` copies
  `3\nLeo\n1`, Copy → Table for Teams puts `text/html` with `value` and
  row headers on the clipboard, `t` turns it back. All 20 steps pass.
- Screenshot checked by eye: names in accent down the left, records
  across, toggle lit.

## Next

Closed: None. Declined: None. Raised: N-111, N-112.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
