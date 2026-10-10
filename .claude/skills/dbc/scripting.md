# dbc scripts — reference

Full manual: README "Scripting in Go" and "ETL across connections". The
API surface is `sdb/sdb.go` and `sdb/etl.go`; read those when this page and
the code disagree.

## Shape

```go
//go:build ignore

// Count the demo cats by breed.
package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	r, err := s.Query("demo-sqlite", "SELECT breed, count(*) AS n FROM cats GROUP BY breed")
	if err != nil {
		return err
	}
	s.Print("%d breeds", len(r.Rows))
	s.Show(r)
	return nil
}
```

```sh
dbc script s.go                 # text tables + Print lines on stdout
dbc script copy_mytable         # a script in scripts_dir, by name (.go optional)
dbc script loop_params          # else a built-in example of that name (said on stderr)
dbc scripts                     # list scripts_dir, then the examples (kind column); the dir on stderr
dbc script --check s.go         # check without running: file:line:col: msg, exit 1 on an error
dbc script --check -t json a b  # … as one JSON array of {file,line,col,severity,msg}
dbc -t json script s.go | jq .  # one JSON array of every Show; Print → stderr
dbc -t csv -o out.csv script s.go
```

`scripts_dir` defaults to `~/.config/dbc/scripts`; a relative value is
relative to the config file, never the cwd. Put a user's scripts there,
not in the repo's `scripts/` (those are the samples). The directory may not
exist yet: dbc makes it on the first save from a UI, so `mkdir -p` it when
writing a script from the shell.

Writing a script for the user:

1. `dbc scripts` — the directory (stderr) and the names already taken.
2. Write `<dir>/<name>.go`. Use letters, digits, `.`, `_` and `-`, ending
   in `.go`, with no leading dot (the UIs refuse to rename or save anything
   else). Put a comment above `package main` (before
   or after the build tag): its first sentence is the description both UIs
   and `dbc scripts` show.
3. `dbc script --check <name>` until it prints nothing. It never runs
   anything, so it is safe on a script that writes.
4. Run it only with the user's go-ahead if it writes to a real connection.

The user will see it at once in `Ctrl+O`. A dbc web tab with the script open
and unsaved edits gets a conflict on its next save rather than overwriting
your version, and the reverse holds for you: re-read the file before
editing a script the user may have open.

`//go:build ignore` only keeps `go build ./...` from compiling the script;
dbc runs it regardless. Connection names come from config /
`connections.toml` / the demos — a script cannot define its own.

## `sdb.S`

| Method | Purpose |
| --- | --- |
| `Conns() []string` | configured connection names |
| `Query(conn, sql, args...) (*sdb.Result, error)` | query with params (capped at `max_rows`) |
| `Exec(conn, sql, args...) (int64, error)` | statement → rows affected |
| `DB(conn) (*sql.DB, error)` | raw `database/sql` handle (transactions, prepared statements) |
| `Explain(conn, sql, analyze) (*sdb.Plan, error)` | plan: `p.Text(sdb.PlanText{Insights: true})`, `p.Insights`, `p.Root` |
| `Show(r)` | emit a result (TUI table, or stdout headless) |
| `Print(format, args...)` | progress log |
| `Export(r, format, path)` | `csv`/`tsv`/`markdown`/`html`/`json`/`text`; empty path → clipboard; relative → `files_dir` |
| `Path(p) string` | `p` as the script should open it: relative → `files_dir`, `~/` and absolute as written |
| `Canceled() bool`, `Ctx() context.Context` | honor Ctrl+K / Ctrl+C in long loops |
| `sdb.IsCanceled(err) bool` | tell a stop from a real failure |

`sdb.Result`: `Columns []string`, `Rows [][]string`, `Raw [][]any`,
`Duration`, `Affected`.

**Files: open every one through `s.Path`** — `os.Create(s.Path("out.csv"))`,
`os.ReadFile(s.Path("in/ids.txt"))`. A relative path is then in `files_dir`
(`config.FilesDir`, default `$HOME`; `sdb.Paths.FilesDir` on the session),
the directory a pipeline's file nodes use, from `dbc script`, the TUI, dbc
web, a schedule and dbc.app alike. A bare `os.Create("out.csv")` lands in
the process's cwd: the shell's, or wherever dbc web was started. That is
the same file only by luck. `s.Export` goes through `s.Path` itself, so a
relative export path is in `files_dir` too, not the cwd: say where
(`s.Print("wrote %s", s.Path(name))`). Inside a `go.action` or
`script.run` node, `s` is the run's session, so the same holds. `s.Export`
makes missing directories (as `csv.write` does); `s.Path` makes none, so
`os.MkdirAll(filepath.Dir(p), 0o755)` before writing where a directory may
not exist yet, `files_dir` itself included when the config names one. A
session with no Paths (`runScript` in tests) leaves a relative path in the
cwd.

Every DDL statement a script runs is logged with the `Print` lines, just
before it runs: `DDL <conn>: <statement as written>`. A failed `Query` or
`Exec` adds `DDL <conn> failed: <err>`. DDL here means a statement starting
with CREATE, ALTER, DROP, TRUNCATE, RENAME, COMMENT, GRANT or REVOKE
(`sqlsplit.IsDDL`). The log covers `Query`/`Exec` (each statement of a
multi-statement one) and the CREATE TABLE, TRUNCATE and Setup that
`Copy`/`Writer` run. Statements run through `s.DB` are not logged, and
`dbc copy` logs nothing. A script cannot turn the log off, so when you
check a script's output, expect these lines among its own.

## ETL

| Method | Purpose |
| --- | --- |
| `Copy(src, dst, table, sdb.CopyOpts{…}) (sdb.CopyStats, error)` | table or query → another connection, one transaction |
| `Reader(conn, sql, args...) (*sdb.Reader, error)` | stream rows, uncapped, typed: `for rd.Next() { rd.Row() }`; then `rd.Err()` |
| `Writer(conn, table, cols, sdb.WriteOpts{…}) (*sdb.Writer, error)` | batched load in one transaction: `Write(row)`…, `Close()` commits, `defer w.Abort()` |

`CopyOpts`: `To`, `Columns`, `Where`+`Args` or `Query`, `Create`,
`Truncate`, `Transform func([]any) ([]any, error)` (return `nil` to skip a
row), `ProgressEvery`, `Progress`, `BatchSize`. `WriteOpts`: `Setup`,
`Truncate`, `BatchSize`.

Behavior worth knowing before promising anything to the user:

- Postgres → Postgres with no `Transform`/`Args` streams `COPY` to `COPY`
  (lossless, fast). Into Postgres otherwise: `COPY FROM STDIN` text. Into
  MySQL/SQLite/bytdb: multi-row `INSERT` batches.
- A failed or stopped copy leaves the destination as it was — except that on
  bytdb and MySQL `CREATE TABLE` runs before the load, so a failure can leave
  an empty new table. MySQL `Truncate` is a `DELETE`; so is Postgres's
  within one database, which is what lets a table be copied onto itself
  with a `Transform` (an in-place rewrite) instead of deadlocking.
- `Query` may be a write with `RETURNING`: `DELETE FROM jobs WHERE done
  RETURNING *` moves rows, on either path. From Postgres the write commits
  only after the load does, so a failed copy leaves the source rows in
  place; MySQL/SQLite/bytdb commit it as it runs. A write without
  `RETURNING` is refused before it runs.
- Into Postgres, a `Transform`/`Writer` value may be a Go slice (→ array),
  map or struct (→ JSON), `time.Duration` (→ interval), pointer, or
  `driver.Valuer` (`sql.NullString`).
- A Postgres read pins `DateStyle` ISO, `IntervalStyle` postgres and exact
  floats for itself, so servers configured differently copy correctly.
- `Create` keeps exact types only Postgres → Postgres; otherwise columns get
  broad types and the source PK. No defaults, sequences, other indexes.
- bytdb needs a primary key (create the table first to copy a query into
  it) and has no exact decimal: `numeric` lands as `double precision`.
- `Reader` values: `int64`, `float64`, `bool`, `string`, `time.Time`,
  `[]byte`, `nil`; Postgres `numeric`/`uuid`/intervals/arrays/ranges as text. An unclosed
  Writer is rolled back when `Run` returns, never committed.

## The scripts browser (TUI and dbc web)

`Ctrl+O` in either UI. Sections: Scripts (by name, with description and
age; the five templates — blank, query, loop, copy, export — when the dir is
empty), Examples (the repo's samples, embedded, read-only; Enter copies
one), and Trash (`<dir>/.trash/`, newest 50 kept; Enter restores). Making a
script (new, duplicate, copy an example) asks a name, writes the file, then
opens it for editing. Enter on a script runs it.

| | TUI | dbc web |
| --- | --- | --- |
| edit | `e` → `$VISUAL`/`$EDITOR`/`vi`, checked on exit | `Shift+Enter` → a script tab |
| new · duplicate · rename | `n` · `d` · `r`/F2 | `Alt+N` · ⋯ · F2 |
| trash · copy path · filter | Del/`x` · `y` · `/` | Ctrl+Delete · ⋯ · type |

A dbc web script tab: Monaco in Go, errors marked 600 ms after typing (the
same `script.Check`), `sdb` completion and hover, explicit `Ctrl+S` save
(drafts survive a reload in localStorage), and Run saves first. It is never
connected; a script names its own connections. Each `s.Show` of a run is
kept (newest 20) in the one result tab the run lands in, behind
"Result 1 · 2 · 3" — in the TUI too (`[` / `]` step through them).

To check these in the real UIs, use the e2e suites (SKILL.md): the web
step "script tabs" (`web/e2e/scripts_test.go`) and the TUI step that drives
`e` through a stand-in `$EDITOR`.

## Pipelines (package `pipeline`, via `sdb`)

A pipeline is fragments in order; a fragment is one source → transforms →
sinks (a tree, one input per node) or one action, run in batches
(`Fragment.Batch`, default 1000) with the sinks committing together at
the end. Specs are JSON in `~/.config/dbc/pipelines/*.json`
(`config.PipelinesDir`); three examples are embedded under
`scripts/pipelines/`. From the shell: `dbc pipelines`, `dbc pipeline
run|check|export NAME` (`-p k=v`, `--preview N`, `--fragment F`, `-t json`),
`dbc plugins`.

- Plugins (`pipeline.Plugin`: name, kind, `Fields`, `New`, optional `Check`,
  `File` for a user plugin) register at init: the SQL ones in
  `pipeline/builtin_sql.go` (`sql.write` has `mode: upsert` on Postgres and
  SQLite, over `etl.WriteOptions.Upsert`: a temp table merged with
  `INSERT … ON CONFLICT` / an `ON CONFLICT` clause, last row of a key
  wins), row and column ones in `builtin_rows.go` / `builtin_cols.go`
  (`cols.cast`, `cols.add`, `text.clean`, `rows.dedupe`, `discard`), files
  in `builtin_files.go` (`csv.*`, `jsonl.*`; writers rename a temp file in
  at Commit; every path through `Env.Path`: `~/`, absolute, or relative to
  `files_dir` — `config.FilesDir`, default `$HOME`, handed over as
  `pipeline.Options.FilesDir` by the jobs engine and `sdb.Paths.FilesDir`
  — never the process's cwd; `pipeline.ResolvePath` is the rule, which
  `s.Path` and `s.Export` share), `lookup` and `pipeline.check` in `builtin_check.go`; the
  Go-code ones (`go.transform`, `go.source`, `go.sink`, `go.action`,
  `script.run`) in `script/plugins.go` because they need the interpreter.
  A `go.*` snippet without a package clause is wrapped
  (`script.WrapSnippet`): standard packages it names are imported for it.
  Entry points are plain funcs looked up by name (`goNode.entry`; a
  present one of the wrong shape is an error), bound by `bindPlugin`,
  called once per batch, panics recovered. A plugin `Check` message
  starting `warning: ` (or `code:L:C: warning: `) is a warning in
  `pipeline.Check`, anything else an error.
- **User plugins** (`script/userplugins.go`): each `.go` file in
  `plugins_dir` (`config.PluginsDir`, default `~/.config/dbc/plugins`) is
  `var Plugin = sdb.Plugin{Name, Kind, Label, Doc, Fields}` plus the kind's
  funcs (`Next`; `Apply`; `Write`; `Run(e *sdb.Env)`; optional
  `Open`/`Flush`/`Commit`/`Abort`/`Close`/`Check(cfg)`), reading its
  settings from `e.Cfg` (`pipeline.Env.Cfg`, set by the runner). The file
  is compiled once per load to read `Plugin` and bind the funcs; every
  node then compiles its own interpreter (`compilePluginSource` in `New`),
  so globals are per node and no interpreter is shared across goroutines.
  `script.SyncPlugins(dir)` reloads only when the dir's stamp (names,
  sizes, mtimes) changed and swaps the whole user set
  (`pipeline.SetUserPlugins`, which refuses a built-in's or another file's
  name); `pipeline.PluginProblems()` lists files that did not load (a node
  naming one gets "did not load from FILE: why"). `script.CheckPlugin`
  is the editor's check (AST shape by kind + lints + compile, never runs
  the file); `script.LoadPluginFile` loads one file alone (`dbc plugins
  --check`). Hosts: the CLI loads in `loadPlugins` (pipeline/job run and
  check, scripts, `dbc plugins`); dbc web at `New`, after its own saves and
  every 2 s (`pluginWatch`, stopped by `Shutdown`), broadcasting
  `plugins`; the TUI at start and where it checks or runs (`m.syncPlugins`).
  Examples (one per kind) are embedded from `scripts/plugins/`
  (`scripts.PluginExamples`). dbc web serves the files at
  `/api/v1/plugin-files[/:name]`, `plugin-check`, `plugin-examples`,
  `plugin-trash` (`web/plugins.go`) and edits one in a script tab named
  `plugin:<file>` (`scripts.js` `isPlug`), its store events sent as
  `scripts` under that name; the palette's *Yours* section draws
  `p.file` and the problems. The TUI lists them in the `Ctrl+O` browser
  (`rowPlugin`, `Enter` edits, `pluginLoaded` reports). The assistant gets
  `pipeline.Summary()` beside `sdbapi.Summary()`. e2e: web "plugin files
  and the palette" (`web/e2e/plugins_test.go`), TUI "Ctrl+O plugins: …"
  (`tui/e2e/plugins_test.go`).
- `pipeline.Host` is what a node sees as `e.S`; `*sdb.S` satisfies it, so
  `sdb` aliases the types (`sdb.Batch`, `sdb.Env`, `sdb.Cfg`,
  `sdb.NewPipeline`, `s.RunPipeline`, `s.RunPipelineNamed`,
  `s.RunPipelineSpec`). Package `pipeline` must not import `sdb`.
- The builder (`pipeline.Builder`, `sdb.Pipeline`) adds `Func`/`ThenFunc`
  nodes a script's own Go; `pipeline.Gen` writes a spec as such a script
  (`dbc pipeline export`); a spec with a func node cannot be exported.
- `pipeline.Check` is the validation (`Diag{Where, Severity, Msg}`): names,
  plugin fields, `${…}` references (a param, `frag.<earlier>.<key>`,
  `run.*`), connection names when given, and the fragment's shape.
- The direct shape — `sql.read`/`sql.table` on Postgres straight into
  `sql.write` on Postgres — runs as `etl.Copy` and reports `Direct`.
- After changing `sdb` or `pipeline` exports — or the doc comment of a
  field scripts see (`pipeline.Options` is `sdb.PipelineOpts`): add the
  symbol to `script/engine.go`'s export map and run `go generate
  ./sdb/sdbapi` (`TestAPIUpToDate` fails on a stale `api.json`).
- `Options.Progress` reports each fragment's start, every batch and its end
  (final status, skipped included): a host draws states from it alone.
- Runs outside a script go through package `jobs` (`jobs.Engine`): one per
  process, several runs at once, a `Run` record per run (fragments, node
  counters, the log, `Origin` and `Source` for the host), events
  (`RunStarted`, `State`, `Progress` coalesced to 250 ms, `Logged`,
  `Preview`, `Notice`, `RunDone`). dbc web's `Server.jobs` is one; its pipeline tab
  (`web/pipelines.go`, `web/static/js/pipelines.js`) previews and runs
  through it, and the preview rows land in the asking tab's grid via
  `workspace.ShowResult`. In the browser: the web e2e step "pipeline tabs"
  (`web/e2e/pipelines_test.go`; `DBC_E2E_SHOTS=dir` keeps its screenshots).
- **Jobs** (`jobs/spec.go`: `Spec` with `Pipelines []Step{ID, Pipeline,
  After, Params}`, `Triggers{Schedule, TZ, CatchUp, Webhook}`,
  `Policy{OnFailure, MaxParallel, Overlap, Timeout}`) live in
  `~/.config/dbc/jobs/*.json` (`config.JobsDir`); one example,
  `scripts/jobs/nightly.json`. `jobs.CheckJob` checks the DAG (Kahn: one
  root, no cycle, all reachable), each step against its pipeline
  (`CheckOptions.Find`, normally `jobs.PipelineFinder(dir)`: the dir, then
  the examples), params and `${…}` (job params, `run.*` =
  `pipeline.RunVars`), cron lines and policy. `Engine.StartJob` runs the
  readiness walk (`jobs/job.go`); `${run.date}` etc. come from
  `pipeline.Options.Run`. One real run of a pipeline at a time
  engine-wide (`Engine.pipes`), one run of a job unless `overlap: queue`.
- **Cron** (`jobs/cron.go`): five fields, Vixie's either-day rule, wall
  clock in the job's `tz`; a skipped DST time fires at the jump, a repeated
  one once. `Schedule.Next`/`NextN`. The scheduler (`jobs/scheduler.go`)
  runs only in dbc web (`Server.sched`), only for `jobs_dir` (never an
  example), rescans every minute and on `Reload`; a fire later than a
  minute is a miss unless `catch_up`; a fire is claimed with an O_EXCL
  file in `runs_dir/.fires`, so two dbc web processes run it once.
  `Scheduler.Step` drives it in tests with a fake clock.
- **Records** (`jobs/records.go`, `userdata/runs.go`): one JSON per run in
  `runs_dir/<kind>/<name>/<id>.json`, written at start, every
  `FlushEvery` (2s) and at the end; a `running` record not rewritten for
  `StaleAfter` (30s) is `interrupted` (`settle` on read, `Recover`
  rewrites; hosts call it at start); `runs_keep` prunes. Previews are never
  recorded. `Engine.History(filter)`, `Get` falls back to disk,
  `Run.Tree()` is the text drilldown.
- **s.RunJob** (`sdb/jobs.go`): runs on the session's `WithJobs` runner
  (dbc web hands `Engine.ScriptRunner()` to every workspace), else
  `sdb.DefaultJobRunner`, which package jobs sets to a private engine.
  `sdb` cannot import `jobs`; that is why it is a hook.
- dbc web: `web/jobs.go` (store, `job-check` with each cron line's next five
  fires, `POST /api/v1/jobs/:name/run` = manual with the cookie, webhook
  with the Bearer secret and only for `triggers.webhook`),
  `GET /api/v1/runs?kind=&name=&status=&since=&limit=` adds the history;
  window events `jobs`, `job.state`, `job.notice`. `job-check` also returns
  the job's **layout** (`jobs.Spec.Layout`, over package `dag`, which
  `erd/layout.go` uses for ranking, ordering and placing too); so does
  `GET /api/v1/jobs/:name/layout`, and `GET /api/v1/runs/:id` for a job
  (`jobs.Run.Layout`, as it ran). `POST /api/v1/runs/:id/cancel` answers
  409 for a run another process runs (`jobs.ErrElsewhere`).
- The page: **job tabs** (`web/static/js/jobs.js`, `t.job`, `#jobp`, saved
  tabs' `job` column) — the DAG canvas (the server's layout; palette of
  pipelines; wire output ● to a card to add an after), the inspector (cron
  lines with their next fires, webhook, policy, step params), ▶ Run landing
  on the tab's Runs face; the **Runs view** (`web/static/js/runs.js`,
  `Alt+R`, ◷ Runs): the list, and a run page drilling DAG → fragments on
  the run's time axis → node counters → the filtered log, live from the
  `job.*` events (another process's run is re-read every 2 s). The web e2e
  step "job tabs and the runs view" (`web/e2e/jobs_test.go`) drives both.
- `dbc run cancel ID [--url U] [--secret S]` (`$DBC_WEB_URL`,
  `$DBC_WEB_SECRET`) stops a run of a running dbc web through that API.
- The plan, with each phase's outcome (all six are in), is
  `ai_docs/plans/pipelines.md`.

## yaegi pitfalls

- **Two-value assignment into a map element stores nothing.**
  `m[k], _ = v.(string)`, `m[k], _ = other[k]`, `m[k], _ = f()` silently
  drop the write; `m[k], _ = <-ch` panics (traefik/yaegi#1655).
  `dbc script --check` warns on each one. Assign to a local first:

  ```go
  v, _ := x.(string)
  m[k] = v
  ```

- **An operator's result stored straight into a row is mis-stored.**
  `b.Rows[i][c] = s + "!"`, `b.Rows[i][c] = n * 2`, `row[c] = -n` (a
  `[]any` handed in by compiled code: a batch's rows, a result's `Raw`)
  writes into the wrong frame slot — the row keeps its value and a local,
  often the batch, is overwritten, so a transform's `return b, nil`
  returns nil and its rows vanish. Comparisons, calls
  (`strings.ToLower(s)`), variables and `any(…)` are fine. The checks warn
  (`lintStoreComputed`), the runner logs a transform's first dropped
  batch, and `TestYaegiStoreComputedBug` pins it:

  ```go
  v := s + "!"
  b.Rows[i][c] = v
  ```

- A script is interpreted: keep it to stdlib + `sdb`. Third-party imports
  are not available.
- **yaegi's compile pass is not the Go compiler.** `--check` catches
  undefined names, type mismatches and bad imports, but an unused variable
  or import passes, and only the first compile error is reported.
