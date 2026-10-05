# Web grid: resize the transposed grid's records and names (N-112)

Session: `5a7e89b3-ab5d-4f7f-947c-e599859be5e6`

## Ask

"do N-112": the transposed web grid (added earlier this session, see
`2026-1005-1247-web-grid-transpose`) gave every record one auto width and
capped the names gutter at 40 characters, with no way to resize either.

## What it does

- Every record header has a resize handle on its right edge. Records
  share one width, so dragging any record's border widens them all.
  Double-click fits it to the widest shown value (the existing flip `fit`).
- The `column` corner has a handle for the names gutter: drag to any
  width (past the 40-char auto cap), double-click to fit the longest
  name plus room for the sort arrow (`fitNames`).
- Both widths (`recFit`, `fnFit`) live in the tab's grid snapshot and
  reset only when the result's columns change, like upright hand widths.
- A drag's release is not a click: it neither selects a record nor sorts.

## Design

The one shared width makes a naive drag feel wrong: widening record j by
Δw also widens records 0…j-1, so its border would move (j+1)·Δw while the
pointer moved Δw. `flipResizeMove` compensates by scrolling: scrollLeft =
sl0 + j·Δw keeps record j's left edge fixed on screen, so its right border
tracks the pointer. The body/head width is grown before the scroll is set
(drawing waits for the next frame, and the browser would clamp the scroll
to the old width). When the browser cannot give that scroll (a result
narrower than the pane, or narrowing near an end), one correction pass
shares the drag over the j+1 widths: W = w0 + (dx + s − sl0)/(j+1), with
s the scroll the browser settled on. Widths are whole pixels, since
`drawFlip` divides by them.

`view()` now also reports `recW` / `fnW`, for tests.

## Files

`web/static/js/grid.js` (`flipResizeStart`, `flipResizeMove`, `fitNames`,
handles in `drawFlip`, `fnFit` state), `web/static/css/app.css` (`.hrn .rz`),
`web/e2e/web_test.go` (drag checks + `handleCenter`, `drag`, `evalNum`
helpers), `README.md`.

## Verification

e2e `cd web/e2e && DBC_E2E=1 go test -count=1 .`: all 20 steps pass.
"transpose the grid" now checks a 40px drag of record 2's border on a
3-record result (width +20, border follows the pointer within 2px, the
selection unchanged), a 40px drag on a 200-record result scrolled 30
records in (width +40, border follows), and the names gutter dragged +30
then double-clicked back to fit. Two harness pitfalls hit on the way: the
handle's center is 3.5px inside the border (compare border to border), and
a rod double-click needs a click first, then `Click(…, 2)`.

Also in this commit: another session moved N-109 from Open to Roadmap in
`next-list.md`; not this session's edit, committed as found.

## Next

Closed: N-112. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
