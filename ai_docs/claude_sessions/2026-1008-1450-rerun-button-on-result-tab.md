# A ↻ on the result tab on screen reruns its query

Session: `50122616-ad6c-40b4-ba6e-630a1235a4d9`

## Ask

1. Dropped in from the cats-todo backlog: "I want to be able to rerun the
   query of any actively shown query result tab".
2. `/sw` (sess-wrap).

## Decisions

- **The ask was already met as worded.** Rerun shipped in 930f5a3
  (`2026-1008-0000-rerun-result-tab`): `r` with the results pane focused,
  or "↻ Rerun its query" on a tab's right-click menu. The backlog item was
  written about 13 hours later, so one question was asked: what is
  missing? The choices were a one-click ↻ on the tab, a rerun key that
  works from the editor, or nothing. **Answer: a one-click ↻ on the tab.**
- **Only the tab on screen gets a ↻.** A click on any other tab shows it,
  and then its ↻ is there. A ↻ on every label would cost each title two
  cells in a strip that shrinks titles to fit.
- **The click takes r's path.** A statement that may write still asks
  first, the sort is kept (N-158), and a pinned tab stays pinned. No new
  workspace or server code was needed.
- **A script's tab gets no ↻.** It has no statement, and `RerunResultTab`
  refuses it. A ↻ that only logs a refusal would be noise.
- **TUI: the ↻ has its own hit rect (`layout.rtabRerun`), not a chip.**
  `rtabChips` means "a click shows this tab", and a test counts them.
  The rect covers the glyph's cell only, so a click on the space beside it
  only focuses the results.
- **TUI: the write confirm opens at the click.** `r` still opens it at the
  grid's cursor. `rerunCurResultTab` now picks that spot and calls the new
  `rerunCurResultTabAt(x, y)`. The menu keeps itself on screen when drawn,
  as the strip's right-click menu (also opened on the border row) relies on.
- **TUI: the compact form carries it too:** ` ‹ 2/7 ↻ › `. It is the
  first thing dropped when that does not fit, since the steps matter more
  and `r` still reruns.
- **TUI styling:** the current tab's colour (focus-aware), bold, not
  underlined. It reads as that tab's, but as a control beside the label
  rather than part of it.
- **dbc web: the ↻ sits before the ×.** It is always visible on the
  current tab (the × is too). The click looks the tab up in the strip as
  last drawn rather than trusting the attribute.

## Changes

- **`tui/resulttabs.go`**
  - `rtabPart.rerun`, and the `rtabRerun` constant (`"↻ "`).
  - `resultTabParts` adds the ↻ after the current label when the tab has a
    `Stmt`, in the label forms and the compact form.
  - `drawResultTabs` styles it and records `m.lay.rtabRerun`, using the
    glyph's cell offset (`width` of the prefix, not a byte index).
  - New `resultTabRerunAt`.
  - `rerunCurResultTab` is split; the new `rerunCurResultTabAt(x, y)` holds
    the body.
  - The file comment's strip diagram shows the ↻.
- **`tui/layout.go`**: new field `rtabRerun Rect`.
- **`tui/mouse.go`**: a left click on the ↻ focuses the results and calls
  `rerunCurResultTabAt` at the click. It is checked before the strip's
  chips.
- **`tui/help.go`**: the `r` row mentions the ↻.
- **`web/static/js/app.js`**
  - `drawResultTabs` adds `span.rr[data-rerun]` to the current tab when it
    has a `stmt`. Its tooltip warns when the statement writes.
  - The strip's click handler sends a ↻ click to `rerun`.
  - The tab tooltip and the help row mention ↻.
- **`web/static/css/app.css`**: `.rstrip .rt .rr`, plus a hover style.
- **README**:
  - a mouse-table row for the ↻;
  - the `r` row mentions it;
  - the strip diagram shows it;
  - the Rerun paragraph names it.

## Tests

- `TestRerunByClick` (tui) covers:
  - there is exactly one ↻, right after the current tab's cut label;
  - on an INSERT's tab, a click opens the confirm at the click's
    coordinates, and "Keep the result" runs nothing;
  - "Run it again" runs it;
  - on the pinned count tab, a ↻ click refreshes it in place: still
    pinned, 2 rows, focus on the grid, and the "rerun … completed" log line.
- `TestResultTabPartsRerun` (tui) covers:
  - the ↻ in the wide form;
  - no ↻ on a script's tab;
  - the compact form ` ‹ 1/3 ↻ › ` at width 12;
  - the ↻ dropped at width 9.
- e2e `resultTabsPerConn`: after the `r` step, it checks that there is one
  `.rr` and that it is on `.rt.on`. Clicking it gives a new seq in the
  same pinned tab, still sorted.
- `go test ./...` (CATS_* stripped) and the full `DBC_E2E=1` suite pass.
  `gofmt` also realigned a comment in `TestRerunKeepsTheSort`, left
  unformatted by the previous commit.
- Not seen in a real terminal: N-163.

## Next

Closed: None. Declined: None. Raised: N-163.
Deferred: None. Promoted: None. Moved: None.
Updated: N-159. Full list: `ai_docs/todo/next-list.md`.
