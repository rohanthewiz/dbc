# N-277: grid cells cost what they show, not the value's length

Session: `2f348a07-d9a3-42f1-8e6f-8fc647277b34`

## Ask

"continue with N-277" (RPU plan, phase 3). In both grids a cell's cost
followed the length of its value. TUI: `truncate(flatten(val))` ran per
visible cell per frame. `flatten` built a `NewReplacer` and copied any
value with a newline, `truncate` measured the whole string before
cutting, and `valuesWidth` did the same for 500 rows on arrival. Web:
`handleResult` sent every value whole, and `grid.js` ran `flat()` and
`esc()` over each visible cell's full string per scroll frame. The
item's fix: clip to a head before flattening and measuring, make
`truncate` exit early, clip cells server-side (~500 runes plus a flag),
and fetch the full value for Inspect and copy. Then `/sw`.

Commit: `5536adb` (one commit for the item).

## Measured first

New benchmarks in `tui/gridcell_test.go`, over a result whose three text
columns hold a ~100 KB multi-line JSON-ish document each. The "before"
numbers were run on HEAD in a throwaway worktree with only the benchmarks
copied in.

| | before | after |
|---|---|---|
| one frame, 60 rows, 200×50 (`BenchmarkGridFrameLongCells`) | 217 ms, 47.7 MB | 0.35 ms, 0.36 MB |
| arrival, 500 rows (`BenchmarkGridArrivalLongCells`) | 4.9 s, 986 MB | 1.5 ms, 4 KB |

Arrival was the bigger cost. The item had only suspected it ("does the
same for 500 rows on arrival"). `SetResult` measures each text column
twice (`contentWidth` and `valuesWidth`), and every value was flattened
and grapheme-walked whole.

The intermediate steps are worth keeping. A head clipped before
flatten + width alone gave 0.54 ms and 52 ms (68 MB, the head's flatten
copy). Counting flatten's widths during the walk instead of copying gave
15 ms. Measuring only to the cap the caller clamps to gave 1.5 ms.

## What was built

### TUI

- `truncate` (`tui/canvas.go`) is one pass over the graphemes, stopping
  at the first one past `w` (`cutAt`). It gives the same results as the
  old measure-then-walk version (checked by a random test against a copy
  of the old one). `uniseg.StringWidth` is the sum of
  `FirstGraphemeClusterInString` widths, so the two agree by
  construction.
- `cutAt(s, w, more, flat)` returns the byte length drawn and whether a
  "…" follows. `more` means s is only a head, so a "…" follows even when
  it fits. `flat` counts each grapheme as flatten draws it, so the cut is
  found before flattening.
- `tui/grid.go`:
  - `cellText(s, w)` takes `valueHead(s, cellBytes(w))`, finds the cut
    with `flatWidth`, and flattens only the part drawn.
  - `cellBytes(w)` = 16 bytes a cell. Real text stays under it: CJK is 3
    bytes in 2 cells, a flag 8 in 2, a three-person ZWJ family 18 in 2,
    CRLF 2 bytes in 1. A first try at 8 a cell was too tight for ZWJ
    families.
  - `valueHead` cuts back to a rune start.
  - `flatWidth` gives LF, CR, CRLF and tab one cell each. uniseg counts
    them as 0, and they always stand alone as graphemes.
  - `cellTextWidth(s, limit)` measures width with no copy and stops at
    `limit`.
  - `valuesWidth`/`contentWidth` take a `limit`: `maxColWidth` from
    `setResult`, `maxUserWidth` from `Fit` and `fitRecords`. Every
    caller clamps to that cap anyway, so the widths come out the same.
  - flatten's Replacer is a package var (`flattener`).
- The upright draw and the transposed `drawFlip` both use `cellText`.
  Inspect, copy and export still read `grid.value` whole.

### Web

- `web/grid.go`:
  - `cellClip = 500` runes, past the page's `MAX_CH` (400).
  - `handleResult` clips with `clipRunes` and lists cut cells in
    `resultPage.Cut` as `[row in Cells, result column]`. A list, because
    cuts are rare.
  - Uncut cells still point into `r.Rows` (no copy). A short row's
    missing cell is sent blank, as the TUI's `grid.value` draws it.
    Before, it would have indexed past the row.
  - `colWidths` counts runes only up to `cellClip`.
- New `GET /api/v1/ws/:id/cell?seq=&sort=&desc=&row=&col=` → `{value}`
  (null for NULL). It is checked like a copy: 409 if the seq moved, 400
  out of range or with no result. It is registered next to `/result`,
  so N-274's origin check covers it.
- `grid.js`:
  - `g.pages` holds `{cells, cut}` (`pageOf`), so the rows and their cut
    set can't drift apart.
  - `isCut(pg, r, rc)` drives a closing "…" in both `draw` and
    `drawFlip`. The CSS `text-overflow: ellipsis` covers the usual case.
    The appended "…" covers a head that happens to fit, e.g. CRLF-dense
    text.
  - `inspect` is async. For a cut cell it fetches `/cell` with the
    grid's seq and sort before opening, and its "Copy value"/`y` then
    copies the whole value.
  - `cellAt` was the only other reader of page cells. Copy and export
    were already rendered server-side.

## Found on the way

- The TUI's Inspect dialog re-wraps its whole value on every frame.
  `inspectModal.size` splits and wraps everything, and `modalRect` calls
  it on each draw, click and wheel step (`tui/mouse.go:125,456`). `draw`
  then wraps it all again. Timed with a throwaway benchmark (deleted):
  ~3.8 ms a frame at 95 KB and 38 ms at 1 MB. Raised as **N-325**.
- `gofmt -l tui` flags `tui/app_test.go` because of a comment alignment
  at `TestTablesSidebarPreview`. That predates this session (the HEAD
  copy fails gofmt too), so it was left alone.
- `TestChatEffort` (N-321) failed once in the full `./...` run and
  passed on two reruns of `./web`.

## Tests

- `tui/gridcell_test.go`:
  - `TestTruncateOnePassMatchesMeasureFirst`: 3,000 random values, edge
    widths.
  - `TestCutAtMoreAlwaysEllipsizes`.
  - `TestCellTextMatchesTheWholeValue`: 400 values of up to ~8,000
    pieces (ASCII, JSON punctuation, CJK, emoji, a flag, a ZWJ family,
    precomposed and combining accents, LF/CRLF/CR/tab), at widths
    1/2/3/40/random/400. `cellText` and `cellTextWidth` equal
    `truncate(flatten(v))` and `min(width(flatten(v)), limit)`.
  - `TestCellTextPathologicalHeadStillMarksTheCut`: 41 bytes a cell of
    combining marks; the result is still a prefix ending in "…".
  - `TestValueHeadCutsOnARuneStart`.
  - `TestGridLongCellsDrawFlattenedAndCut`: auto width at the cap, a fit
    at 400, the transposed draw.
  - The two benchmarks.
- `tui/app_test.go`: two `contentWidth` calls take the new `limit`.
- `web/grid_test.go`:
  - `TestGridPageClipsLongValues`: the page is under 8 KB, the cell is
    500 runes and a prefix of the value, the cut list, widths and
    content, a cut list relative to a later page.
  - `TestGridCellIsTheWholeValue`: whole, sorted desc, NULL, three
    400s, a single-cell copy still whole, a 409 after a new run, a 400
    with no result.
  - `TestClipRunes`.
- `web/e2e/longcells_test.go`, step "long values: heads in the grid,
  whole in Inspect" (last in `TestWeb`):
  - The first page's `decodedBodySize` (Resource Timing) is under 8 KB.
  - A cut cell is 500 flattened characters plus "…".
  - Inspect shows 100,000 characters, after a `GET …/cell`.
  - After two header clicks (sort desc), Inspect shows id 3's 300,000,
    and `y` copies all of it.
  - A NULL opens with no request.
  - It reuses `web_test.go`'s `hasReq(reqs, method, tail)`.
- Results: full `./...` green (bar the N-321 flake, green on rerun).
  Full e2e `DBC_E2E=1` green in 79 s; the Docker/Postgres steps skipped
  for want of containers, as usual.

## Docs

- README: nothing changes for a user (Enter still inspects). No edit.
- `ai_docs/todo/next-list.md`: N-277 moved to Closed with the measurements
  and the build, and N-325 raised. **Next ID** is N-326.
- Memory: the backlog note says N-277 is done and N-278 is next.

## Next

Closed: N-277. Declined: None. Raised: N-325. Deferred: None. Promoted:
None. Moved: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
