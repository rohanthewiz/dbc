# Grid columns: resize by dragging a header border, hide and show

Session: 34872b5d-f31e-4a31-b7df-6d5f825e6433
Date: 2026-09-24

## Ask

Next-list item N-031, pasted as-is: "Grid column resize by dragging a header
border, and hiding columns. Widths are auto-sized (capped at 40 cells) and
the inspector shows a full value."

## Design

- **Display columns.** Grid coordinates now use *display* columns, the same
  way rows already went through `order`. `grid.cols` maps display column →
  result column (the visible ones, in result order). The cursor, the range,
  hit-testing, hover, `value`/`isNull` and `sub` (every copy) all take
  display columns, so hiding a column means rebuilding one slice, not adding
  a skip test to every walk.
- **Facts about the data stay per result column.** `widths` (auto),
  `userW` (set by hand, 0 = auto), `numeric`, `hidden` and `sortCol` are all
  indexed by result column, so they survive a column being hidden and shown.
  Hiding the sorted column keeps the sort.
- **`rebuildCols`** keeps the cursor, the anchor and `leftCol` on the same
  *result* columns across a rebuild. A column that was itself hidden moves
  to the next visible one to its right, like deleting a spreadsheet column.
- **Copies and exports take the view.** A hidden column is left out of a
  cell, row, range or whole-result copy, and out of the export dialog's
  clipboard and file paths. That enables "hide the noisy columns, then copy
  the table into Teams". A whole-result copy says so in the log:
  `the result (8 rows, 1 column hidden)`.
- **The file export now uses `grid.Selected(true)`,** as its clipboard button
  beside it already did. It keeps every row, but now follows the grid's sort
  order and leaves out hidden columns. Before, it wrote `m.lastRes` as-is.
- **The layout survives a re-run** when the new result's columns are
  identical (names and order): hidden columns and hand-set widths carry
  over, because edit-the-WHERE-and-re-run is the common loop. Any other
  result starts fresh.
- **The last visible column can't be hidden.** An empty grid reads as a
  failed query, and no header would be left to right-click to undo it.

## What changed

### `tui/grid.go`

- New fields: `cols`, `hidden`, `userW`, `hoverB` (the hovered border),
  `resizeCol`/`resizeX` (the drag in progress).
- `minColWidth = 3`, `maxUserWidth = 400`. Auto-sizing still caps at
  `maxColWidth = 40`; `Fit` doesn't.
- `SetResult` split out `contentWidth` (uncapped measure) and `colWidth`
  (hand-set width, else auto). It clears `cols` before rebuilding, because a
  stale map put the cursor past column 0 when the previous result had
  leading hidden columns. Found in review; the test covers it.
- New: `resultCol`, `colName`, `rebuildCols`, `displayOf`, `HiddenCount`,
  `HiddenCols`, `Hide(c0, c1)`, `Show(rc)`, `ShowAll`, `Resize`, `Fit`,
  `startResize`, `resizeTo`, `hiddenAfter`.
- New `hitBorder`: the separator cell right of a header, tested before the
  column spans, so a click one cell to its left still sorts.
- Drawing: the hovered border is `┃` in accent (motion during a drag doesn't
  update hover, so the highlight follows the border being dragged). A gap
  where hidden columns sit is `║` in accent, including before the first
  column, on the gutter rule. The strip shows `· N hidden`.

### `tui/mouse.go`

- `dragGridCol`. A header-border press starts a resize, and a double-click
  on it fits. Hover sets `hoverB`.
- Right-clicking a header moves the cursor onto that column, unless the
  column is inside the range, in which case the menu hides the range's
  columns. It sets `cur.col` directly, since `moveTo` does nothing on a
  zero-row result.

### `tui/app.go`, `tui/menu.go`

- Grid keys: `<`/`>` narrow or widen by 2, `=` fits, `-` hides (the cursor's
  column or the range's), `+` shows all. `hideColumns`/`showAllColumns` log
  what they did and why a hide was refused. The startup key hint mentions
  `-/+`.
- New `columnItems` in the grid menu: "Hide column <name>" (or "Hide N
  columns"), "Fit column to its content", then a "hidden columns" heading
  with "Show <name>" per column (at most 8, then "… and N more") and "Show
  all columns".

### `tui/modals.go`, `tui/editor.go`

- The inspector gets its column name from `grid.colName` instead of
  `lastRes.Columns[in.col]`, which would be wrong once columns are hidden.
- Export to file goes through `grid.Selected(true)`.
- `plural(n, noun)` helper added next to `itoa`.

### Tests, docs

- `grid_test.go`: hide/show remapping (sort by a hidden column holds, header
  `║`, strip count), refusing to hide everything, a range copy skips hidden
  columns, resize clamps and `Fit` passes the cap, the layout survives a
  re-run and resets on different columns.
- `app_test.go`: dragging a border resizes (plus hover, no sort,
  double-click fits), right-clicking a header hides and "Show breed" brings
  it back (the row copy leaves it out), and the `-`/`+` keys including the
  refusal.
- README: rows added to the mouse and key tables, plus a paragraph on hidden
  columns being a view that copies and exports honor.

`go vet ./...` and `go test ./...` pass.

## Next

Closed: N-031. Declined: None. Raised: N-036.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
