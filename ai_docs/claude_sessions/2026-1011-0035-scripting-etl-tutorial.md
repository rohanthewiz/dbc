# A scripting and ETL tutorial, verified end to end

Session: `d97d65d1-486f-4a95-9637-7969d431dd26`

## Ask

"Create a tutorial on how to use the scripting and ETL features of dbc."
Then: add each problem found along the way as an open Next-list item, and
wrap the session.

## What was made

- **`docs/scripting-and-etl-tutorial.md`** (new; the repo had no `docs/`
  before). A guided tour that builds one small flow, an SQLite "shop"
  loaded into a bytdb "warehouse", one layer at a time:
  0. a self-contained `dbc.toml` workspace (every `*_dir` beside it,
     `files_dir = "data"`) and a deliberately messy seed;
  1. a first script (`s.Query`, `Print` vs `Show`, `-t json`, `dbc scripts`);
  2. reports and files (`s.Export`, `s.Path`, `files_dir`, cancellation,
     knobs as constants);
  3. `dbc script --check` as a CI gate, showing the yaegi map-assign warning;
  4. `dbc copy`, including a failed re-run that leaves the destination as it
     was;
  5. script ETL: `Copy` + `Transform`, `Reader`/`Writer` for a join across
     two connections, the DDL log, how rows move and what is guaranteed;
  6. pipelines: a four-fragment `shop-sync` (`text.clean`, `go.transform`,
     a `pipeline.check` gate, `lookup`, `cols.add`, a dated `csv.write`,
     `${frag.*}` values into an audit `sql.exec`); check, preview,
     `--fragment`, `-p`, `-t json`, `export`;
  7. a user plugin (`order.tier` transform) used from a pipeline;
  8. a job: a fan-out DAG whose root wraps the §5 script via `script.run`;
     cron, policy, crontab, `--wait=false`, `run cancel`;
  9. orchestration from a script (`RunPipelineNamed`, the builder with
     `ThenFunc`, `RunJob`);
  10. run records (`dbc runs`, `run show`, `runs --sql`);
  11. where each piece lives in the TUI and dbc web; a pitfalls table.
- **README**: a pointer to the tutorial at the top of "Scripting in Go".

## How it was verified

- A fresh build in the scratchpad, `CATS_*` stripped, `HOME` isolated, run
  from a scratch workspace holding the tutorial's own `dbc.toml`. Every
  command, script, pipeline, plugin and job in the doc was run there, and
  the outputs in the doc are those runs' (paths shown as `~/dbc-tutorial`).
- Repeated runs were checked too (the script twice, then the job), which is
  what exposed the `${run.id}` key problem below.

## Decisions

- **A repo file, not a shared doc.** A tutorial for the tool ships and
  versions with it, so it went in `docs/`. It could also be published as a
  shareable page if wanted.
- **SQLite + bytdb, not Postgres.** Both are embedded, so the tutorial runs
  anywhere with no server. The Postgres differences (placeholders, direct
  COPY, exact types with `Create`) are called out where they matter.
- **Script knobs are constants.** A first draft read `MIN_TOTAL` from the
  environment and silently ignored it: the interpreter's environment is
  empty (N-203). The doc says to use constants, a settings file through
  `s.Path`, or pipeline params.
- **The audit table is keyed by `bigserial`.** It was first keyed on
  `${run.id}`, then on `${run.started}`. The first is empty under a script's
  `s.RunPipelineNamed` (N-206). The second has one-second resolution, so
  two runs in the same second collided.

## Problems found (all raised as Open items)

- N-203: `os.Getenv` is `""` in scripts (`interp.Options{}` in
  `script/engine.go`).
- N-204: a skipped fragment's time prints as `2562047h47m16.85s`.
- N-205: a whole-pipeline preview fails on a fresh database, with an
  unhelpful error, when a later fragment reads what an earlier one creates.
- N-206: `${run.id}` is empty under `s.RunPipeline*`.
- N-207: SQLite reports a stale rows-affected for DDL in headless runs.
- N-208: `-t json script` shows `duration "0s"`, and a preview's
  rows-in counts look wrong.

## Files

- `docs/scripting-and-etl-tutorial.md` (new)
- `README.md`
- `ai_docs/todo/next-list.md`

## Next

Closed: None. Declined: None. Raised: N-203, N-204, N-205, N-206, N-207,
N-208. Deferred: None. Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
