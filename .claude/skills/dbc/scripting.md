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
| `Export(r, format, path)` | `csv`/`tsv`/`markdown`/`html`/`json`/`text`; empty path → clipboard |
| `Canceled() bool`, `Ctx() context.Context` | honor Ctrl+K / Ctrl+C in long loops |
| `sdb.IsCanceled(err) bool` | tell a stop from a real failure |

`sdb.Result`: `Columns []string`, `Rows [][]string`, `Raw [][]any`,
`Duration`, `Affected`.

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

- Plugins (`pipeline.Plugin`: name, kind, `Fields`, `New`, optional `Check`)
  register at init: the SQL and row ones in `pipeline/builtin_*.go`, the
  Go-code ones (`go.transform`, `go.source`, `go.action`, `script.run`) in
  `script/plugins.go` because they need the interpreter. A `go.*` snippet
  without a package clause is wrapped (`script.WrapSnippet`): standard
  packages it names are imported for it. Entry points are plain funcs
  looked up by name, called once per batch, panics recovered.
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
  (`RunStarted`, `Progress` coalesced to 250 ms, `Logged`, `Preview`,
  `RunDone`). dbc web's `Server.jobs` is one; its pipeline tab
  (`web/pipelines.go`, `web/static/js/pipelines.js`) previews and runs
  through it, and the preview rows land in the asking tab's grid via
  `workspace.ShowResult`. In the browser: the web e2e step "pipeline tabs"
  (`web/e2e/pipelines_test.go`; `DBC_E2E_SHOTS=dir` keeps its screenshots).
- The plan for the rest (jobs, scheduler, monitoring, TUI, plugin files)
  is `ai_docs/plans/pipelines.md`.

## yaegi pitfalls

- **Two-value assignment into a map element stores nothing.**
  `m[k], _ = v.(string)`, `m[k], _ = other[k]`, `m[k], _ = f()` silently
  drop the write; `m[k], _ = <-ch` panics (traefik/yaegi#1655).
  `dbc script --check` warns on each one. Assign to a local first:

  ```go
  v, _ := x.(string)
  m[k] = v
  ```

- A script is interpreted: keep it to stdlib + `sdb`. Third-party imports
  are not available.
- **yaegi's compile pass is not the Go compiler.** `--check` catches
  undefined names, type mismatches and bad imports, but an unused variable
  or import passes, and only the first compile error is reported.
