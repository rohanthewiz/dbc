# Pipelines, Phase 3: jobs, the scheduler, run records

Session: `2eb4b189-5525-4243-93a9-0f4ada3c226d`

## Ask

1. `/sl` — load the last session (`2026-1009-1122-phase-2-pipeline-tab`)
   and the next-list.
2. "Do phase 3" — Phase 3 of `ai_docs/plans/pipelines.md` (N-174): jobs (a
   DAG of pipelines), the in-house cron scheduler in `dbc web`, the webhook,
   run records as one JSON file per run, `s.RunJob`, `dbc job run|check`,
   `dbc jobs`, `dbc runs [--sql]`, `dbc run show`, the example job.
3. `/sw phase-3-jobs-scheduler-runs`.

## What was built

### `pipeline`

- `Options.Run`: the run's values, `${run.id|date|time|started|trigger|job}`
  (`pipeline.RunVars`), in any node; the clock ones default from the run's
  start, so they work from a script too. `Check` now refuses an unknown
  `run.` name ("is not a run value (run.date, …)").
- `Interrupted` status (a record whose writer died).
- `runner.report` copies a fragment's `Vars` as well as its nodes: the
  Progress snapshot shared the runner's live map, which the engine's record
  copies raced (a `fatal error: concurrent map clone and map write` in one
  of six `go test ./jobs` runs once checkpoints made copies frequent).
  `TestProgressSnapshotsAreCopies` fails without it.

### Package `jobs`

- `spec.go`: `Spec{Name, Desc, Root, Params, Pipelines []Step{ID, Pipeline,
  After, Params}, Triggers{Schedule, TZ, CatchUp, Webhook}, Policy{OnFailure,
  MaxParallel, Overlap, Timeout}}`, `ParseJob` (unknown keys refused),
  `RootID` (root may be left out), `Downstream`, `Schedules`, `NextFire`.
  `CheckJob`: names, the DAG (dangling/self/duplicate afters, one root that
  is `root`, Kahn for cycles, reach), each step against its pipeline
  (`CheckOptions.Find`: exists, its own Check, params both ways, `${…}` a
  job param or a run value), cron lines (parse, fire at all), the zone,
  the policy.
- `cron.go`: five fields, names, lists, ranges, steps (`5/15` is
  `5-59/15`), macros, Vixie's either-day rule (a field starting with `*`
  counts as unrestricted). `Next` walks naive wall-clock time (UTC) and maps
  each match to the earliest instant showing it (`instant`, trying the
  offsets ±26h around); a skipped wall time maps to the jump
  (`ZoneBounds`). So `30 2 * * *` fires at 03:00 on spring-forward day and
  `30 1` once on fall-back day. `NextN` for editors.
- `engine.go` rewritten around one `Run` shape for both kinds: `Kind`
  (pipeline|job), `By`, `PipelineRun{ID, After, Params, RunStats}`,
  `Line.Pipeline`. Triggers manual, preview, schedule, webhook, script, cli.
  `runOne` (one pipeline on its own session) is shared by `execute` (a
  pipeline run) and `runStep` (a job step); `finish` makes the record final,
  writes it, prunes, releases holds, sends `RunDone`. `background` runs the
  progress ticker and the record flusher. One real run of a pipeline at a
  time engine-wide (`pipes`, a job step included; the refusal names the
  job and step). New events `State` (a step's or a queued run's state) and
  `Notice` (the scheduler's words; a record that could not be written).
  `Options` gained `RunsDir`, `RunsKeep`, `FlushEvery` (2s), `StaleAfter`
  (30s), `Find`.
- `job.go`: `StartJob(JobRequest{Spec, Params, Trigger, By, Origin,
  Source})` — checks, settles job params, loads every step's pipeline up
  front (the run keeps those copies), lists every step queued. Overlap
  `skip` refuses a second run; `queue` accepts one queued run (status
  queued) that starts when the one before ends; a third is refused.
  `executeJob` is the readiness walk with `max_parallel`; a failure skips
  everything downstream (`State` skipped each); `stop` cancels the walk with
  a cause; `timeout` is `WithTimeoutCause`. Final: canceled (user/Close),
  failed "timed out after …", failed with the first failing step, or
  succeeded; a summary line (`job nightly succeeded in 26ms: copy ✓ 8 rows,
  …`). Step params are substituted at the step's start and recorded.
- `records.go`: `save` (once-told failures), `checkpoint` (at a step's start
  and end), `prune` (never a record another process still writes),
  `stale`/`interrupt` (a running or queued record not rewritten for
  `StaleAfter` is interrupted: running parts interrupted, queued ones
  skipped), `fromDisk` (Get falls back to it), `Recover` (rewrites them;
  dbc web and `dbc job run` call it at start), `History(userdata.RunFilter)`
  (live + disk, settled), `LastRun`. Previews are never recorded.
- `scheduler.go`: `Engine.NewScheduler(SchedOptions{Dir, MissGrace,
  MaxWait, Now})`, `Start`, `Stop`, `Reload`, `NextFires`, `Step`. Scans
  `jobs_dir` (never the examples) every step; keeps fires across a reload
  unless the tz or cron lines changed; a broken file is told once. A fire
  later than `MissGrace` (1 min) is a miss (told) unless `catch_up`; at the
  first scan `catch_up` runs the latest fire after the job's newest record.
  Wakes at least once a minute (timers stand still in sleep; the wall clock
  is what cron means). A fire is claimed first with an O_EXCL file in
  `runs_dir/.fires` (swept after two days), so two dbc web processes on one
  machine run it once.
- `find.go`: `PipelineFinder(dir)` and `LoadJob(dir, name)`: the dir, then
  the examples, `.json` optional.
- `script.go`: `Engine.ScriptRunner()`; `sdb.DefaultJobRunner =
  runOnOwnEngine` (a private engine over the session's manager and paths,
  its lines printed `[step] …`). A canceled job returns a wrapped
  `context.Canceled` so `sdb.IsCanceled` holds.
- `tree.go`: `Run.Tree()`, the drilldown as text (run, pipelines with
  `after`, fragments with node in→out, errors once).

### `sdb`

`sdb/jobs.go`: `s.RunJob(name, params) (*JobRun, error)`, `JobRun`,
`JobPipeline{ID, RunStats}`, `JobRunner`, `DefaultJobRunner` (a var, so
sdbapi does not list it), `WithJobs` and `Manager` (host-only). `Paths`
gained `JobsDir`, `RunsDir`. Exported to yaegi (`JobRun`, `JobPipeline`);
`api.json` regenerated.

### `userdata`, `config`, `scripts`

- `specstore.go`: the pipelines store generalized (`specStore`: list, read,
  save, rename, trash, restore; `SpecTrashInfo`); `pipelines.go` now wraps
  it (its API unchanged, `PipelineTrashInfo` an alias); `jobs.go` the same
  for jobs (`JobInfo` with steps, schedule, webhook).
- `runs.go`: `SaveRun` (0700 dirs, 0600 file, atomic), `ListRuns`
  (`RunFilter{Kind, Name, Status, Since, Limit}`, `RunHead` with rows and
  the file's mtime), `ReadRun` by id (glob over kinds and names),
  `PruneRuns` (a `live` predicate), `ValidRunID`.
- Config `jobs_dir`, `runs_dir` (resolved like `pipelines_dir`), `runs_keep`
  (default 200); `FindJob`/`JobRef`. `dbc.example.toml` and the README's
  config sample say so.
- Examples: `scripts/jobs/nightly.json` (copy → clean ∥ breeds → report,
  param `min_age`, a cron line to copy) and a fourth pipeline,
  `scripts/pipelines/breed-counts.json` (bytdb `cats_copy` → `breed_counts`,
  key `breed`). `scripts.Jobs()`, `JobByName`.

### CLI (`jobscmd.go`)

`dbc jobs` (schedule, next fire for your jobs only, last run, kind),
`dbc job run [-p k=v]` (own engine, `Recover` first, memory pool raised for
the fan-out, lines `[step] …`, preview rows as a script's, the tree or `-t
json` the whole record; exit 0/1/130, Ctrl+C cancels first), `dbc job
check`, `dbc runs [--job|--pipeline] [--status] [--since 7d|36h|date]
[--limit]`, `dbc runs --sql` (tables `runs`, `pipelines`, `fragments`,
`nodes` in a throwaway bytdb file, removed before the results are written),
`dbc run show ID`. `dbc pipeline run` now goes through an engine as well,
so it leaves a record (previews do not). The cats completion manifest lists
the new commands and flags.

### dbc web

- `web/jobs.go`: the store (the pipelines' protocol; "jobs" window event,
  and the scheduler reloads), `GET /api/v1/jobs` (rows with next fire and
  last run; examples never scheduled), `POST /api/v1/job-check` (diags and
  each cron line's next five fires), `POST /api/v1/jobs/:name/run` —
  manual with the session cookie, the webhook with the Bearer secret (403
  unless the job sets `"webhook": true`; `By` is the caller's IP), 409 on
  overlap. `GET /api/v1/runs` adds `runs` (history) when given a filter;
  `GET /api/v1/runs/:id` reads back from disk.
- `Server`: the engine with `RunsDir`, `Recover` at `New`, `sched` made in
  `New` (no field race with handlers), started in `Run`, stopped first in
  `Shutdown`; every workspace gets `Jobs: s.jobs.ScriptRunner()`
  (`workspace.Options.Jobs` → `WithJobs` in `RunScript`). `onJob` sends
  `job.state` and `job.notice` (the notice also to dbc web's stdout).
- `pipelines.js`: job runs are kept apart (`jobRuns`), so a job named
  `nightly` cannot light up `nightly.json`'s canvas: their start and end are
  a line each in the log on screen, their lines kept under `\x01job:<name>`
  for the jobs tab; `sync` skips them; notices are logged.

### Tests

`jobs`: `TestCronNext` (a table incl. month ends, leap day, Feb 30 never,
Vixie both ways, both New York DST days), `TestCronFallBackFiresOnce`,
`TestCronParseErrors`, `TestCheckJob` (one case per rule),
`TestJobDiamondFansOutAndIn` (the middle steps meet each other, so only
`max_parallel: 2` gets through; d after both), `TestJobFailurePolicies`
(finish_branches / stop), `TestJobOverlap` (skip, queue, a second waiter,
the pipeline held by the job), `TestJobTimeout`, `TestJobParams`,
`TestJobRecords` (live heartbeat, final, a dead record read interrupted,
`Recover`, `LastRun`, a preview unrecorded), `TestExampleJobRuns` (the
example on both demos), `TestScriptRunsAJob` (both runners),
`TestSchedulerFires` (on time, once, overlap told, a miss told),
`TestSchedulerRescans`, `TestSchedulerCatchesUp`,
`TestSchedulersShareAFire`. `userdata`: `TestRunRecords`, `TestJobsStore`.
`pipeline`: `TestRunValues`, `TestProgressSnapshotsAreCopies`. Root:
`TestParseSince`, `TestRunsSQLTables`. `web`: `TestJobStore`,
`TestJobCheckRoute`, `TestJobRunAndWebhook`. Web e2e: a new step "job runs
in the log" (`web/e2e/jobs_test.go`).

## Decisions and deviations

- **No `"manual"` trigger key**: running by hand (UI, CLI, script) is always
  allowed; the webhook is opt-in per job.
- **Job params** were added (the plan only had step params): a step's
  values substitute `${param}` and `${run.…}`.
- **`StartJob` takes a parsed spec** (`JobRequest`), as `StartPipeline`
  does; `LoadJob` resolves a name.
- **Interrupted by heartbeat age, not PID**: a record a process still
  writes is rewritten every 2s; 30s of silence means its writer is gone. A
  `dbc web` starting while a cron's `dbc job run` runs does not kill its
  record.
- **The scheduler is dbc web's only, for `jobs_dir` only** — the example's
  cron line is there to copy, not to fire on every machine.
- **Fire claims** (O_EXCL files) rather than a scheduler lock: nothing to
  release when a process dies, and either dbc web can fire.
- **A job step whose pipeline is already running fails** (one real run of
  a pipeline at a time); it does not wait.
- **`dbc job run --wait=false` not done** (N-181).
- `dbc pipeline run` records its runs now (it runs through an engine).

## Checked

`gofmt -l .` empty, `go vet ./...` clean, `go test ./...` green, `-race` on
`jobs`, `web`, `workspace`, `pipeline`, `userdata`, `config` and the root
(jobs ×3, the others ×2), `go test ./jobs` 15 times clean after the Vars
fix. The full web e2e suite passed against the built binary in headless
Chrome; the new step too. The real binary, in a throwaway HOME:
`dbc job run nightly -p min_age=3` (tree, preview rows, exit 0), a failing
job (exit 1, the error pinned to its node), Ctrl+C (exit 130, record
canceled), `kill -9` mid-run (running, then interrupted after 33s, and
`dbc web`'s start rewrote it), `dbc runs` filters, `--sql`, `run show`;
`dbc web` fired a `* * * * *` job at 12:38:00 exactly, ran one by curl with
the Bearer secret, refused a job without `webhook` (403), and stopped
cleanly on Ctrl+C.

## Next

Closed: None. Declined: None. Raised: N-181, N-182.
Deferred: None. Promoted: None.
Updated: N-174. Full list: `ai_docs/todo/next-list.md`.
