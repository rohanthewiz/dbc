# Numeric columns sized from every row (N-052)

Session: f4f14471-9584-42ea-854c-d60f07e21fc5
Date: 2026-09-25

## Ask

Next-list item N-052: column auto-widths are measured from the first 500
rows (TUI grid and web grid alike), so a numeric column that widens later
shows "10…" at row 1,000. Size numeric columns from every row.

## Changes (`0374c85`)

| Piece | Change |
|---|---|
| `workspace/view.go` | `WidestNumeric(r, c)`: the widest display value of a numeric column over every row. A number's text is ASCII, so this is a `len()` per cell. It measures the widest text rather than using min/max: a float's text is not monotonic in its value (`0.123456789` is wider than `1000`). NULLs count as `NULL`, which is how both grids draw them. |
| `tui/grid.go` | `contentWidth` uses `WidestNumeric` for columns in `g.numeric`; text columns keep the `widthSample` (500) loop. The border double-click (fit) goes through `contentWidth`, so it covers every row for numbers too. |
| `web/grid.go` | `colWidths(r, numeric)` does the same. It had run on every page fetch; the widths are now computed once per result and cached in `resultView` (`widths`, `content`) next to the sort order, so the full scan does not repeat per scroll step. `grid.js` uses the server's widths as-is. |
| tests | `TestGridSizesNumericColumnsFromEveryRow` in `tui` (synthetic 1,000-row result) and `web` (a 1,000-row recursive CTE; the test env's `MaxRows` is 1000). `1000` gets width 4; a long text value after row 500 still does not count. |

## Verified

- `go test ./tui ./web ./workspace ./export` green (CATS_* stripped); build,
  vet, gofmt clean.
- With the three source files stashed (old code), both new tests fail at
  width 3.

## Next

Closed: N-052. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
