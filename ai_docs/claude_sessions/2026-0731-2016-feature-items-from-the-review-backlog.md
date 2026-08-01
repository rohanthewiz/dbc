# Feature items from the review backlog

Session: e498c31f-6e09-43a4-8da3-d09afdd0daa0

## What happened

Continued from the robustness session: implemented the six remaining feature
items in `ai_docs/fable_recommends.md`, one commit per item (standing
preference). All tests pass, `go vet` clean, plus live smoke tests of the
headless script output and of the new config key.

With this, **every item on `fable_recommends.md` is done** — a further backlog
needs a fresh review pass.

## Commits (in order)

1. `3835b3b` — **Headless scripts honor `-f`/`-o`** (feature 3).
   `runScriptHeadless` printed a hardcoded text table per shown result. It now
   collects what the script pushes with `s.Show` and renders it through the
   same `emit()` the query path uses: one document in the `-f` format, to
   stdout or the `-o` file. `-f json` is therefore one array rather than a run
   of separate documents. `outFormat()` parses `-f` once in `main` and both
   headless paths take it as a parameter. New `scriptLog(f)` routes `s.Print`
   to stderr when a machine-readable format owns stdout, stdout otherwise.
   Tests: `TestScriptHeadlessHonorsFormatAndOutfile` (via new
   `testdata/show_two.go`), `TestScriptLogDestination`.
2. `d8635bb` — **NULL styling** (feature 5). `renderResult` gives a cell whose
   `Raw` value is `nil` the muted foreground, so a real NULL cannot pass for a
   column holding the string `"NULL"`. New `isNull(r, row, col)`. Test:
   `TestNullCellsAreMuted` (reads the color out of the cell's `Style`, not the
   legacy `Color` field — tview leaves that at default once Style is set).
3. `9a0db97` — **Copy cell/row with `y`/`Y`** (feature 6). The results table
   switched to cell selection (`SetSelectable(true, true)`), which is what
   makes "copy this value" pointable and gives the arrows somewhere to go on a
   wide result. The keys only fire while the table has focus. The clipboard
   text comes from pure `selectedText(res, row, col, wholeRow)`, which is
   where the tests are — pressing the key in a test would clobber the tester's
   clipboard. Tests: `TestSelectedText` (8 cases),
   `TestCopyKeysDoNotStealFromTheEditor`.
4. `8ef1fe3` — **`Ctrl+T` lists tables** (feature 4). New `db/catalog.go` with
   `TablesQuery(driver)`: information_schema for Postgres and MySQL (the
   latter scoped to `DATABASE()`), `sqlite_master` for SQLite, all three
   shaped to the same `table_schema · table_name · table_type`. Runs through
   the same path Ctrl+R uses — `runQuery`'s body was extracted as `App.run`.
   In the status hints `^T tables` took `Tab focus`'s slot (identical width).
   Tests: `TestTablesQueryPerDriver`, `TestTablesQueryRunsOnSqlite` (real
   database, views included), `TestCtrlTListsTables`.
5. `72b487a` — **Query history, `Ctrl+P` recall** (feature 2). New
   `ui/history.go`: JSON-lines file at `~/.config/dbc/history.jsonl`, 0600,
   append per query, trimmed to the newest 500 at load (the only rewrite).
   The modal lists newest first with a live filter over SQL and connection
   name; Enter inserts into the editor at the cursor rather than running.
   Recording happens in `runQuery` (before the run), per statement, skipping
   consecutive duplicates. Startup now logs the full ten-key list. Tests:
   8 in `ui/history_test.go` plus `TestHistoryRecallInsertsIntoTheEditor` and
   `TestHistoryRecordsStatementsNotCatalogQueries`.
6. `399e488` — **`max_display_rows`** (the performance note). New config key,
   default 2000 (above the stock `max_rows` of 1000, so it bites only once
   someone raises `max_rows`); explicit `0` draws everything. `App.rowsShown`
   caps what `renderResult` materializes; the status bar says `(showing N)`
   and the log explains once. Tests: `TestLoadRowCaps`,
   `TestDisplayCapBoundsTheTableNotTheResult`.

## Design decisions

- **Script results are collected, not streamed.** Matching the query path
  exactly (`RenderAll` over everything shown) is what makes `-f json` and
  `-o` correct. Cost, stated in the commit: a single-result script no longer
  gets the old `-- demo │ 8 rows in 1ms` banner, because that is what the
  query path does with one result.
- **`s.Print` to stderr only when it would corrupt the stream** — `text` to
  stdout keeps sharing, since that is a human reading a table.
- **Cell selection over row selection.** It is the half of "copy row/cell"
  that row selection cannot express, and it buys horizontal navigation. This
  is the one change that alters the resting look of the TUI; a one-line
  revert if the full-row highlight is missed.
- **History inserts, does not run.** A recalled query is usually one you want
  to edit — and auto-running someone's remembered `DELETE` is not a thing to
  do.
- **Recorded before the run**, per statement: the query worth recalling is
  very often the one that just failed, and `Ctrl+T`'s catalog query is the
  app's, not the user's, so it stays out.
- **Two row caps, not one.** Fetching more costs memory and patience;
  drawing more costs a tview cell per value. Raising `max_rows` for an export
  should not make the table crawl.

## Things learned about tview (worth keeping)

- `TableCell.Color` stays at `default` once `Style` is initialized —
  `SetTextColor` writes into `Style` instead. Read a cell's color with
  `cell.Style.Decompose()`.
- **tview dispatches a key straight to the focused primitive**; captures on
  parent containers never see it. The history modal's key handling had to sit
  on the filter `InputField`, not on the `Flex` around it.
- `QueueEvent` and `QueueUpdate` land on different channels, so `drain()`
  after a `press()` does *not* guarantee the event was handled. Tests that
  press a key must `waitFor` the resulting condition (e.g. the front page
  changing) rather than draining.

## Smoke tests

- `dbc -f json script scripts/loop_params.go` → three envelopes on stdout,
  valid to `jq`, the three `min age …` prints on stderr.
- `dbc script scripts/loop_params.go` → prints and banners together on
  stdout, unchanged shape for a human.
- `dbc -f csv -o r.csv script …` → `wrote 3 results (16 rows) to …`, clean
  CSV blocks in the file.
- `dbc -config <max_rows=3, max_display_rows=2> "SELECT 1 AS a; SELECT 2 AS b"`
  → the new key loads and the headless path is untouched by it.

## Still open

Nothing on `ai_docs/fable_recommends.md`. The `forvar`/`minmax` modernizations
the linter flags in `ui/modals.go` (`f := f`, a hand-rolled `min`) predate
this session and were left alone to keep the commits focused.
