# Pipelines, Phase 4: the jobs tab and the Runs view

Session: `c7599a51-deab-48f9-a5a4-a3f209c3e6a2`

## Ask

1. `/sl` — load the last session (`2026-1009-1254-phase-3-jobs-scheduler-runs`)
   and the next-list.
2. "do phase 4" — Phase 4 of `ai_docs/plans/pipelines.md` (N-174): the
   `dag` layout package extracted from `erd/layout.go` (erd's output
   unchanged) and the layout route; `jobs.js` (the DAG canvas, triggers
   with next fire times, policy, Run); `runs.js` (the Runs table with
   filters and live rows, the run page with three drilldown levels, the
   fragments timeline, the node table, the filtered log, *Open preview*,
   catch-up on reload); `dbc run cancel`; route tests and an e2e step.
3. `/sw phase-4-jobs-tab-runs-view`.

## What was built

### Package `dag` (new)

`dag/dag.go`: `Edge{From, To}`, `Ranks` (longest path, back edges cut by a
DFS in index order, duplicates and self loops one constraint and none,
roots pulled right), `Layers`, `Neighbours` (one per edge, so a pair joined
twice pulls twice), `Order` (barycenter, positions updated as each layer
sorts, stable), `WrapTarget`, `Wrap`, `Place` (columns left to right,
centred; optional `extra` gap per row), `Placement{X, Y, Col, ColX, ColW, W,
H}`, and `Layout` running them all (`Options{X0, Y0, ColGap, RowGap,
Sweeps, Wrap}`).

`erd/layout.go`'s `layGroup` now builds the group's edges in relationship
order and calls `Ranks`, `Layers`, `Order`, `Wrap`, `Place` (inside its
`place` closure, which widening calls again); the box keeps `rank` and
`col`, loses `pos`. **Proof of no change**: a temporary test fingerprinted
(sha256 of every box's position, rank, column and every path's points) the
fixtures, `star(12/45/120)`, `chain(12)`, `fanOut(60, 6)` and 300 random
schemas (cycles, duplicate keys, self-references) before and after: all 306
identical. The test was then removed; erd's own tests pass.

### Package `jobs`

- `layout.go`: `LayoutSteps(ids, after)` (unknown or self afters ignored, a
  repeated id keeps its first place), `Spec.Layout`, `Run.Layout` (from the
  record's `PipelineRun.After`), `Layout{Nodes map[id][x,y], W, H, Card}`.
  Cards 196×74, ranks 70 apart, steps 22 apart, 20 padding; never wrapped
  (a second column of one rank would read as "runs after").
- `Engine.Cancel`: a run not in this engine whose record (settled) says
  running or queued is another process's — `ErrElsewhere`, with words, no
  longer a silent nil.
- `Spec.JSON` compacts arrays of scalars (`pipeline.CompactArrays`,
  exported from `Spec.JSON`'s pipeline side).

### `web`

- `job-check` returns `layout` (none for unparsable text);
  `GET /api/v1/jobs/:name/layout` (the file, else the example);
  `GET /api/v1/runs/:id` is now `runRecord{jobs.Run; Layout}` (a job's);
  `POST /api/v1/runs/:id/cancel` maps `ErrElsewhere` to 409, unknown to 404.
- Saved tabs: a `job` column (`ALTER … ADD COLUMN IF NOT EXISTS`),
  `ValidJobName`, one file per tab (script, pipeline or job), no console.
- `pages/workbench.go`: `#jobp` (class `pipe`) in `.editor-wrap`, the
  topbar's ◷ Runs (`#runs-btn`), `runs.js` and `jobs.js` before `app.js`.

### The page

- `runs.js` (`dbc.runs`): `drawDag` (cards at the server's layout, Bezier
  edges, optional hit strokes; a step not in the layout yet goes right of
  the rest), `critical` (back from the step that ended last through the
  upstream that ended last), the kit (`records` from job.* events, `local`
  ids of this engine's runs, `fetchRun` with lines that race the read kept
  aside and de-duplicated, rAF-throttled redraws, `sync`), `mount` (a view:
  list with filters — which job or pipeline, status, age — and live rows
  over the history; the run page: bar with Stop, ⧉ id, ↗ file; the DAG; a
  step's fragments on the run's time axis; a fragment's node table with
  rows/s and pinned errors; values; the log narrowed by step and fragment
  — a fragment's lines are those naming it or logged while it ran; scroll
  and log stickiness kept across redraws; what a click opens scrolled into
  view; a run of another process re-read every 2 s; a 1 s tick while live;
  `soft` redraws wait while a filter select has focus), `open` (the Runs
  dialog, Backspace back). Open preview: the origin tab, else ◎ Preview
  again (opens the pipeline tab, waits for its workspace, previews).
- `jobs.js` (`dbc.jobs`): `jobText`/`parseJob`, the file (load, drafts
  `dbc.job.draft.<name>`, save with conflict dialog), the check (at once
  for a change of shape, 500 ms for typing; diags placed on the step's
  line for the JSON view's markers; fires drawn in place), model edits
  (addStep, connect refusing loops, removeEdge, removeStep, duplicateStep,
  renameStep), the canvas (stage.js; cards with last run's state, ◉ root,
  ⚠; ports), the palette of pipelines (re-read each time a job tab shows),
  the pointer (palette drag onto a card or the canvas, wire drag, select,
  pan), keys (Delete, Ctrl+D, Escape, f), the inspector (job: name, desc,
  Run buttons, params, schedule lines with next fires, tz, catch up,
  webhook URL and ⧉ curl, policy; step: id, pipeline with datalist and ↗,
  afters with loop-making ones disabled, the pipeline's params with
  defaults fetched once; dependency), the JSON view, faces (⊞ Design / ◷
  Runs), run (save, params, POST, land on the run page), stop, `live`
  runs for busy marks, `lastRun` (cards' states from the first look), the
  job.* and "jobs" events (log lines `[step] …` under `\x01job:<file>`,
  start/end also on screen when the tab is not), browser actions.
- `app.js`: the tab kind `t.job` (`.job-mode` with `.script-mode` and
  `.pipe-mode`), activation, saved body, boot, strip (⧉), menus
  (`jobTabItems`), run/stop/save/check/rename/close routing, log keys,
  `engineRunState` shared with pipeline tabs, `runsKit` and `jobKit`
  hosts, Alt+R, `dbc.cmd.runs`, help sections "Job tabs" and "Runs".
- `pipelines.js`: job runs are only recognised and passed over now
  (`jobRuns` a Set); their logging moved to `jobs.js`.
- `scripts.js`: Jobs, Job examples, job trash, + New ▾ → Job, F2 and
  Ctrl+Del on jobs. `app.css`: DAG cards and edges, job tab, Runs view.

### CLI

`dbc run cancel ID [--url U] [--secret S]` (`$DBC_WEB_URL`,
`$DBC_WEB_SECRET`): reads the run first (an ended one: "had already ended:
<status>"), POSTs the cancel with the Bearer secret, follows the record up
to 15 s and says how it ended; messages for no dbc web, a refused secret,
no secret, 404, 409. `webClient` unwraps dbc web's envelope.

## Decisions and deviations

- **No positions of the user's on the job canvas**: the server's layout
  places every card, re-fetched (via job-check) at once after a change of
  shape.
- **The Runs view is a dialog plus a job tab's face**, not a tab kind.
- **The layout route the tab uses is job-check**, which lays out the text
  being edited; `GET /api/v1/jobs/:name/layout` is there for a name only.
- **Open preview** cannot recall a past run's rows (not recorded, N-185):
  it switches to the origin tab, or previews again.
- **`dbc run cancel` needs dbc web's secret** given to both: dbc web keeps
  its secret in memory only (deliberately), so no file holds it.
- A run another process runs cannot be cancelled from dbc web (N-183).

## Found and fixed on the way

- `Engine.Cancel` returned nil for a run another process was running, so a
  page's Stop or the API looked like it worked; now 409 with where it runs.
- My e2e's first Delete check expected the tab dirty after add + delete —
  the canvas writes the file's very text back, so it is not; the test was
  wrong, not the code.
- The run page in a job tab sits in the editor pane: a step's fragments
  opened below the fold, and a click there missed. Clicks now scroll what
  they opened into view (by hand; `scrollIntoView` would also scroll the
  workbench's clipped boxes).

## Checked

`gofmt -l .` empty, `go vet ./...` clean, `go test ./...` green, `-race` on
`dag`, `jobs`, `web`, `erd`. The full web e2e suite passed in headless
Chrome against the built binary, including the new step "job tabs and the
runs view" (the example copied and laid out by rank, a palette drop on a
card and Delete, a wire, Ctrl+S on disk, ▶ Run to succeeded with a critical
path, a step's fragments and a fragment's node counters, the job's list,
Alt+R with Enter and Esc, the design cards ✓, a reload restoring the tab
and its states, + New ▾ → Job with a dropped step run live — the run page
●, the tab busy, the dialog "1 running" — and ■ Stop to canceled).
Screenshots of the job tab, the run page, the fragment drilldown, the
dialog (idle and live) and the light theme were looked at. The real binary
in a throwaway HOME: `GET …/jobs/nightly.json/layout`, `dbc run cancel` of
a webhook-started run (stopped), again (already ended: canceled), unknown
(404), wrong secret, no secret, nothing listening, a bad id (exit 2), and a
`dbc job run` in another process (409 "is running in another process");
dbc web stopped cleanly.

## Next

Closed: None. Declined: None. Raised: N-183, N-184, N-185.
Deferred: None. Promoted: None.
Updated: N-061, N-064, N-174, N-177, N-178, N-180. Full list: `ai_docs/todo/next-list.md`.
