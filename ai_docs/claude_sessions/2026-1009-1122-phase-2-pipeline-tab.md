# Pipelines, Phase 2: the pipeline tab in dbc web

Session: `26acad01-e89b-476d-ad0f-0a304fbc9181`

## Ask

1. `/sl` — load the last session (`2026-1009-1004-phase-1-pipelines`) and
   the next-list.
2. "Start phase 2" — Phase 2 of `ai_docs/plans/pipelines.md` (N-173): the
   pipeline tab in `dbc web`, so a pipeline can be built, previewed and run
   without writing JSON or Go.
3. `/sw phase-2-pipeline-tab`.

## What was built

### Package `jobs` (new): the engine's first half

`jobs/engine.go`. One `Engine` per process, owned by the host (dbc web's
`Server.jobs`), running pipelines outside any workspace's run slot,
several at once.

- `StartPipeline(Request{Spec, Params, Fragment, PreviewRows, Trigger,
  Origin, Source})` returns the run's header at once and runs it on its own
  goroutine with its own `sdb.S` (DDL log on, `Release` at the end, paths
  set), the spec cloned so a canvas edit cannot reach a running fragment.
- Refused up front: a spec `pipeline.Check` rejects; a second real run of
  a running pipeline, or a second run from the same origin (`ErrBusy`, a
  `busyError` whose text says what is running; `BusyRun`); a start after
  `Close` (`ErrClosed`). A preview beside a run is allowed.
- `Run` record, shaped for Phase 3: id (`20261009-020000-7f3a`), kind,
  name, source, trigger, params, fragment, preview rows, origin, times,
  status, error, `Pipelines []PipelineRun` (an id plus `pipeline.RunStats`;
  every fragment listed queued from the start, and the ones a failure never
  reached marked skipped), the log (capped at 2000 lines). The last 50
  finished runs are kept in memory (`Recent`); `Get`, `Running` (by start
  time, not id), `Wait`, `Cancel` (a finished run's cancel is not an
  error), `Close(grace)`.
- Events, one run's in order (a lock per run), never under the engine's
  lock: `RunStarted`, `Progress` (a state change at once, batch counters
  coalesced to `ProgressEvery`, 250 ms, by a ticker that stops before
  `RunDone`), `Logged`, `Preview`, `RunDone` (sent before `Wait` returns).
- Errors are logged and recorded with their serr context (`errText`):
  "unknown connection — conn demo-sqlite · fragment clean · node src ·
  plugin sql.read".
- It imports `script` for its side effect, so every host has the `go.*`
  plugins.

### `pipeline`

`Options.Progress` now also reports each fragment's start (running, nodes
listed) and end (final status — skipped included, for `Options.Fragment`
and for an action in a preview), through `runner.report`, so a host draws
states from the one callback. `api.json` regenerated (the field's doc
comment changed).

### `workspace`

`ShowResult(key, title, r)`: a result from outside the run slot lands as a
script's `s.Show` does — the first of a key placed as a run's result (in
the tab on screen unless pinned), later ones joining it behind "Result 1 ·
2"; a new key starts over. Refused while the tab's own run is in flight.
`showLocked` became `showIntoLocked(…, **resultTab)` with `w.showTab` /
`w.outTab`.

### `web`

- `web/pipelines.go`: the store (the scripts protocol for `.json` files:
  list with examples and trash, read, save with base revision and conflict,
  rename, trash, restore; a save must be a JSON object), examples,
  `pipeline-check` (unsaved text; a parse error placed at its offset or at
  the unknown key, Check's diags placed by line with `locate`, connection
  names accepting a `<conn>/<database>` that resolves), `pipeline-preview`
  (the editor's text, rows 1–1000, default 50), `pipeline-run` (the saved
  file, else an example), `pipeline-export/:name`, `plugins` (with the
  connection names), `runs`, `runs/:id` (with the log),
  `runs/:id/cancel`. Window events `pipelines`, `job.run`, `job.progress`,
  `job.line`, `job.preview`, `job.done`. A preview's rows land in the
  origin tab's workspace and that tab gets the "result" event a script's
  show sends; its title is `preview <fragment>/<node>`, once.
- `Server`: `jobs` made in `New`, closed first in `Shutdown`.
- Saved tabs: a `pipeline` column (`ALTER … ADD COLUMN IF NOT EXISTS`),
  validated by `ValidPipelineName`, never beside `script`; no console for a
  pipeline tab.
- `pages/workbench.go`: `#pipe` inside `.editor-wrap`; `stage.js` and
  `pipelines.js` loaded before `app.js`.

### The page

- `web/static/js/pipelines.js` (`dbc.pipelines.create(host)`): the file
  (load, drafts in `dbc.pipe.draft.<name>`, save with a conflict dialog —
  keep mine or load the file's), the check (500 ms after an edit), the
  model edits (add, wire with the fragment's rules — one input per node, no
  loops, a new wire replaces the old —, remove, duplicate, rename node and
  fragment with `${frag.…}` references following, move fragments), the
  canvas (lanes, cards with ports and counters, Bezier wires with click
  targets, auto-placement by depth, a dropped card sliding to the nearest
  free spot, the first look fitted down to 75 % when bigger than the
  pane), the inspector drawn from plugin fields, the pointer (palette drag
  with a ghost, card drag snapped to 10 px, wire drag, pan; Delete, Ctrl+D,
  Escape, f), the JSON view, previews, runs (the newest run of a pipeline
  chosen by event order and start time, not by id), Stop, params dialog,
  "Run with parameters…", Export as Go into a script tab, browser actions.
  `specText` writes a spec as `Spec.JSON` does.
- `stage.js`: pan and zoom (`dbc.stage`): Ctrl/⌘+wheel zooms at the
  pointer, a plain wheel pans, `fit`, `toStage`.
- `app.js`: the tab kind (`t.pipeline`; `.script-mode` plus `.pipe-mode`,
  `.pipe-json`), activation, header, log keyed `\x01pipeline:<name>`,
  save/check/run/stop routing, the tab menu, close-with-changes, rename,
  boot, `pipeKit.sync()` at the first stream open and on resync, the busy
  mark from engine runs, a connection click setting the selected node's
  connection, `focusWork` (the canvas, never the hidden editor), help keys.
- `editor.js`: `defineJSON` — a Monarch grammar `dbcjson`, defined before
  the first model.
- `scripts.js`: the browser's Pipelines section, pipeline examples,
  pipeline trash, New pipeline; the kit exposes `make`, `refreshBrowser`,
  and `dbc.scripts.ask` / `ago`.
- `grid.js`: an empty grid's hint for a pipeline tab. `app.css`: the canvas.

### Tests

`jobs/engine_test.go` (a run of the clean-and-load example with states and
log, a preview, busy refusals and cancel, a failure skipping the rest with
context, refusal and close), `pipeline` `TestRunProgressStartBatchesEnd`,
`workspace` `TestShowResultFromOutside`, `web/pipelines_test.go` (store,
check placement, preview landing in the tab with events, run and cancel,
export and plugins, saved tab). The web e2e step "pipeline tabs"
(`web/e2e/pipelines_test.go`): a new pipeline from the browser, the
inspector, two palette drags (no card covering another), two wires by
hand, a preview into the grid with the switcher, Ctrl+S on disk, a run
with counters and the summary, the JSON view, a reload keeping the draft, a
JSON edit round-tripping, a waiting Go source run then stopped, Export as
Go. `DBC_E2E_SHOTS=dir` keeps its screenshots (`shot`).

## Decisions and deviations

- **No `pipeline-schema` route.** The vendored Monaco registers a `json`
  language whose module (the JSON language service) is not shipped, so a
  `json` model fails loading it and nothing could consume a schema; the
  JSON view uses `dbcjson` and the check's markers.
- **`plan.js` keeps its own pan/zoom**: it is inlined alone into the
  standalone plan page under a CSP hash.
- **Runs belong to the engine**; the tab is only the origin (grid, busy
  mark, Stop). One real run per pipeline, one run per origin.
- SQL and Go fields are code boxes (N-175); `table` is a plain line
  (N-176); fragments reorder from the lane's ⋯ menu (N-179).
- The page never logs `job.done`: the runner's summary and the engine's
  failure line are already there.
- Under the canvas the editor holds the pipeline's JSON, hidden by
  `visibility`, so it cannot take focus and keys never edit it unseen.

## Found and fixed on the way

- The web e2e step "script tabs" had failed since Phase 1, on a clean HEAD
  too: the suggest widget draws only the rows in view, and the new `S`
  methods pushed `Query` and `Show` out of it. It now narrows by prefix.
- Screenshots showed a dropped card covering another, the run's summary
  logged twice, `preview load/load/peek` titles, and errors without
  context; all fixed.
- A run started in the same second as the previous one could lose to it on
  the canvas (ids sort by start to the second); the page now goes by event
  order and start time.

## Checked

`gofmt -l .` empty, `go vet ./...` clean, `go test ./...` green, `-race`
on `jobs`, `web`, `workspace`, `pipeline`, `script`. The full web e2e
suite (`DBC_E2E=1`) passes against the built binary in headless Chrome; the
pipeline step passed 3 of 3 repeated runs after the run-order fix.
Screenshots of the canvas (a new pipeline, the clean-and-load example, a
failed preview, the JSON view) were looked at. A `dbc web` with a pipeline
left running shuts down at once on Ctrl+C.

## Next

Closed: N-173. Declined: None. Raised: N-175, N-176, N-177, N-178, N-179, N-180.
Deferred: None. Promoted: None.
Updated: N-061, N-064, N-174. Full list: `ai_docs/todo/next-list.md`.
