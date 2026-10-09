# Pipelines, Phase 5: the TUI — browser, run monitor, Runs list

Session: `00d45d50-d4d5-4ab6-97da-326569a017f9`

## Ask

1. `/sl` — load the last session (`2026-1009-1349-phase-4-jobs-tab-runs-view`)
   and the next-list.
2. "Do phase 5" — Phase 5 of `ai_docs/plans/pipelines.md` (N-174): in the
   TUI, the pipelines & jobs browser (`Ctrl+J`), the Runs modal, the run
   monitor tree, `$EDITOR` with the check, the status-bar indicator; the
   `Model` owns an engine; a TUI e2e step.
3. `/sw phase-5-tui-pipelines-jobs`.

## What was built

### The engine in the TUI (`tui/jobs.go`)

- `Model` owns a `jobs.Engine` (`initJobs`, before the first workspace):
  records in `runs_dir` (in memory under `Options.NoPersist`), `Recover`
  at start with its count in the startup log, `stopJobsForQuit` in
  `shutdown` (`Close(30s)`: each run canceled and rolled back; stderr says
  which). Every tab's workspace gets `Jobs: m.jobs.ScriptRunner()`, so a
  TUI script's `s.RunJob` runs there too.
- **`jobPump`**: the engine's Sink pushes into an unbounded FIFO; one
  goroutine hands the queue to `m.send` in order. Needed because Bubble
  Tea's `Program.Send` blocks (unbuffered channel) and the engine emits
  `RunStarted` from inside `StartPipeline`, which Update called — a direct
  `Send` would deadlock the program (and the engine sends under a run's
  event lock).
- `jobEvent`: `Logged` → the log on screen as `[nightly › clean] …`
  (`runPrefix`), `Preview` → the grid of the tab the run was started from
  (`runFrom`, `Workspace.ShowResult`, title as dbc web's `landPreview`;
  a background tab is marked •), `Notice` → the log, `RunDone` → the
  status text; `liveRuns` kept from the events so the status bar never
  asks the engine.
- Status bar: `● nightly 3m12s` (`+N`) at the right end, the hints giving
  way; a click opens the run's monitor or the Runs list (`lay.jobInd`).
- `jobTick` (1s, `jobTickEvery` a var for tests): elapsed times; every 2s
  the Runs list's disk check and the monitor's re-read of another
  process's live run; the browser's running marks.
- `startPipeline` / `startJob` (`By: "terminal"`), `askParams` (one
  prompt per param; Enter asks only those with no default, `p` all, each
  offered its default; Esc back to the browser).
- `Ctrl+C` does not quit over a live run (says so); `Ctrl+Q` does and
  stops it. `tui.Run` raises the in-memory SQLite pool to 16 (`memPool`),
  as dbc web does.

### The browser (`tui/pipes.go`)

- Sections Jobs (desc · `◷` schedule), Pipelines, Examples (both kinds),
  Trash (both kinds, folded); running marks `● 3m12s`.
- Keys as the scripts browser: `Enter` run, `p` run with every param, `e`
  edit (example: copy first), `n` new (pipeline: a source into a preview on
  the tab's connection; job: one step), `d` duplicate / copy an example,
  `r`/`F2` rename, `Del`/`x` trash (`t` shows it, `Enter` restores), `y`
  path, `h` the row's runs, `/` filter, `⌥J` all runs, `Ctrl+O` the scripts
  browser. Chips `ƒ Scripts ^O`, `◷ Runs ⌥J`, `+ New`; right-click menus.
- Made specs get their `name` set to the file's stem (`named`), as dbc web.
- `specEdited`: `checkSpec` (parse error placed by `pipeline.ParseErrorAt`;
  `pipeline.Check` with `jobs.CheckConns`, or `jobs.CheckJob`), each
  finding logged `path:line:col: where: msg` via `pipeline.Locate` /
  `jobs.Locate`.
- The scripts browser gained a `⇉ Pipelines & jobs ^J` chip and `Ctrl+J`.

### Runs (`tui/runs.go`)

- `runsModal` (`Alt+J`): `Engine.History` (limit 300), glyph coloured by
  state (`listItem.markSt`, new), kind · trigger · when · error, time ·
  rows; filter; `h`'s narrowing with `a` to widen; `^K` stops, `y` copies
  the id. Re-reads only when `userdata.RunsStamp` moved.
- `runMonitor`: run → pipelines → fragments → nodes, columns lined up
  (time; rows or a node's in → out; afters / direct COPY / batches /
  rows/s past 100ms / error); `Enter`/`←`/`→` fold; the row under the
  cursor narrows the log and pins its error; live by `apply` (line
  appended, fragment replaced, state re-read); `^K`, `y`, `Y`
  (`Run.Tree`), `⌫` back to the list. Sized to the run.

### Shared pieces

- `pipeline/locate.go`: `LineCol`, `Locate`, `ParseErrorAt` — moved from
  `web/pipelines.go` (which now calls them; behaviour unchanged).
- `jobs/locate.go`: `Locate` for CheckJob's wheres (step paths, top-level
  keys, `schedule[i]`).
- `jobs.CheckConns` (moved from `web`'s `checkConns`, which delegates).
- `userdata.RunsStamp`: newest record directory mtime.

### Docs

README: `Ctrl+J` / `Alt+J` rows in the TUI keys, a section "Pipelines and
jobs in the TUI". F1 groups "Pipelines & jobs (^J)" and "Runs (⌥J) and a
run". The dbc skill notes both UIs' views. The plan's Phase 5 outcome.

## Decisions and deviations

- **Keys follow the scripts browser**, not the plan's `d` trash / `r`
  runs: `d` duplicate, `r` rename, `h` runs.
- **`Alt+J` for the Runs list** (the plan named no key; `Alt+R` is the
  TUI's run-all).
- **No toolbar button**: a tenth button drops a 120-column terminal to
  glyph-only buttons; the scripts browser's chip is the mouse path.
- **Examples run as they are** (a spec example runs by name headless).
- **No origin** on TUI runs: the engine's one-run-per-origin rule is for
  dbc web's tab Stop; the TUI keeps `runFrom` for previews instead.

## Found and fixed on the way

- The `Send` deadlock above, designed out before it bit (and tested:
  `TestJobPumpKeepsOrderWithoutBlocking`).
- The monitor's name column did not count the fold mark and glyph, so
  node names were cut; times under 1ms read "0s" and rows/s from them
  read six figures — now `<1ms` and no speed under 100ms.
- The Runs list would have re-read every record file every 2s; now only
  when a record directory changed.
- e2e: a bare ESC followed at once by a key arrives as Alt+key (the
  decoder holds ESC); the step waits for the dialog to go first. Also the
  F1 step's probe for "Query tabs" fell below the fold with the new rows.

## Checked

`gofmt -l .` empty, `go vet ./...` clean, `go test ./...` green, `-race`
on `tui`, `jobs`, `web`, `userdata`, `pipeline`, `workspace`. The TUI e2e
suite (real binary in a pty) passed 4 runs in a row with the new step:
a pipeline run from `Ctrl+J` (monitor ✓, node counters, log lines, the
preview in the grid), `Alt+J` listing it beside a headless `dbc pipeline
run`'s run, Enter / Backspace, a two-step job, a waiting run on the status
bar with `Ctrl+C` refused, a click on the indicator and `^K` to canceled,
`e` with a broken spec (finding at `e2e_names.json:3:`) and back, and
`dbc runs -t json` listing the TUI's runs. The web e2e suite passed (two
Postgres steps skipped as usual).

## Next

Closed: None. Declined: None. Raised: N-186, N-187.
Deferred: None. Promoted: None.
Updated: N-115, N-174, N-183. Full list: `ai_docs/todo/next-list.md`.
