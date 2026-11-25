# N-336: dbc copy takes --columns, --query, --batch and --tables, and shows its rate and progress

Session: `8839602d-d9a8-4393-be1e-2889ac97cd8a`

## Ask

"Next list item N-336", then `/sw`. When N-297 closed with `copy --dry-run`
and `migrate pending`, it left four independent parts:

1. `dbc copy` has no `--columns`, `--query` or `--batch`, though
   `etl.CopyOptions` takes all three.
2. It takes one table per call.
3. It prints a bare row count, with no rate and no estimate before it starts.
4. A job re-run always starts at the root, with no "from the failed step".

The three copy parts were built, one commit each. The job resume is a
design question with choices that belong to the user, so it is raised as
N-339 with those questions written out.

## Part 1: `--columns`, `--query`, `--batch` (`f3c5f51`)

- `--columns a,b` (comma-separated or repeated, a `StringSliceFlag`) becomes
  `CopyOptions.Columns`. `copyColumns` trims each name, so `"id, name"`
  works. An empty or repeated name is a usage error: the SELECT would return
  the column twice, and the CREATE or load would then fail with no word on
  why.
- `--query SQL <dest-table>` sets `Query` and `To`; the one argument is the
  destination. Refused (exit 2) in `queryCopyRequest`: with `--where` or
  `--columns`, with no argument or two, and an empty `--query`. An empty
  `$Q` in `--query "$Q"` would otherwise read as no `--query` at all and
  take the destination's name as a source table. `flagQuerySet`, set by the
  flag's `Action`, tells an empty `--query` from none. A query's rows onto
  its own connection are not a self-copy, so that check is skipped.
- `--batch N` sets `BatchSize`; a value below 0 is refused. Into Postgres
  it is refused in `checkCopyConns`, the first place that knows the
  destination's engine (`db.Driver` → `pgx`; a derived `pg/db` too).
  Postgres loads by COPY, so the flag would do nothing, and the house rule
  is that a flag that does nothing is refused. `HideDefault: true` keeps
  "(default: 0)" out of the help, next to the usage's "default 500".

**Found by the real binary:** `--query` let the dry run see a write with
RETURNING (a move) for the first time. `PlanCopy` wrapped it in a subquery
to describe and count it. Off Postgres that is a syntax error, so the dry
run failed with "copy plan failed". Nothing ran: the source still had all
its rows. Now `PlanCopy` runs nothing of a write:

- no count, since counting it would run it;
- off Postgres, no describe (new `ColumnsNote`);
- Postgres still describes it through Parse, which runs nothing.

The plan prints a `moves:` line (new `Writes`), and a missing table with
`--create` reads "would be created from the columns the query returns".
The count was factored into `countSource`, for part 3.

An apparent oddity: `--columns id,total` seemed to create `total` as plain
`numeric`. In fact `information_schema.data_type` drops the precision, and
`numeric_precision`/`numeric_scale` showed 10,2 on both tables.

## Part 2: `--tables` (`6f98055`)

- **A flag, not more arguments.** A second argument already means the
  destination's name (`copy orders archive.orders`). Taking a third as a
  third table would make two arguments a rename and three arguments three
  tables. `copyRequests` builds one `copyRequest` per table, as if each were
  the argument, so the options and checks are shared.
- **Refused:** a table argument as well, `--query`, a table named twice, an
  empty name, and `--from` = `--to` (each table would be copied onto
  itself).
- **`copyMany`** runs the tables one after another, in the user's order (a
  parent before the child whose foreign key needs it). Each is `runCopy`,
  in its own transaction. Each summary line prints as its table ends. Then
  `manyRun.String` gives a total that names what failed, was canceled and
  was not tried, so a re-run can name just those:
  `copied 1 of 3 tables, 3 rows in 3ms; failed: nope; not tried: owners`.
- **Failures.** Without `-k` it stops at the first failure. `-k`, refused
  for copy before ("a copy has nothing to go on to"), is now allowed with
  `--tables` and tries the rest. A connection that cannot be opened stops
  it either way, since every table after would fail on it too.
- **Exit status.** 1 on any failure; 4 (`exitConnect`) when the first
  failure was a connection error; 130 on Ctrl+C (the table in progress
  rolls back, the ones before it stay).
- **`planMany`** is the dry run. It plans every table whatever `-k` says,
  since a dry run is for finding all the problems. It puts a blank line
  between plans and exits 1 if any plan has a problem or failed.

## Part 3: rate and progress (`93a3bd0`)

- **The rate.** `CopyStats.String` now reads `… in 405ms at 444400 rows/s
  (direct COPY)`. New `etl.RateText` leaves the rate out under 0.1 s, the
  TUI run monitor's rule, so tiny copies (the tutorial's examples) are
  unchanged. Scripts' `s.Copy` summary gets the rate too, as it shares
  `String`; so does the `--tables` total.
- **`copyMeter`** replaces sdb's `"  src → dst: N rows"` on a terminal. It
  sets its own `opts.Progress`; sdb fills in its own only when that is nil.
  Its lines:

      pg1:big → pg2:big: 2000000 rows to copy
      pg1:big → pg2:big: 300000 of 2000000 rows (15%), 846946 rows/s, ~2s left

- **The count runs beside the copy, not before it** (`countBeside`).
  Counting first would hold the copy back for up to the dry run's 30 s
  bound on a big table, which is where a percentage matters most. The copy
  starts at once, and the percentage joins the lines when the count lands.
  On Postgres the second sequential scan mostly shares the first one's
  reads (synchronized scans).
- **Stopping the count.** When the copy returns, the count is canceled and
  waited for, under the meter's lock, with `done` set. No line can follow
  the summary, and no query of the meter's is left on the pool for the next
  table or the manager's `Close`. pgx, MySQL (KILL), SQLite (interrupt)
  and bytdb (its row pumps poll ctx; checked in v0.21.2's stdlib) all stop
  a query on cancel. On one SQLite file, a count still reading could delay
  the copy's commit, but busy_timeout is 5 s and a SQLite count is far
  faster.
- **`etl.CountCopy`** is the dry run's `countSource` on its own, and it
  never counts a query that writes (new `writes` helper, shared with
  `PlanCopy`).
- **Line rules.** The count is said up front only when progress lines will
  follow (100,000 rows or more), so a small copy stays quiet as before.
  Past its count (rows arrived since), a line drops "of N (P%)" rather
  than read 104%. A time left that rounds to 0s is left out. That one was
  seen in the first run's output: `~0s left`.
- **Off a terminal:** no count. A cron copy reads its source once, as
  before.

## Checks

- **Real binary, SQLite to SQLite:** the columns subset (made with only
  those columns, in order), a query with `--create`, `--batch 2`, and the
  refusals (exit 2). The move's dry run and its refusal without `--create`
  (exit 1); the source was untouched. `--tables`: two tables; a missing one
  in the middle, without and with `-k`; a dry run of four with two missing
  on the source.
- **postgres:17 in a throwaway container on 55442** (55432 and 55433 belong
  to other projects' containers, left alone; mine stopped at the end):
  - a move's dry run, then the move (10 rows gone from the source);
  - `--columns` on the direct path;
  - Postgres to SQLite with `--batch 7`;
  - two tables by direct COPY;
  - a refused connection under `-k`, which stops with exit 4.
- **On a pty** (stdout and stderr on a pty through a python helper;
  `script` cannot run here because the tool's stdin is a socket):
  - 2M rows by direct COPY: the first progress line came before the count,
    and every later line had a percentage.
  - 300k rows Postgres to SQLite, row by row.
  - Off a tty, only the summary.
- **Tests:** `TestCopyCLIParsingN336`, `TestCopyRequestN336`,
  `TestRunCopyN336`, `TestCheckCopyConns` (`--batch`),
  `TestCopyRequestsTables`, `TestCopyMany`, `TestPlanMany`,
  `TestCopyMeterLine`, `TestCopyMeterCounted`, `TestCopyMeterCountBeside`,
  `TestRunCopyProgress` (210,000 SQLite rows). In etl: `TestPlanCopy` (a
  write), `TestCountCopy` (SQLite and bytdb; a write not run),
  `TestCopyStatsRate`. `resetFlags` now resets the new flags and
  `flagDryRun`.
- **Suites:** the full `./...` is green after
  `go generate ./sdb/sdbapi`. The api.json had gone stale from the two doc
  comments edited (`CopyStats.String`, the `s.Copy` example), and the 14 KB
  summary budget still holds. vet is clean; staticcheck (`go run
  honnef.co/go/tools/cmd/staticcheck@v0.8.2`, CI's version) is clean. The
  `etl` and root live suites are green on postgres:17.
- **Docs:** README (Copy headless: examples, flag rows, the `--tables`
  paragraph, progress), the dbc skill, `cats-plugin.toml` completions
  (`TestCatsManifest_CompletionsCoverCLI` caught their absence).

## Notes

- **Not done:** table patterns in `--tables` (a whole schema), per-table
  renames in it, and `--query` from a file. None is raised: nobody has
  asked.
- `gofmt -l` still lists `designer/import_test.go`, `sqlsplit/folds.go` and
  `tui/app_test.go`. They were unformatted at HEAD, untouched here, and left
  alone.
- `~/go/bin/staticcheck` is built with go1.24 and cannot load the module.
  Adding `GOFLAGS=-mod=mod` to the `go run` was refused by the auto-mode
  classifier; plain `go run …@v0.8.2` works.

## Next

Closed: N-336. Declined: None. Raised: N-339. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
