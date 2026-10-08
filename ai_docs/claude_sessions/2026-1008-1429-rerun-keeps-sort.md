# A rerun keeps the grid's sort (N-158)

Session: `31e0add4-3d1f-4fd5-aa88-9c06ccec5091`

## Ask

1. Dropped in from the cats-todo backlog: next-list item N-158. A rerun of
   a result tab (`r`, `RerunResultTab`) dropped the grid's sort in both
   UIs. The TUI's `grid.SetResult` reset `sortCol`, and dbc web's
   `grid.load` started each new seq unsorted. The item suggested keeping the
   sort "perhaps for a rerun only", since an edit-the-WHERE run may want the
   reset.
2. `/sess-wrap`.

## Decisions

No questions were asked. These were judgement calls:

- **Only a rerun keeps the sort.** An edited statement run into the tab is
  a new question, and its rows start in result order. A rerun asks the
  same question again. Hidden columns and widths keep their older rule:
  they survive any run whose columns are the same.
- **The workspace says which result a rerun replaced, as a seq, not a
  flag.** `resultTab.rerunOf` holds the seq of the result the rerun
  replaced. Every other landing sets it to 0: a run, a script's show, or
  `ShowScriptResult`. Each UI keeps the sort only when `rerunOf` equals
  the seq it was showing. A plain "this is a rerun" flag would fail when
  a grid missed a landing (an edited run, then a rerun of that one). That
  grid would apply a sort meant for a different question.
- **The columns must still match.** If a rerun's columns change (the
  table was altered), the sort is reset along with the hidden columns and
  widths. The sort column's index might not mean the same column any more.
- **The workspace reads all three values under one lock.** The web needs
  the result, its seq and `rerunOf` together. `LastResultRerun` returns
  them from one locked read, so a run landing between two calls can't pair
  one result with another's `rerunOf`. `LastResultSeq` now calls it.
- **dbc web: no second fetch for a kept sort.** The page is requested with
  the old sort. When the sort is kept, that first answer is already
  correct; only a reset costs the unsorted refetch. `adopt` now takes the
  sort from the page instead of forcing -1, so the header arrow always
  matches the rows.
- **dbc web: a rerun that lands while you look elsewhere.** This is a
  rerun that finishes while the page is on another query tab or
  connection. The new result has no view kept under its own seq. `viewFor`
  then falls back to the view kept for `rerunOf`. `restore` carries that
  view's sort, hidden columns and widths over, but not the cursor or
  scroll, because the rows are new. Snapshots now store `cols` so restore
  can check that the columns match. The TUI gets this for free: its parked
  grid still holds the old result and seq.

## Changes

- **`workspace/results.go`**
  - New field `resultTab.rerunOf`, set in `placeRunLocked` when the slot
    is `w.runAgain`, and cleared on any other fill and in `showLocked`.
  - New `ResultTab.RerunOf`.
  - The package comment's rerun paragraph mentions it.
- **`workspace/workspace.go`**: new `LastResultRerun`.
  `ShowScriptResult` clears `rerunOf`.
- **`tui/grid.go`**
  - New `grid.seq`.
  - `SetResult` now calls `setResult(r, cap, rerun bool)`; a new
    `SetRerun` calls it with rerun set. It keeps the sort under the
    existing same-columns test and re-applies it to the new rows.
- **`tui/resulttabs.go`**
  - `syncResults` calls `SetRerun` when the tab's `RerunOf` equals the
    grid's seq, and records the seq after every sync.
  - The `rerunResultTab` comment is updated.
- **`web/grid.go`**: `resultView.rerunOf`, read from `LastResultRerun`;
  the `/result` page carries `rerunOf` (omitempty).
- **`web/resulttabs.go`**: `resultTabRef.RerunOf` on the strip.
- **`web/static/js/grid.js`**
  - New helpers `sameCols` and `rerunOf`.
  - `load` keeps the sort for a rerun of the seq on screen.
  - `adopt` takes `d.sort` and `d.desc`.
  - `snapshot` stores `cols`.
  - `restore` handles a snapshot of the replaced result.
- **`web/static/js/app.js`**: `viewFor` falls back to the view kept for
  `rerunOf`. The `rerun` comment is updated.
- **README**: the grid's hidden-columns paragraph and the rerun paragraph
  say the sort survives only a rerun.

## Tests

- `TestRerunNotesTheResultItReplaced` (workspace): a rerun sets `RerunOf`
  to the old seq, and an edited run into the same tab sets 0.
- `TestGridSortSurvivesOnlyARerun` (tui): `SetRerun` keeps the sort and
  re-sorts the changed rows. `SetResult` resets it, and so does a rerun
  with other columns.
- `TestRerunKeepsTheSort` (tui, model level): `r` on a pinned tab sorted
  descending keeps `d,c,b,a` after a row is added. Unpinned, an edited run
  starts in result order.
- `TestRerunOnTheWire` (web) now checks `rerunOf` on the strip and on the
  `/result` page.
- e2e `resultTabsPerConn`: after `r`, the pinned tab is still sorted by
  age, with the ▲ arrow.
- Mutation checks, so the new tests are known to catch the old behavior:
  - With `syncResults`' `SetRerun` branch disabled, `TestRerunKeepsTheSort`
    fails.
  - With `grid.js` `load` reverted, the e2e step fails ("rerun still
    sorted by age").
- `go vet ./...` and `go test ./...` pass, and so does the full
  `DBC_E2E=1` suite (~28 s).
- The web background-landing path (the `viewFor` fallback with the
  `restore` carry-over) has no e2e check. A rerun on SQLite finishes too
  fast to switch query tabs mid-run.

## Next

Closed: N-158. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
