# Pipelines, Phase 6: the plugin SDK and the rest of the built-ins

Session: `d07aa754-5bce-4c19-8051-6ab84678434c`

## Ask

1. `/sl` — load the last session (`2026-1009-1449-phase-5-tui-pipelines-jobs`)
   and the next-list.
2. "Do phase 6" — Phase 6 of `ai_docs/plans/pipelines.md` (N-174): the
   plugin SDK (user plugin files, `Check` per kind, `dbc plugins`, the
   palette's *Yours*, the assistant summary), the rest of the built-ins
   (`csv.*`, `jsonl.*`, `cols.cast`, `cols.add`, `text.clean`, `lookup`,
   `rows.dedupe`, `discard`, `pipeline.check`, `sql.write mode: upsert`),
   the docs and an embedded example plugin.
3. `/sw phase-6-plugin-sdk-builtins`.

## How it was done

Two background forks, each in its own git worktree, took the self-contained
slices while the plugin SDK was built in the main checkout: one wrote the
eleven new built-ins (`pipeline/builtin_files.go`, `builtin_cols.go`,
`builtin_check.go`, `builtin_more_test.go`), the other `sql.write`'s upsert
(`etl/writer.go`, `pipeline/builtin_sql.go`, `etl/upsert_test.go`,
`pipeline/upsert_test.go`, with a live Postgres 17 run in a throwaway
container, removed after). Both were reviewed, copied over, and their
worktrees and branches removed.

## What was built

### The registry and the runner (`pipeline`)

- `Plugin.File` (a user plugin's path; "" for a built-in);
  `SetUserPlugins(ps, probs)` swaps the whole user set under one lock and
  refuses a built-in's or another file's name; `PluginProblems()` lists
  files that did not load; `nodeKind` says "plugin X did not load from
  FILE: why" instead of "no plugin".
- `Env.Cfg`: the node's settings after `${…}` and defaults, set by the
  runner — how a plugin file's funcs read their fields.
- `pipeline.Check`: a plugin `Check` message marked `warning:` (at the start
  or after its place, `code:3:1: warning: …`) is a warning, not an error.
- The runner logs a transform's first dropped batch per node ("Apply
  returned no batch for N rows …"), unless the node asked the source to stop.
- `pipeline.Summary()`: the registry as text for the assistant (kind, name,
  first sentence, fields; yours marked; the plugin file's shape).

### The built-ins (forks, reviewed)

`csv.read`, `jsonl.read` (columns known at Open, so a header-only file still
creates a table), `csv.write`, `jsonl.write` (temp file renamed in at Commit,
fsync, 0644), `cols.cast`, `cols.add`, `text.clean`, `rows.dedupe`,
`discard` (`Shown`, so its count joins the fragment's only when nothing
wrote), `lookup` (side query read once, `max_rows`, keys as text),
`pipeline.check` (a gate; publishes `value`). Keys are length-prefixed
(`keyOf`); messages carry their details in the text (`serr.F`).
`sql.write mode: upsert`: Postgres COPYs into `"_dbc_upsert"` (`ON COMMIT
DROP`) and merges with `INSERT … SELECT DISTINCT ON (key) … ORDER BY key,
ctid DESC ON CONFLICT … DO UPDATE` (last row of a key wins); SQLite gets an
`ON CONFLICT` clause; MySQL and bytdb refuse; the direct COPY path is off.

### User plugins (`script/userplugins.go`)

- A file is `var Plugin = sdb.Plugin{Name, Kind, Label, Doc, Fields}` plus
  the kind's funcs (source `Next`, transform `Apply`, sink `Write`, action
  `Run(e)`; optional `Open`/`Flush`/`Commit`/`Abort`/`Close`/`Check(cfg)`).
  Doc defaults to the first paragraph of the comment above `package`.
- Loaded once per load (descriptor read, funcs bound by `bindPlugin`, which
  the `go.*` plugins now share); **each node compiles its own
  interpreter** in `New`, so globals are per node and no interpreter is
  shared by goroutines.
- `SyncPlugins(dir)` reloads only when the dir's stamp changed;
  `LoadPlugins`, `LoadPluginFile` (one file, for `--check`);
  `CheckPlugin` (AST: package main, `var Plugin`, the kind from the
  literal, each func's signature via `funcSig`; the lints; a compile).
- `go.sink` (left from Phase 1); `goSource` takes `Close(ok bool)`; a
  mistyped optional func of a `go.*` node is now an error, not ignored.
- `sdb`: `Plugin`, `Field`, `Kind`, `FieldType`, `Stats` and the
  `Kind*`/`Field*` constants, exported to the interpreter; `api.json`
  regenerated (doc first sentences shortened to keep the summary under
  14 KB).

### Hosts

- Config `plugins_dir` (`config/plugins.go`), `userdata/plugins.go` (the
  scripts store under plugin names), four example plugins in
  `scripts/plugins/` (`mask_email`, `gen_series`, `webhook_post`,
  `wait_file`; `scripts.PluginExamples`).
- CLI: `loadPlugins` before pipeline/job run and check and a headless
  script; `dbc plugins` adds a `from` column; `dbc plugins --check [FILE…]`.
- dbc web (`web/plugins.go`): `/api/v1/plugin-files[/:name]`, rename,
  trash/restore, examples, `plugin-check`; load at `New`, after each
  change, and every 2 s (`pluginWatch`, ended by `Shutdown` via a context
  made in `New` — test servers outlive their tests); a `plugins` event; the
  store's changes sent as `scripts` events under `plugin:<file>`, which is
  also the script tab's name (`api.go` accepts it). `GET /api/v1/plugins`
  adds `problems`. The page: the scripts browser's Plugins section,
  examples and trash, + New ▾ → plugin from an example; plugin tabs (◈,
  check endpoint, Ctrl+S logs the load, Run = save + check + load); the
  palette's *Yours* first, broken files ⚠ not draggable, the inspector's
  "did not load"; a registry refetch on `plugins`.
- TUI: plugins loaded in `initJobs` (problems in the startup log) and
  synced where used (`m.syncPlugins`: the browsers, `checkSpec`, starts);
  the `Ctrl+O` browser's Plugins section (`rowPlugin`, `Enter` edits,
  `pluginLoaded` reports on return, ⚠ mark), examples, trash, new from an
  example; F1 row.
- The assistant: `sdbapi.Summary() + pipeline.Summary()` with a script
  question, in both UIs.

### Docs

README: the plugin table (every built-in), the interpreter quirk, the
`text.clean` throughput (~37 µs per 1000 rows vs ~490 µs interpreted), a new
section *Plugins of your own*, `plugins_dir` in the config block,
`dbc plugins --check`. `dbc.example.toml`. The dbc skill (`scripting.md`:
the built-ins, user plugins, the yaegi pitfall; `SKILL.md`: the command).
The plan's Phase 6 outcome; the header says all six phases are done.

## Decisions and deviations

- **The loader is in `script`**, not `pipeline.LoadPlugins`: it needs the
  interpreter.
- **One interpreter per node**, not one compile per process (globals per
  node; no shared interpreter across goroutines).
- **Settings via `e.Cfg`**, not a `cfg` parameter: the funcs keep the
  `go.*` signatures.
- **Plugin files edit in script tabs** (`plugin:<file>`) and the scripts
  browser in both UIs, rather than a new kind of tab or the Ctrl+J browser.
- **The TUI syncs at use points**, not on a timer; dbc web watches every 2 s.
- **Upsert's count stays rows written**, as for an insert.

## Found and fixed on the way

- **yaegi mis-stores an operator's result put straight into a row**
  (`b.Rows[i][c] = s + "!"`, `-n`, `n * 2`; also via `row := b.Rows[i]`):
  the element keeps its value and the batch variable is overwritten, so
  `Apply` returns nil and the rows silently vanish. Found when the
  `mask.email` example dropped every batch; bisected to that shape
  (calls, comparisons, variables, `any(…)` and slices the code made are
  fine). `lintStoreComputed` warns in every check, the runner logs a
  transform's first drop, `TestYaegiStoreComputedBug` pins it, the README
  and the skill say the workaround (N-188 to report it upstream).
- Plugin `Check` warnings were errors in `pipeline.Check`, so a lint
  warning blocked a run.
- A `go.*` node's optional func with the wrong signature was silently
  never called.
- A stopped dbc web server (tests) would have kept reloading the
  process-wide registry from its own dir.
- e2e: the full suite's crowded tab strip made a tab click by title miss;
  the step reaches tabs through the browser's ✎.
- An earlier test-only yaegi quirk: a conversion inside a closure returning
  `any` (`func(int) any { return int64(seen) }`) panicked in `reflect.Set`;
  the test plugin was written without it.

## Checked

`gofmt` clean, `go vet ./...` clean, `go test ./...` green; `-race` on
`script`, `pipeline`, `web`, `tui`, `etl`, `userdata`, `config`, `jobs`.
The live Postgres upsert test (fork, postgres:17). In the real binary:
`dbc plugins` (yours with their file, a broken file on stderr),
`dbc plugins --check` (path:line:col, exit 1), and a headless pipeline of
`gen.series → cols.add → mask.email → csv.write + sql.write (upsert)`, then
`csv.read → cols.cast → rows.filter → preview`, then `pipeline.check`, run
twice (the upsert kept 10 rows). The web e2e suite passed whole with the new
step "plugin files and the palette" (copy an example, a plugin tab, a
broken save's marker and ⚠ in Yours, fixed, loaded, dragged onto a lane, the
browser row); the TUI e2e suite passed with "Ctrl+O plugins: …" (an example
copied, the stand-in editor, the check's `path:line:col`, did not load,
loaded, a pipeline placing it from Ctrl+J, `dbc plugins` from another
process).

## Next

Closed: N-174. Declined: None. Raised: N-188, N-189, N-190, N-191.
Deferred: None. Promoted: None.
Updated: N-064, N-115. Full list: `ai_docs/todo/next-list.md`.
