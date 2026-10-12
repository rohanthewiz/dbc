# An interactive scripting and ETL tutorial: `dbc tutorial`

Raised 2026-10-12. The ask: make `docs/scripting-and-etl-tutorial.md`
interactive. This plan covers **step 1**, a headless `dbc tutorial` command. **Step 1
is done** (2026-10-12; all five phases).
A panel in `dbc web` (step 2) and a TUI pane (step 3) are under *Later*.
They would reuse the lessons and checks built here.

## The one-paragraph version

The tutorial already works as a script: every section is files to write and
`dbc …` commands to run, against two embedded engines (SQLite `shop`, bytdb
`warehouse`). `dbc tutorial` ships those files inside the binary (as
`scripts/embed.go` ships the samples) and adds three verbs. `init` writes
the section 0 workspace. `step N` prints a lesson and, with `--run`, writes
its files and runs its commands. `check N` tells you whether the lesson
worked and, when it didn't, what to do. The lessons are data in one Go
table (text, files, commands, checks). A test keeps the embedded files equal
to the code blocks in the Markdown, so the two can't drift.

## Goals

- **Zero copy-paste to a working workspace.** `dbc tutorial init` and then
  `dbc tutorial step 0 --run` leaves a seeded shop, as section 0 does by
  hand.
- **Learn by doing, or by watching.** `step N` shows what to type. You type
  it yourself, or `--run` types it for you, echoing each `$ dbc …` line
  first.
- **Know when you're done.** `check N` verifies the *state* the lesson
  leaves (tables and row counts, files, run records, clean checks), not the
  text you typed. So editing a script your own way still passes.
- **A failure says what to do.** Every check has a hint, taken from the
  tutorial's section 12 (Pitfalls) where one applies.
- **The doc stays the source of truth for prose.** The Markdown is still
  the tutorial; the command is a companion that points at its sections.

## Non-goals

- A browser or TUI tutorial UI (*Later*).
- Rewriting the Markdown or generating it from the lesson table. The table
  holds short summaries and the doc holds the long text. A test ties their
  *files* together, which is where the drift would hurt.
- Grading code style or comparing output text. Output contains timings and
  run ids, so checks look at state only.
- Postgres or MySQL workspaces. The tutorial is written for the two
  embedded engines.

## Decisions (taken as recommended; easy to revisit)

| Fork | Chosen | Why / alternatives |
|---|---|---|
| Where lessons live | **A Go table in a new `tutorial` package** | JSON/TOML lesson files: a check is code (a SQL count, a glob), so it would need a little DSL |
| Where the files live | **`tutorial/workspace/` embedded with `embed.FS`** | Extracting them from the Markdown at run time is fragile, and the doc would become load-bearing |
| How commands and checks run | **The dbc binary itself (`os.Executable()`) as a child process, cwd = the workspace** | In process: the headless paths call `fail`/`usage`, which `os.Exit`, and `./dbc.toml` is found from the cwd. A child runs exactly what the learner would type |
| Re-running a done step | **`--run` skips a lesson whose checks already pass; `--force` runs it anyway** | Section 0's seed and section 4's first copy fail the second time (by design: they demonstrate the PK and the one-transaction load) |
| Existing files | **`init` and `--run` never overwrite. A file you edited stays yours, and `--force` replaces it** | Always overwriting would lose the learner's edits |
| Earlier lessons not done | **`step N --run` warns and names the first unfinished lesson, then goes on** | Blocking would get in the way of someone working in their own order |
| Commands that are meant to fail | **Marked `ExpectFail`, printed as "(fails on purpose: …)"** | §4 shows the one-transaction load by running a copy that hits the PK |
| Workspace location | **`init [DIR]` (default `./dbc-tutorial`); `step`/`check` take `--dir` (default `.`)** | A global "current tutorial" setting would be state to manage |
| Output | **Text only for now** | `-t json` for `check` (CI-style) is under *Later* |

## Commands

```
dbc tutorial                      the lessons, with ✓ / ✗ / · when run in a workspace
dbc tutorial init [DIR] [--force] write dbc.toml, seed.sql and the empty dirs; say what's next
dbc tutorial step N [--dir D] [--run] [--files] [--force]
                                  print lesson N: what it teaches, its files (✓ present / + to write),
                                  its commands, and the doc section. --files prints the files' content.
                                  --run writes the missing files, runs the commands (echoing
                                  "$ dbc …"), then runs check N
dbc tutorial check [N] [--dir D]  check lesson N, or every lesson; exit 1 if a check fails
```

Exit codes follow the rest of dbc: `0` ok, `1` a check or command failed,
`2` bad usage (unknown lesson, not a tutorial workspace).

A directory counts as a tutorial workspace when its `dbc.toml` is there and
defines the `shop` and `warehouse` connections. Otherwise `step`/`check`
say so and print `dbc tutorial init`.

## Lessons and checks

Lesson numbers match the doc's section numbers. A check passes on state.
The SQL checks run `dbc -c CONN -t json "SELECT …"` and read the JSON.

| § | Files written | Commands (`--run`) | Checks |
|---|---|---|---|
| 0 | `dbc.toml`, `seed.sql` (by `init`) | `-f seed.sql` · `-t markdown "SELECT * FROM orders"` | shop has 4 customers and 6 orders |
| 1 | `scripts/order_summary.go` | `script order_summary` · `scripts` | the script exists; `script --check` is clean |
| 2 | `scripts/region_report.go` | `script region_report` | `data/reports/orders_{amer,emea,none}.csv` and `overview.html` exist |
| 3 | `bad.go` (outside `scripts_dir`, so it never runs by name) | `script --check order_summary region_report` · `script --check bad.go` | every script in `scripts/` checks clean |
| 4 | — | the two copies · the failing re-copy (ExpectFail) · the count | warehouse has `orders` = 6 and `paid_orders` = 4 |
| 5 | `scripts/load_warehouse.go` | `script load_warehouse` · the customers query | warehouse `customers` = 4, emails trimmed and lower case, no NULL region; `order_facts` = 6 |
| 6 | `pipelines/shop-sync.json` | `pipelines` · `pipeline check` · `pipeline run -p min_total=30` · the fragment preview | `pipeline check` is clean; `dim_customer` = 4; `load_log` ≥ 1; a `data/exports/big_orders_*.csv` |
| 7 | `plugins/order_tier.go`, `pipelines/order-tiers.json` | `plugins --check` · `pipeline run order-tiers` | `plugins --check` is clean; `plugins -t json` lists `order.tier`; `pipeline check order-tiers` is clean |
| 8 | `pipelines/warehouse-facts.json`, `pipelines/revenue-report.json`, `jobs/nightly-shop.json` | `job check` · `jobs` · `job run nightly-shop -p min_total=100` | `job check` is clean; a succeeded `nightly-shop` run record; a `data/exports/revenue_*.csv` |
| 9 | `scripts/orchestrate.go` | `script orchestrate` | the script checks clean; a `nightly-shop` run record whose trigger is `script` |
| 10 | — | `runs --limit 5` · `runs --sql …` | `runs -t json` returns at least one record |
| 11 | — | — (UIs: the lesson prints the key table) | nothing to check: said so, never failed |
| 12 | — | — (prints the pitfalls table) | nothing to check |

In §6 the real run comes **before** the fragment preview. The doc warns
that a preview on a fresh warehouse fails at `lookup`, because
`dim_customer` doesn't exist yet. `--run` uses the order that works.

## Package layout

```
tutorial/                     ◄─ new package, no dependency on main
    tutorial.go               Lesson, Command, Check types; File, WriteFile, Init, CheckWorkspace
    lessons.go                the table above
    checks.go                 check helpers: rowsEqual, fileMatches, clean, runRecord
    flow.go                   Show, Run, CheckLesson, Status
    runner.go                 Runner, ExecRunner, CommandLine (shell quoting)
    workspace/                embedded, //go:build ignore on every .go
        dbc.toml  seed.sql  bad.go
        scripts/  pipelines/  jobs/  plugins/
    tutorial_test.go          fake-runner unit tests + the doc-drift test
    e2e_test.go               builds dbc, runs init + every step --run + check (skipped with -short)
tutorialcmd.go                the cli.Command; the Runner that execs os.Executable()
tutorialcmd_test.go           argument and usage handling
```

- **`Runner`** is `func(ctx, dir string, args ...string) (Output, error)`.
  The command passes one that execs the binary. The unit tests pass a fake
  that returns canned JSON, so lessons and checks are tested without a
  database.
- **Streaming.** `--run` streams each child's stdout and stderr through to
  the terminal, so the learner sees what they would see typing it. Checks
  capture the output instead.
- **Errors** are wrapped with `serr` where they are failures (a file that
  can't be written, a config that can't be read). What a learner reads
  (✓ ✗, "found:", hints, "not a tutorial workspace") is plain text. A
  dbc log line in a check's output is cut down to its `msg` and `error`.

## Keeping the doc and the files in step

`TestWorkspaceMatchesDoc` reads `docs/scripting-and-etl-tutorial.md` and
finds each file's code block: the fenced block right after a line that
names the file (`` `scripts/order_summary.go`: ``, `` `dbc.toml`: ``, "Save
this as `seed.sql`"). It then compares the block byte for byte with the
embedded copy. `bad.go` isn't shown in the doc, so it is listed as
command-only, and the test makes sure every *other* embedded file is found.
A change to either side without the other fails `go test ./...`.

The e2e test is the other half. It runs every lesson for real on a fresh
build in a temp dir and checks each lesson passes, so a dbc change that
breaks the tutorial is caught too.

## Docs

- The tutorial gets a box at the top: "Prefer to follow along
  interactively? `dbc tutorial init`, then `dbc tutorial step 0`."
- README: `dbc tutorial` in the "New to scripts, pipelines and jobs?" note
  that links the tutorial (the README has no single table of subcommands).
- The dbc skill's subcommand table gets the row too.

## Phases

1. **Package and files.** `tutorial/` with the embedded workspace copied
   from the doc, the types, and the drift test. *Done when* the drift test
   passes.
2. **Lessons and checks.** The table, the check helpers, and fake-runner
   unit tests for pass, fail and hint on each check kind.
3. **The command.** `init`, `step`, `--run`, `check`, the list, and the
   workspace detection, wired into `newCLI()`.
4. **End to end.** The e2e test, plus a manual run of a fresh build in a
   scratch HOME and directory (per the dbc skill).
5. **Docs.** The tutorial box, the README row, and the skill row.

## What was built (step 1)

- `tutorial/` and `tutorialcmd.go` as laid out above; `tutorialCommand()`
  in `newCLI()`, and `tutorial` in `cats-plugin.toml`'s completions.
- Tests: `TestWorkspaceMatchesDoc`, `TestLessonsMatchDocHeadings`,
  `TestLessonFilesEmbedded`, fake-runner tests for each check kind and for
  `Run` (skip when done, ExpectFail, stop on failure), and
  `TestTutorialEndToEnd` (builds dbc and does every lesson, about 7s;
  skipped with `-short`).
- Checked by hand on a fresh build in a scratch HOME: `init`, every
  `step N --run`, a re-run (skipped as done), `check` on an empty
  workspace (✗ with hints), an unknown lesson and a foreign `dbc.toml`
  (both exit 2).
- `bad.go` is lesson 3's file, not `init`'s, so each lesson writes only
  what it introduces.

## Later

- **Step 2, a tutorial panel in `dbc web`.** It shows the lesson (built
  with element) next to the editor. *Try it* loads the step's script into
  a Go tab or its pipeline onto the canvas, and *Check* calls the same
  `tutorial` checks through a small API.
- **Step 3, the TUI.** The lesson text in the help pane, with `check` from a
  key.
- `dbc tutorial check -t json` for CI and tools.
- `dbc tutorial reset` (delete `shop.db`, `warehouse.bytdb`, `data/` and
  `runs/` after a confirmation).
- A section 3 exercise that has the learner *fix* `bad.go`, checked by
  `--check` reporting no warning.
