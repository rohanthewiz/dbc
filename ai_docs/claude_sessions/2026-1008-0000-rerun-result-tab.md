# Rerun the query of a result tab, pinned or not

Session: `9dc52f86-6660-4c38-83b1-2d4da4576aad`

## Ask

1. Dropped in from the cats-todo backlog: "Allow me to re-run the query of
   a pinned result".
2. "Do a sess-wrap then merge these changes into local main".

## Decisions

No questions were asked. These were judgement calls:

- **The result refreshes in place.** The fresh result goes back into the
  same tab, which stays pinned, keeps its place in the strip and keeps its
  title. A pin stops OTHER runs from replacing the tab; rerunning the tab's
  own statement means the user wants that result up to date. A new tab
  would leave a stale copy behind. DataGrip's refresh on a pinned tab works
  the same way.
- **Any tab with a statement can be rerun**, not just pinned ones. For an
  unpinned tab the result is the same. A shared tab stays shared.
- **A statement that may write asks first.** `ResultTab.Writes` is
  `db.ChangesRows(stmt)`. `r` sits among the grid's keys, so a stray press
  could run an INSERT a second time. Both UIs also ask from the menu row:
  the row is one misclick from its neighbours.
- **A script's tab cannot be rerun.** It has no statement. The refusal
  says to run the script again.
- **A rerun picks no target and needs no room.** It is not refused at the
  cap with every tab kept, as a new run would be.
- **A rerun of one tab in a multi-statement group** refills only that tab
  and takes it out of the group. A single-statement run on it already
  does this.
- **The statement is not recorded in the history again.** It was recorded
  on its first run. An app run's catalog SQL ("list tables") never
  belonged in the history.
- **dbc web has its own endpoint (`POST …/rerun`), not a result-tab op.**
  The ops change the strip and answer at once. A rerun is a run: it takes
  the slot, ticks and lands later as a "run" event.
- **The page shows a tab before rerunning it** when that tab is not on
  screen. `grid.load` keeps hidden columns only from the grid it replaces,
  so this keeps the tab's own layout.

## What changed

### Workspace

- **`workspace/run.go`.** `Run` became a thin wrapper around
  `runLocked(stmts, tag, again)`. When `again` (the tab being rerun) is
  set, the target is that tab, `targetLocked` is skipped, and the title
  is kept: it is captured under `mu` for the job, and `w.runTitle` covers
  landRun's exec fallback. `beginRunLocked` clears `runAgain` and
  `runTitle`.
- **`workspace/workspace.go`.** New fields `runAgain *resultTab` and
  `runTitle string`.
- **`workspace/results.go`.**
  - A package-comment section with a before/after picture.
  - `placeRunLocked` may refill a kept target only when
    `target == w.runAgain`.
  - `RerunResultTab(id)`. It refuses a script's tab (Invalid), and Busy
    comes through `runLocked`. The tag is `"rerun " + title`.
  - `ResultTab.Writes`.
  - The `PinResultTab` doc now mentions reruns.

### TUI

- **`tui/resulttabs.go`.**
  - `rerunCurResultTab`: the `r` key. For a write it opens a confirm menu
    ("Keep the result as it is" / "↻ Run it again — …"), at the grid's
    cursor or at the plan view's top left.
  - `rerunResultTab` runs through `startRun`.
  - A menu row "↻ Rerun its query" (`r`). It reads "— it writes again"
    for a write and is disabled with a reason for a script's tab.
  - The strip hint gained `r rerun`, and the pin log line mentions `r`.
- **Elsewhere in the TUI.** `r` is routed in `gridKey` (`tui/app.go`); the
  plan view gets it through `resultTabKey`. Help rows are in
  `tui/help.go`.

### dbc web

- **`web/resulttabs.go`.** `handleRerun`, plus `writes` on
  `resultTabRef`. The route is in `web/server.go`.
- **`web/static/js/app.js`.**
  - `rerun(r)` / `rerunCurrent`. They show the tab first, then
    `POST /rerun`. A write opens a modal ("Run this statement again?",
    with the statement shown and the focus on "Keep the result").
  - The `r` key in the results pane, the menu row, the tooltip, and the
    help dialog's "Result tabs" rows.
- **`web/pages/workbench.go`.** The strip's aria-label mentions `r`.

### Docs

README: a "Rerun" paragraph under Result tabs, plus rows in the TUI's
mouse table, the TUI's keys table and dbc web's keys table.

## Verification

- **Checks.** `gofmt` is clean; `go vet` passes on the touched packages;
  `go test ./...` passes; `node --check` passes on `app.js`.
- **New workspace tests** (`workspace/results_test.go`):
  - `TestRerunPinnedTab`: same id and place, still pinned, title kept, new
    seq, fresh count, other tab untouched, `Writes` flags.
  - `TestRerunKeptTabsAtTheCap`: every tab kept at the cap; a shared
    tab and the "columns cats" title kept.
  - `TestRerunRefusalsAndFailure`: a failed rerun keeps the old result;
    a script's tab is refused, so is a rerun while busy, and so is an
    unknown id.
  - `TestRerunGroupTab`.
- **New TUI test.** `TestRerunResultTab`: the confirm for a write, Esc
  runs nothing, picking "Run it again" runs it, `r` on the pinned count
  shows 2, the log line, and the strip menu row.
- **New web wire test.** `TestRerunOnTheWire`: tag, run event, pinned id
  kept, new seq, `writes`, and a 400 for a gone tab.
- **Web e2e** (`DBC_E2E=1`): pass, 30s. `resultTabsPerConn` now presses
  `r` on the pinned, sorted tab and checks that the result moved in place
  and the strip menu has the row.
- **TUI e2e** (`DBC_TUI_E2E=1`): pass, 9s.
- **Not seen in a browser:** the write-confirm modal (N-157).

## Next

Closed: None. Declined: None. Raised: N-156, N-157.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
