# N-147: a result tab per statement for a multi-statement run

Session: `86fb22a6-961f-49c0-bd4f-6b860ecc928d`

## Asks

1. N-147 from the cats-todo backlog. Run all, or a selection of several
   statements, landed only the last statement's result in one tab. DBeaver
   and DataGrip open a tab per statement that returns rows. The item warned
   that this would interact with the `result_tabs` cap and with "a run
   replaces its tab".
2. "/sw" (sess-wrap).

## Decisions

Judgement calls (no questions were asked):

- **Only statements that return rows get a tab.** A write's "n affected"
  gets none, or a script of INSERTs would fill the strip. When no statement
  returns rows, the last statement's result lands alone, as it always did.
  A SELECT with zero rows still gets a tab.
- **"A run replaces its tab" became "a run replaces its group".** The tabs
  one run filled share a run id (`resultTab.run`, the run's `runGen`). A
  rerun of several statements refills the target's group in strip order,
  opens new tabs after the group's last, and closes the group tabs it has
  no result for. Without this, every run-all would add a strip's worth of
  tabs.
- **The group decision depends on the statement count, not the result
  count.** At first a group was only replaced when the run returned several
  results. The web wire test showed the flaw: a rerun where only one
  statement returned rows (or a failure part way) refilled one tab and left
  the group's other tabs showing old results.
- **A single-statement run replaces only its own tab**, even inside a
  group, and takes that tab out of the group. That is the edit-one-and-rerun
  loop.
- **Pinned and shared tabs stay out of the group** (`resultSet.kept`), as
  they already did for single runs.
- **The cap keeps the last results.** A new tab first drops the oldest
  unkept tab of other runs, then this run's own oldest (`makeRoomLocked`).
  The last result stays on screen, as before. The job also keeps at most
  `result_tabs` row results in memory, and a Warn note says how many did
  not fit. Results thrown away by the job and by the cap are counted
  together.
- **A failed or stopped run still lands the results before the failure.**
  Run-all with a typo in statement 3 used to throw away statements 1 and 2.
  An Info note says how many landed; the status bar keeps the error.
- **The last tab filled is the one on screen**, and the done note names
  which statement's result it is.
- **The assistant gets the on-screen tab's rows only when that tab holds
  the statement under the caret.** After `SELECT …; UPDATE …` the SELECT's
  rows are on screen while the UPDATE is the last statement, so the old
  `cur == lastStmt` check would have sent the SELECT's rows as the
  UPDATE's. This also fixes an older bug: clicking back to an earlier tab
  sent that tab's rows as the last statement's. In return, a tab whose
  statement is under the caret now sends its rows even when it is not the
  last statement run.

## Changes

- **`workspace/results.go`.** A package-comment section with a picture of
  a group refill. `resultTab.run`. The `landing` type (title, stmt, result,
  statement number). `placeRunLocked(conn, target, run, group, landings)`
  holds the slot, insertion, leftover-close and cap logic, and returns the
  tab put on screen and how many of the run's own tabs were dropped.
  `placeLocked` (used by script shows) is now a one-landing call to it,
  with the same behavior as before. `makeRoomLocked` is new.
- **`workspace/run.go`.** `Run`'s job collects a `landing` per statement
  that returned rows, at most `result_tabs` of them. `landRun` takes them
  (plus `over`), lands them on failure too, and falls back to the last
  result when there are none. `landRowsLocked` places them, sets
  `ev.Result`/`ev.Tabs`, writes the doesn't-fit note, and detects a plan
  in the result on screen. `doneNote(ev, shown)` gains two forms:
  `— 2 result tabs, showing statement 3's: 42 rows` and
  `— showing statement 1's result: 42 rows`.
- **`workspace/events.go`.** `RunDone.Result` is now "the result on
  screen" and can be set alongside `Err`. `RunDone.Tabs` is new.
- **`workspace/chat.go`.** `attachTabLocked(ctx, views, stmt)`. The
  `attachLastRunLocked` signature now takes `stmt` (a script passes `""`,
  which matches its tab). `ChatContext` uses a switch: last statement →
  error or tab; otherwise the tab if it holds this statement.
- **`tui/run.go`.** The failure path draws `ev.Result` when results landed,
  then sets the error status.
- **`web/hub.go`.** On failure `ResultStatus` no longer overwrites the error
  status. `HasResult` is already true when results landed, so the page
  loads the grid. The page's JS is unchanged: it redraws the strip from
  every "run" event.
- **README.** The Run-all paragraph, a "several statements" paragraph under
  Result tabs, and the cap's drop order.

## Tests

- **`workspace/results_test.go`.**
  - `TestRunAllTabPerStatement`: titles, on-screen result, done note, chat
    context with the caret on the INSERT, on b, and on a after clicking
    back; writes only → one exec tab.
  - `TestRunAllReplacesItsGroup`: a pinned tab stays out; a rerun from the
    middle tab refills the same ids; a shorter rerun closes the third; a
    solo run replaces one tab and leaves the group; a later run-all opens
    its new tab next to the group.
  - `TestRunAllTabsStayTogether`: new tabs are inserted mid-strip after the
    group.
  - `TestRunAllUnderTheCap`: drop order and the "2 of the run's 4 results"
    note.
- **`workspace/workspace_test.go`.** `TestRunStopsAtTheFirstFailure` now
  expects statement 1's result and the Info note (it asserted the old rule,
  "a failed run publishes no result"). `TestStragglerIsDropped` uses the
  new `landRun` signature.
- **`web/resulttabs_test.go` → `TestRunAllTabPerStatementOnTheWire`.** Two
  tabs on the wire, then a failed rerun: `ok:false`, `hasResult:true`, the
  error status, and the group shrunk to its first tab.
- **`tui/app_test.go` → `TestRunAllStopsAtTheFirstFailure`.** The grid shows
  statement 1's result while the status keeps "error after".
- Full `go test ./...` passes with `CATS_*` removed from the environment;
  gofmt and vet are clean. Not run: the real binary or a browser (raised
  N-154).

## Next

Closed: N-147. Declined: None. Raised: N-154, N-155. Deferred: None.
Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
