# Pipelines, Phase 1: the `pipeline` package, plugins, the builder and the CLI

Session: `c204b3a8-b51f-40f3-8ee3-56153875238c`

## Ask

1. From the cats-todo backlog: "Take a deep look at the scripting layer of
   dbc and come up with a plan to take it to the next level" — UI-first with
   Go scripting at the core; plugins (sources, transformers, sinks);
   drag-and-drop connections; batch processing within a fragment; fragments
   in sequence as pipelines; pipelines in a DAG as jobs, the root one
   triggered by schedule, hand or an external call; fan-out and fan-in; a
   monitoring view with drilldown.
2. "Go with the recommended decisions, start Phase 1."
3. `/sw phase-1-pipelines`.

## The plan

`ai_docs/plans/pipelines.md` (963 lines), in the house plan format. Three
exploration agents mapped the web layer, the TUI/workspace/userdata/config
layer, and the ETL internals first. The decisions table was accepted as
recommended; the ones that shape the work:

- A JSON spec file is the source of truth for a pipeline; Go appears as
  inline node code, plugin files, the `sdb` builder, and *Export as Go*.
- Runs execute in a server-wide engine (Phase 3), not a tab's run slot.
- `dbc web` fires schedules; external cron calls `dbc job run`.
- Run history is one JSON file per run under `~/.config/dbc/runs/`, not a
  bytdb file: bytdb's exclusive sidecar lock would stop a cron-driven run
  from recording while `dbc web` holds it. bytdb comes back on the read
  side (`dbc runs --sql`, Phase 3).
- Rows pass between fragments only through the database; scalars as
  `${frag.<name>.<key>}`.
- A synchronous batch loop inside a fragment for v1; the node interfaces
  allow a goroutine-per-node driver later.
- An in-house five-field cron parser (Phase 3); a `dag` layout package
  extracted from `erd/layout.go` (Phase 4).
- Phases: 1 core headless → 2 web pipeline tab → 3 jobs/scheduler/records
  → 4 jobs canvas and Runs view → 5 TUI → 6 plugin SDK and the rest of the
  built-ins.

## What was built (Phase 1)

### Package `pipeline` (new, ~2.3k lines with tests)

- `batch.go`: `Batch{Cols []Col; Rows [][]any}` with `Col`, `Get`, `Set`,
  `AddCol`, `Drop`, `Keep`, `Rename`, `Filter`, `Clone`, `Result`;
  `FormatValue` as the grid renders a value.
- `node.go`: `Host` (what a node may ask of the session — `*sdb.S`
  satisfies it; a test runs the runner on bare `*sql.DB`s), `Env` (session,
  params, vars, batch size, `Logf`, `Stop`, `SourceEngine`), the `Source`,
  `Transform`, `Sink`, `Action` interfaces, `Stats`, and the record types
  `RunStats`/`FragmentStats`/`NodeStats` with `Status`.
- `plugin.go`: `Plugin{Name, Kind, Label, Doc, Fields, New, Check}`,
  `Field{Type: string|text|int|bool|duration|enum|conn|table|columns|sql|go}`,
  `Config` (all strings, with typed readers), the registry, `Validate`,
  `Defaults`.
- `spec.go`: `Spec`/`Fragment`/`Node`/`Edge` (an edge marshals as a
  two-element array; `JSON()` keeps scalar arrays on one line), `Parse`
  (unknown keys refused), `${…}` substitution (`Refs`, `Subst`), `ValidName`.
- `check.go`: `Check(spec, CheckOptions{Conns})` → `[]Diag{Where, Severity,
  Msg}`: names, plugin fields, the plugin's own `Check`, references (a
  param, `frag.<earlier>.<key>`, `run.*`), connection names, and the shape
  (one source; one input per node; sinks at the leaves; everything
  reachable; an action alone).
- `run.go`: `Run(ctx, host, spec, Options{Params, Log, Progress, Fragment,
  PreviewRows})`. A fragment builds its nodes (substitute, default,
  validate, `New`), then: an action runs; the direct shape (`sql.read` or
  `sql.table` on Postgres straight into `sql.write` on Postgres) runs as
  `etl.Copy`; otherwise the batch loop — source `Next` → push through the
  tree (every child but the last gets a `Clone`) → transforms' `Flush` in
  order → sinks' `Commit` in node order → source `Close(true)` last. Any
  failure aborts every open sink and closes the source as failed. A sink no
  rows reached, with nothing but the source above it, is still opened and
  committed so a reload from an empty source still truncates. Preview
  reshapes the fragment: a `rows.limit` after the source, every sink a
  `preview`, a preview after any leaf transform; actions are skipped.
- `builder.go`: `New(name).Desc().Param()`, `Fragment(name).Batch().Node()/
  Named()/Then()/Func()/ThenFunc()/Wire()`; a Go value as a node (`Source`,
  `Transform`, `Sink`, `Action`, or a func of five shapes).
- `gen.go`: `Gen(spec)` writes the builder form as a dbc script, ids kept.
- `builtin_sql.go`: `sql.read`, `sql.table`, `sql.write` (create from the
  columns via the new `etl.CreateStmt`; DDL outside the transaction on
  bytdb/MySQL; `key`, `setup`, `batch`), `sql.exec` (DDL change counts
  ignored; publishes `affected`).
- `builtin_rows.go`: `cols.select`, `rows.filter` (`= != < <= > >= in like
  null notnull`, numbers as numbers), `rows.limit` (stops the source),
  `preview` (`Stats.Shown`, so it does not double a fragment's rows beside
  a load).

### Package `script`: the Go-code plugins (`plugins.go`)

`go.transform` (`Apply`, optional `Open`/`Flush`/`Close`; `Apply` may take
the `Env` first), `go.source` (`Next`), `go.action` (`Run(s *sdb.S) error`
through `RunSource`), `script.run` (a saved script by name, path or
example). A snippet without a package clause is wrapped (`WrapSnippet`):
`package main` and an import for every known standard package it names,
plus `sdb`; diag lines are shifted back. Entry points are plain funcs
looked up by name and kept for the run, called once per batch, panics
recovered. `checkSnippet` is each plugin's `Check`. The yaegi export map
gained `Batch`, `Col`, `Env`, `Cfg`, `Params`, `Pipeline`, `Fragment`,
`PipelineSpec`, `PipelineOpts`, `RunStats`, `FragmentStats`, `NodeStats`,
`Paths`, `NewPipeline`, `ParsePipeline`, `NewBatch`, `ColsOf`.

### `sdb` (`pipeline.go`)

Aliases of the above; `NewPipeline`, `ParsePipeline`, `NewBatch`,
`ColsOf`; `s.RunPipeline(p, opts)`, `s.RunPipelineSpec`,
`s.RunPipelineNamed` (a path, `PipelinesDir/<name>[.json]`, an example),
`s.LoadPipeline`; `s.WithPaths(Paths{ScriptsDir, PipelinesDir})` set by
the hosts (headless `main.go`, `workspace.RunScript`). `etlConn` became
the exported `ETLConn` (the `Host` method). `go generate ./sdb/sdbapi`
regenerated `api.json`; `hostOnly` gained `WithPaths` and `ETLConn`; the
assistant summary skips the pipeline structs' fields (keeping `Batch`,
`Col`, `Env`) and its size cap went from 12 KB to 14 KB, with the reason
in the test.

### `etl`

`CreateStmt(src, dst, table, cols, dbTypes, key)` (createDDL without the
catalog lookups; Postgres-to-Postgres keeps exact types via `pgTypeName`),
`Engine.TransactionalDDL`, `Reader.Abort` (the exported rollback).

### Store, config, examples, CLI

- `userdata/pipelines.go`: `ListPipelines` (desc and fragment count read
  off the JSON), `ReadPipeline`, `SavePipeline` (revision protocol),
  `RenamePipeline`, `TrashPipeline`/`ListPipelineTrash`/`RestorePipeline`,
  `ValidPipelineName`.
- `config/pipelines.go`: `PipelinesDir` (`pipelines_dir`, resolved like
  `scripts_dir`, default `~/.config/dbc/pipelines`), `FindPipeline`,
  `PipelineRef`.
- `scripts/pipelines/*.json` embedded: `copy-cats`, `clean-and-load` (a
  `go.transform`, a filter, two sinks, a stamp fragment reading
  `${frag.clean.rows}`), `cats-report`; `scripts.Pipelines()`,
  `PipelineByName`.
- `pipelinecmd.go`: `dbc pipelines`, `dbc pipeline run [-p k=v]…
  [--preview N] [--fragment F]`, `dbc pipeline check`, `dbc pipeline export
  [-o f.go]`, `dbc plugins`; `-t json` prints the run stats as the
  document; the log and preview results go where a script's do
  (`headlessOutput`, shared rules with `runScriptHeadless`). The cats
  completion manifest lists the new subcommands.
- The headless script path now exits 130 on `sdb.IsCanceled` (an ETL
  cancel carries `context.Canceled` and used to exit 1).
- README: a *Pipelines* section under scripting and *Pipelines headless*
  under headless mode; the dbc skill's command table and `scripting.md`.

### Tests and the benchmark

`pipeline/pipeline_test.go` (a `Host` over SQLite and bytdb files: load
into two engines with vars across fragments, failure rolls back every sink,
cancel, preview writes nothing, empty source still truncates, filter
operators, every `Check` rule, parse/JSON, batch helpers, builder and
`Gen`, plugins described); `script/plugins_test.go` (the clean-and-load
example from a script, the builder with a script's own funcs and a
`go.source`, `Gen` round trip through the interpreter, the Go-node checks
and a panic, `script.run`); `userdata/pipelines_test.go`;
`config/pipelines_test.go`. `go test ./...` green; `go vet` clean.

`go test ./script -bench Transform`: an interpreted lower-case-and-trim
over a 1000-row batch takes ~490 µs against ~32 µs compiled — about 15×,
or some two million rows a second interpreted. The README quotes it.

## Decisions and deviations from the plan's sketch

- `pipeline` does not import `sdb` (cycle): nodes see `Host`; `sdb`
  aliases the types; the Go-code plugins live in `script`.
- `Env.SourceEngine` carries the source's engine to a sink that creates a
  table, so a Postgres-to-Postgres load keeps exact column types.
- `${…}` in a SQL field is text substitution (parameters are the user's own
  values); `sql.read`'s `args` field binds values.
- `go.sink` is left for Phase 6, with `text.clean`, `lookup`, the file
  plugins and user plugin files.

## Checked in the real binary

With a temporary `HOME` and no config: `dbc pipelines`, `dbc plugins -t
json`, `dbc pipeline run cats-report`, `dbc pipeline run clean-and-load -p
min_age=5 -t json`, `dbc pipeline run copy-cats` (SQLite → bytdb, created
with a key), `--preview 3` (two previews shown, the action skipped,
nothing written), `dbc pipeline check` on all three (exit 0), `dbc
pipeline export cats-report`.

## Next

Closed: None. Declined: None. Raised: N-173, N-174.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
