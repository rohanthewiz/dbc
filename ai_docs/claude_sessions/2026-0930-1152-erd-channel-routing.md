# ERD lines routed between boxes (N-065)

Session: `a7e73536-347c-45a5-a8ac-e53dd2d34fb3`
Date: 2026-09-30

## Ask

From the user's cats-todo backlog, next-list item N-065: some ERD lines pass
under the boxes between their two ends. These are lines to a far rank, or to
a wrapped column of a hub's children. Lines are drawn first, so they are
hidden rather than striking through text, but they are hard to follow. Route
them through the gaps between boxes. Then wrap the session with `/sw`.

## Approach

Channel routing, as a new step after placement (`erd/route.go`). Boxes are
placed exactly as before and never move for a line. Full Sugiyama's dummy
nodes were rejected because they reorder the real boxes around long edges,
so one new key could reshuffle a diagram a reader navigates by.

```
layGroup: rank → order → wrap → place → route (new)
                                          │
  per column: holes = above top box, each stackGap band, below bottom box
  per line skipping columns: DP picks one hole per column in the way
  per hole: lanes, ordered by the heights each line comes from and goes to
  per line: runs (box → column edge, through each hole) + S-curves in gaps
```

- **Holes.** Each placed column (`column{x, w, boxes, holes}`) has its free
  bands. Between two boxes a band is exactly `stackGap` (24 px). The open
  side above or below a column is unbounded.
- **Choice** (`chooseHoles`). A DP over the columns in the way minimises the
  total vertical travel plus `hole.cost()`. That cost is `laneLoad` (6) per
  line already in the hole, plus `overLoad` (120) per line past a bounded
  hole's capacity (5 lanes at `minLane` 3 px within `holePad` 5). So a hub's
  lines spread over the holes near their targets. Lines are routed in schema
  relationship order; ties take the upper hole, so the result is
  deterministic.
- **Lanes** (`setLanes`). In a band, lanes are centred and `laneGap` (6) apart,
  closing up when crowded. On an open side they stack outward from the box,
  the first `stackGap/2` out.
- **Points** (`crossing.path`). The curves between runs keep the old control
  rule `max(30, 0.45·span)`, so a line between neighbouring columns (whose
  box is the column's widest) draws exactly as before.
- **Loops** (`loopPath`, moved from `picture.go`). Same-column and
  self-reference lines now run straight to the column edge before bowing.
  From a narrow box, the old loop could cut through a wider box stacked
  between its ends.
- **Growth.** `layGroup` now returns the extent of its lines as well as its
  boxes. A line routed over a column's top moves the whole group down (never
  into the title or the previous group). A loop on the last column widens it.
- `layout.paths map[*Rel]*path` holds each line's points plus its marker
  points and directions. `drawer.rel` just strokes it and draws the markers.
  Lines are still painted before boxes, so the box borders cover the lines'
  anti-aliased ends.

## Tests (`erd/erd_test.go`)

- `TestRoutesMissBoxes` samples every line at 1 px on `fixture`, `star(45)`,
  `chain(12)`, a new `farRanks()` and `star(400)`. No sample may fall inside
  any box (1 px in from the border), and none may leave the picture or enter
  the title band.
- `TestLanesFitTheirHole`: nine lines in one band stay inside its padding, in
  order.
- `TestRouteAboveMovesGroupDown`: a chain whose skip line goes over `t01`
  must not rise into the title. With the move-down disabled, this test and
  `TestRoutesMissBoxes` (on `star(400)`) both fail.
- `go test ./...` is green.

## Results

Rendered before and after at 1× from a temporary worktree:

- **star(45):** every line threads between the wrapped columns. Same size
  (1626×1197).
- **farRanks-like chain:** long back-references run through the side column's
  gaps and above or below the chain boxes, instead of through them.
- **Loops and self-references (`fixture`):** unchanged.
- **star(400):** 4426×3411 → 4426×4018, and draws in 0.8 s (was 1.2 s). About
  250 lines can't fit between the boxes and ribbon above and below: raised
  as N-070.

## Notes

- `git stash` doesn't stash an untracked new file, so a "before" build
  failed on the leftover `route.go`. Use `git worktree add` for
  before-and-after renders instead.
- A `/next-list` item edit first grabbed the Open items after N-065 as well:
  Open items aren't separated by blank lines. Restored with `git checkout`
  and redone, cutting at the next `- **N-` line.

## Next

Closed: N-065. Declined: None. Raised: N-070.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
