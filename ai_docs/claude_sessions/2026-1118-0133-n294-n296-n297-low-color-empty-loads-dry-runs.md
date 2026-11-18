# N-294, N-296, N-297: low-colour selections, empty loads, dry runs (and a CI flake)

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the fifth batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-294: not by colour alone (`19740e2`)

**Selections in reverse video.** `tea.ColorProfileMsg` sets
`Model.lowColor` when the profile is below ANSI256; `Ascii` is what
NO_COLOR gives.

- It is copied into `styles.lowColor`, and again at both places the
  styles are rebuilt (cats startup and a host theme change).
- `styles.picked(cs, bg)` gives `cs.Reverse()` at low colour, otherwise
  the selection background. `Style.Reverse` is the first use of
  `AttrReverse`.
- It applies to the grid's cursor cell and range, and to the editor's
  selection and its end-of-line cell.

**Log glyphs.** `logGlyph` puts `⚠ ` before a warning and `✗ ` before an
error. It is added at wrap time, not stored, so `Text()` (copy) and a find
see the message as logged.

`TestLogWrapCacheMatchesAFullWrap`'s reference wrap now carries the glyph.

Not done:

- The `ok` and `accent` colours are still the same.
- The grid's hover and cursor-row tint are still background-only. Both
  are faint aids, not states.

Tests: `TestLowColorSelections` (TrueColor/ANSI256 keep the background;
ANSI and Ascii reverse) and `TestLogGlyphs`. `go mod tidy` made
`colorprofile` a direct dependency.

## N-296: a truncating sql.write empties its table when no rows come (`89d5090`)

**The problem.** With a transform between the source and a sink, a run
whose rows were all filtered out never opened the sink, since its
columns were unknown. Its `truncate` never ran, so the table kept
yesterday's rows.

**The hook.** `pipeline.EmptyLoader` is a new optional interface:
`LoadEmpty(e) (bool, error)`. The runner calls it for a sink no batch
reached when `sourceColsFor` has no columns.

**`sql.write`'s `LoadEmpty`.** With `truncate`, it calls new
`etl.Empty` / `EmptySameDatabase`. Those run the setup and the truncate,
with no rows, in one transaction. `loadSetup` was factored out of
`NewWriter`, so both build the same statements, the same-database DELETE
included.

- A table that does not exist yet is probed and left alone. With no
  columns, `create` has nothing to build it from.
- The log says `no rows reached it; the table was emptied (truncate)` or,
  as before, `nothing written`.

Tests:

- `TestRunEmptyThroughTransformStillTruncates`: truncating, plain and
  not-yet-created sinks under a filter that drops every row.
- `etl.TestEmpty` on SQLite and bytdb.
- etl's live suite on postgres:17.

## N-297: dry runs for the headless movers (`3f1d690`)

**`dbc copy --dry-run`.** New `etl/plan.go` adds `PlanCopy`, reached
through `sdb.S.PlanCopy`. It reports:

- the source SELECT (`copySource`);
- its columns and types: Parse on Postgres (`describe`), the query
  wrapped `WHERE 1 = 0` elsewhere;
- the row count, within 30 s;
- whether the destination exists;
- the `CREATE` (`createDDL`) and the truncate statement (`loadSetup`);
- the path: direct COPY, or row by row.

A missing table without `--create` is reported as the problem, with
exit 1.

**`dbc migrate pending`.** `Migrator.Pending` makes up's selection
(`toApply`, factored out of `upTo`, refusal included) with no lock.

- It creates no `goose_db_version` table: `hasTable` is checked first.
- Text output gives each file's statements under a comment, marking
  NO TRANSACTION files. `-t json` gives each migration's statements.

Checked in the real binary:

- SQLite → SQLite: destination exists; `--create`; `--truncate` (DELETE);
  missing without `--create`, exit 1.
- postgres:17 → itself with `--where`, `--create` and `--truncate`: 90
  rows, direct, catalog types `numeric(10,2) NOT NULL`.
- Postgres → SQLite: mapped types.
- `migrate pending` before and after `up-by-one`, with no version table
  left behind.

Tests: `etl.TestPlanCopy` and `migrate.TestPending`. The README (copy's
flags, Migrations) and the dbc skill are updated.

**Raised N-336**: the rest of N-297.

- copy's `--columns`, `--query` and `--batch`;
- several tables in one call;
- a rate or estimate;
- re-running a job from its failed step.

## CI: a flake on Linux, and the fake pg_dump (`c4c1ea6`)

The first CI run to complete this session (44258040615, commit `2476452`)
failed on the Linux job.

- **The failure.** `web.TestDumpOneAtATimeAndStop` gave "stream closed
  waiting for "dump"" after 10.03 s. The macOS job passed. The new "Fuzz
  (brief)" step was skipped after the failure.
- **Not reproduced.** It passed 5× under `-race` here, and in a
  `golang:1.26.1` Linux container, both before and after the change.
- **What was changed anyway.** The fake pg_dump now `exec`s its sleep, so
  the stop signals the sleeping process, as with a real pg_dump.
  Measured: a non-interactive `sh` (dash, and macOS's sh) waiting on a
  foreground `sleep 6` holds SIGINT for 6 s, against 0.5 s with `exec`.
  The old fake could leave the stop to `cmd.WaitDelay` (10 s), racing the
  test's 10 s wait.
- The case is recorded as an update to N-308 (tests that fail only in a
  full run).

A green run with the fuzz step is still to be seen; the final wrap
checks.

## Next

Closed: N-294, N-296, N-297. Declined: None. Raised: N-336. Deferred:
None. Promoted: None. Moved: None. Updated: N-308. Full list:
`ai_docs/todo/next-list.md`.
