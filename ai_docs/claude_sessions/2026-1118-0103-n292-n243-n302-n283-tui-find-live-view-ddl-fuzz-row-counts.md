# N-292, N-243, N-302, N-283: find in the TUI, live view DDL, fuzz targets, one slow table no longer blanks the counts

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the second batch. The run's triage and the first batch
(N-286, N-288, N-291) are in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-292: find in the TUI's editor, grid and log (`af8737f`)

`tui/find.go` is new: one find line, `findLine`, that serves all three
panes.

- **Where it draws.** `withFind` gives the pane's last body row to the
  line and draws the pane above it, so nothing is hidden under the line.
  `drawFind` draws `⌕`, the text and a hint at the right end: `Enter next ·
  ⇧Enter prev · Esc` while typing, `n next · N prev` off the line, or a
  note. A miss is drawn in the error colour and says "no match".
- **Keys:**
  - `/` opens it in the grid and the log; `Ctrl+F` opens it in those and
    in the editor.
  - Typing searches from the cursor, the cursor's own cell or caret
    included.
  - `Enter`, `↓` and `Ctrl+F` go to the next match; `⇧Enter` and `↑` go to
    the previous one. Both wrap, with a "wrapped to the top" or "wrapped
    to the end" note.
  - `Esc` closes the line and leaves the cursor on the match.
  - `Tab` leaves the line and keeps the find; `n` and `N` then repeat it
    from the grid or the log (`findPaneKey`).
  - In the editor, `:42` goes to line 42 (`gotoLine`). `Ctrl+G` was not
    free: it is the cats agents' key.
  - `findLineKey` decides which keys the line takes: typed text, its own
    keys, and the line editor's. Every other chord passes through, so
    `Ctrl+R` runs and `Ctrl+Q` quits with the line open.
- **What each pane searches:**
  - **Grid** (`grid.find`): the display text of each cell, in reading
    order over the shown columns, so a hidden column is not searched. A
    NULL matches as the "NULL" it draws.
  - **Log** (`logPane.find`): a line's message, not its timestamp. The
    pane's new `mark` field holds the matched line; `draw` lights its rows
    with the selection colour, and `reveal` scrolls it a third of the way
    down. `trim` shifts the mark when old lines go, and `clear` resets it.
  - **Editor** (`editorFind`): the buffer. The match is selected, so
    typing replaces it.
- **Matching.** `indexFold` and `containsFold` scan bytes with no
  allocation for ASCII and fall back to `strings.ToLower` otherwise.
  `lastIndexFold` scans forward from the start.
- **Sort key.** `o` in the grid sorts by the cursor's column, a header
  click's cycle, and logs what it did (`sortByCursor`).
- **Lifetime.** Each find is the Model's (`gfind`, `lfind`, `efind`), and
  every `load` of a tab drops them all (`dropFinds`). That covers a tab
  switch, a new tab and a close.
- **Docs.** F1's keys (the grid's and the log's rows, and `^F` in
  "Anywhere") and the README keys table.
  - The first try put `^F` in F1's Editor group. That pushed `^J` below the
    first screen of the dialog, which the TUI e2e step "F1 opens the keys
    dialog" checks, so `^F` moved to "Anywhere".

Tests: `TestGridFind`, `TestEditorFind`, `TestLogFind`, `TestGridSortKey`,
`TestFindLinePassesChords`, `TestTabSwitchDropsFinds`, `TestIndexFold`. The
`tui` suite and the pty e2e (`DBC_TUI_E2E=1`) pass.

## N-243: live view DDL (`3183822`)

The checks ran against throwaway Docker servers: `dbc-n-pg17`
(postgres:17 on port 55517, with a `dbc` database made for the live
tests) and `dbc-n-my84` (mysql:8.4 on 53384).

`db/live_viewddl_test.go` is new:

- **`TestLiveViewDDLPostgres`** reads three views. Each statement is then
  run again under another name, and a missing view gives "no such view".

  ```sql
  CREATE OR REPLACE VIEW dbc_live_v.big_orders AS
  SELECT id,
      total
     FROM dbc_live_v.orders
    WHERE total > 100::numeric;
  CREATE MATERIALIZED VIEW dbc_live_v.order_sums AS …
  CREATE OR REPLACE VIEW "DbcLive V"."Big Orders" AS …
  ```

- **`TestLiveViewDDLMySQL`** reads `CREATE ALGORITHM=UNDEFINED DEFINER=…
  VIEW \`dbc_live_v_big\` AS select …;`. A user granted SELECT on the view
  but not SHOW VIEW gets
  `Error 1142 (42000): SHOW VIEW command denied to user …`.

These passed too: `tui.TestLiveRoutinesPanePostgres`,
`TestLiveNavigatorPostgres`, and the web e2e step "postgres routines"
(`DBC_E2E_STEPS='postgres routines'`).

## N-302: the remaining fuzz targets and CI (`fe34747`)

**`designer.FuzzImport`** runs on four drivers with the shop schema and
wants:

- no panic;
- a refusal that is an `*ImportError` placed inside the SQL;
- an accepted design whose `Analyze().SQL` imports again and writes the
  same statement. `now` is fixed so relative dates do not drift.

**The explain targets** are seeded from `testdata`: `FuzzParsePostgresJSON`,
`FuzzParsePostgresText` (Postgres and bytdb), `FuzzParseMySQLTree`, and
`FuzzParseTabular` (one TSV fed to both `ParseMySQLTable` and
`ParseSQLite`).

- Each also runs what reads a plan after its parser (`usePlan`):
  `Finalize`, `Text` (80 columns with insights, 10 with colour), `Mermaid`,
  `Findings`, `JSON` and `ResolveAliases`.
- Each runs under a 10 s `watchdog` that panics. Go's fuzzer has no
  per-input timeout, so a panic is the only way a hanging input gets
  saved.

**A false alarm.** FuzzParsePostgresText's first run showed long
`0/sec` stretches, 88k execs in a minute. Two more runs (90 s at 5 s, and
120 s with the watchdog at 1 s) caught no slow input. The stretches were
the fuzzer minimizing new inputs while the N-283 builds and tests shared
the CPU.

**`jobs.FuzzParseCron`** runs in America/New_York, from 1970 to 2100. An
accepted expression's next four times must each be later than the last,
on a whole minute, and either a wall time the fields match or the instant
the clocks jumped over one (`fires`).

60-second runs, all clean:

| Target | Execs |
| --- | --- |
| designer | 3.1M |
| PG JSON | 2.4M |
| PG text | 0.6M |
| MySQL tree | 0.28M |
| tabular | 1.25M |
| cron | 0.35M |

`test.yml` gains a Linux-only "Fuzz (brief)" step that runs all ten
targets for 10 s each. Its first CI run was still in progress at this
wrap.

## N-283: one slow table no longer blanks a Postgres counting (`3c62857`)

`db/rowcount.go`:

- **The share.** The one `PgCountsQuery` now runs under
  `pgStatementShare`, half of `countBudget` (1 min).
- **The fallback.** On any failure, that share running out included, the
  counting falls back to counting table by table with the rest of the
  budget. A huge table runs out its own `countTimeout` (30 s) there while
  the four workers count the others.
- **Why.** Before, the statement waited out the whole budget. The result
  was an empty map and a nil error, cached for `rowCountTTL`.
- **Test hooks.** `countTimeout` and `pgStatementShare` are vars, and
  `db.CountTimeout()` names the limit for the note.

The workspace's counts job adds a muted note when tables are left without
a number:

```text
row counts: N of M tables left without a number — a count(*) ran past its 30s, or the table cannot be read
```

It counts only the tables asked for, never views (`uncounted`,
`countable`).

Tests:

- `TestUncounted`.
- Live `TestLiveRowCountsOneSlowTable` on postgres:17. Another session
  holds `ACCESS EXCLUSIVE` on one table; the share is 500 ms and a count's
  timeout 1 s. The quick table's 42 rows came back and the locked table
  had no count, in 1.7 s.

## Notes for the rest of the run

- The Docker containers `dbc-n-pg17` and `dbc-n-my84` are still up for the
  live checks to come. They are removed at the end of the run.
  `bytdb-numeric-pg` is not this run's and is left alone.
- `gofmt` drift in `designer/import_test.go`, `sqlsplit/folds.go` and
  `tui/app_test.go` predates this run.

## Next

Closed: N-292, N-243, N-302, N-283. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
