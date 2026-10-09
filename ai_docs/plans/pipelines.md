# Pipelines: plugins, fragments, pipelines and jobs — the scripting layer's next level

Raised 2026-10-09. The ask (from the author's cats-todo backlog): take dbc's
scripting layer to the next level — a UI-first system that keeps Go scripting
at the core; plugins of several kinds (sources, transformers, sinks);
drag-and-drop connections; batch processing inside a *fragment* (a PGReader,
a data-manipulation plugin, a PGWriter); fragments in sequence making a
*pipeline*; pipelines in a dependency DAG making a *job*, whose root pipeline
is triggered on a schedule, by hand, or from outside; fan-out and fan-in of
pipelines inside a job; and a monitoring view for jobs, pipelines and
fragments, with drilldown.

This is a plan. The decisions table was accepted as recommended
(2026-10-09). **Phases 1 to 5 are done** (2026-10-09): see their
outcomes under *Phases*. The decisions table is the part to read first; everything after
it follows the recommended column.

## The one-paragraph version

dbc already has the inside of a fragment: `sdb.Reader → Transform → Writer`
streams typed rows between any two connections in one transaction, with a
Postgres-to-Postgres COPY fast path (package `etl`). What it lacks is
everything around that: a way to *name* a step, *compose* steps without
writing the glue, *run* the composition outside a query tab's single run
slot, *schedule* it, *remember* what happened, and *watch* it. The plan adds
one core package, `pipeline` — a batch type, three node interfaces (source,
transform, sink) plus a rowless *action*, a plugin registry with
self-describing configuration, a JSON spec, and runners for a fragment and a
pipeline — and one package `jobs` — a DAG of pipelines, a scheduler with a
small in-house cron parser, an engine that runs jobs concurrently outside any
workspace, and run records on disk. Go stays at the core three ways: the
built-in plugins are compiled Go; custom logic is Go code in a node (inline)
or in a plugin file (`~/.config/dbc/plugins/*.go`), interpreted by the same
yaegi engine scripts use, called **once per batch** rather than per row; and
every pipeline the UI can draw is also expressible, and exportable, as an
ordinary dbc script through a builder API on `sdb.S`. The UI is the lead:
`dbc web` gets a pipeline tab (palette, canvas with drag-and-drop ports,
inspector generated from each plugin's declared fields, preview into the
grid) and a jobs tab (DAG canvas, triggers, a Runs view that drills from job
to pipeline to fragment to node, live over SSE). The TUI gets the same model
without the drawing: browse, run, cancel, watch a run as a tree, and edit the
JSON in `$EDITOR`. Headless, `dbc pipeline run`, `dbc job run` and `dbc runs`
make all of it usable from cron and CI, and `dbc web` is the long-lived
process that fires schedules.

## Vocabulary

```
  Job  ─────────────────────────────────────────────────────────────────┐
  │   a DAG of pipelines; one root; triggered by schedule / hand / HTTP │
  │                                                                     │
  │   ┌──────────┐                                                      │
  │   │ extract  │  root pipeline                                       │
  │   └────┬─────┘                                                      │
  │   ┌────┴──────────┐                    fan-out: both start when     │
  │   ▼               ▼                    extract succeeds             │
  │ ┌───────────┐  ┌──────────┐                                         │
  │ │ customers │  │ products │                                         │
  │ └────┬──────┘  └────┬─────┘                                         │
  │      └──────┬───────┘                  fan-in: starts when all      │
  │             ▼                          upstreams succeeded          │
  │        ┌─────────┐                                                  │
  │        │ report  │                                                  │
  │        └─────────┘                                                  │
  └─────────────────────────────────────────────────────────────────────┘

  Pipeline ──────────────────────────────────────────────────────────────┐
  │   fragments, in order; the next starts when the previous has         │
  │   committed; rows pass between fragments only through the database  │
  │                                                                      │
  │   fragment "orders"                       fragment "merge"           │
  │   ┌─────────┐  ┌─────────┐  ┌─────────┐   ┌───────────────────┐      │
  │   │sql.read │─►│go.trans │─►│sql.write│   │ sql.exec          │      │
  │   │prod-pg  │  │(Go)     │  │warehouse│   │ INSERT … SELECT … │      │
  │   └─────────┘  └─────────┘  └─────────┘   └───────────────────┘      │
  │    source       transform    sink           action (no rows)        │
  │                                                                      │
  │   inside a fragment rows move in BATCHES (default 1000): the source  │
  │   yields one, each transform reshapes it, each sink loads it; the   │
  │   sinks commit together at the end, so a fragment is all-or-nothing │
  │   per sink                                                           │
  └──────────────────────────────────────────────────────────────────────┘
```

| Word | Means | Lives in |
|---|---|---|
| **Plugin** | A kind of node: `sql.read`, `go.transform`, `sql.write`, … Declares its kind, its config fields and how to build a node | `pipeline` registry; built-ins compiled in, user plugins in `~/.config/dbc/plugins/*.go` |
| **Node** | One plugin instance with its config, in a fragment | the pipeline spec |
| **Fragment** | One source, zero or more transforms, one or more sinks, wired by edges; or a single action. The unit of batch processing and of atomicity | the pipeline spec |
| **Pipeline** | Fragments in order, with parameters | `~/.config/dbc/pipelines/<name>.json` |
| **Job** | A DAG of pipelines with triggers and a failure policy | `~/.config/dbc/jobs/<name>.json` |
| **Run** | One execution of a job or a pipeline, with per-level state and counters | memory while running; `~/.config/dbc/runs/…/<id>.json` after |

## What exists today, and carries over

- **The inside of a fragment is done.** `etl.Reader` streams typed rows
  (`int64, float64, bool, string, time.Time, []byte, nil`; `etl/reader.go`),
  `etl.Writer` loads one table in one transaction, COPY on Postgres and
  batched INSERT elsewhere (`etl/writer.go`), and `etl.Copy` composes the two
  with a direct COPY-to-COPY path when both ends are Postgres and nothing
  touches the rows (`etl/copy.go:146`). Create-with-types, Truncate inside
  the transaction, lock timeouts, pinned output settings, cancellation that
  still rolls back — all of it is the fragment's runtime.
- **Go scripting is done.** yaegi runs `func Run(s *sdb.S) error` with the
  stdlib and the `sdb` exports (`script/engine.go:22-45`); `script.Check`
  compiles without running; `sdb/sdbapi/api.json` feeds Monaco completion
  and the assistant; the scripts browser, script tabs and `dbc script` exist
  in both UIs and headless. An interpreted closure handed to compiled code as
  a `func([]any) ([]any, error)` works (`script/engine_test.go:51-134`) —
  the mechanism a Go node needs.
- **The host plumbing is done.** `workspace.Start{Job}` plus `Options.Sink`
  carry mid-run events to both UIs; the web hub has one SSE stream per
  window with window-level broadcasts (`web/hub.go`, `conns.go:433`);
  `web/dump.go` is a worked example of a server-wide, cancellable background
  task that streams lines to every window and answers a GET so a reloaded
  page can catch up.
- **Graph drawing has a seed.** `explain/assets/plan.js` is a pan-and-zoom
  stage with HTML cards and SVG Bezier edges (read-only, tree-only); and
  `erd/layout.go` has a deterministic Sugiyama-lite layout (longest-path
  ranks, barycenter ordering) that a job DAG can reuse once it is
  generalized away from `erd.Table`.
- **Nothing exists for** scheduling, run history, a job registry, plugins,
  or a batch-level API. `scripts-revamp.md`'s *Later* lists scheduling,
  script parameters and recorded runs as wanted.

## Goals

- **UI-first, Go at the core.** Anything drawn in the browser is a JSON file
  a script can run, a builder call a script can make, and a command cron can
  invoke. The UI never gains a capability the Go API lacks.
- **Plugins of four kinds** — source, transform, sink, action — with a
  self-describing config so the UI, the CLI and `Check` all derive from one
  declaration. Built-ins in compiled Go; custom ones as Go, interpreted.
- **Batches inside a fragment**, with the existing transaction guarantees
  kept: a fragment's sink commits once, at the end, or not at all.
- **Fragments in sequence** make a pipeline; **pipelines in a DAG** make a
  job, with one root, fan-out, fan-in, and a failure policy.
- **Triggers**: by hand (both UIs, CLI), on a cron schedule (while `dbc web`
  runs), and from outside (an authenticated HTTP endpoint; the CLI).
- **Monitoring with drilldown**: runs → job DAG → pipeline's fragments →
  fragment's nodes, with live counters and the log, in the browser; the same
  as a tree in the TUI; as JSON headless.
- **Parity in kind, not in pixels**: the TUI browses, runs, watches and
  edits-in-`$EDITOR`; drawing is a browser thing.

## Non-goals

- **A distributed or multi-user scheduler.** One process, one machine, one
  user — `dbc web`'s model. No leader election, no remote workers.
- **In-memory hand-off between fragments.** A fragment's output is what its
  sinks committed; the next fragment reads it back. This is what makes a
  fragment restartable and a pipeline inspectable from any SQL console.
- **Two-phase commit across sinks.** A fragment with two sinks commits them
  one after the other; the gap is documented, not closed.
- **Sandboxing plugins.** A Go plugin runs with the process's rights, as a
  script does today. The plugins directory is the user's, like scripts.
- **Compiled plugins** (`-buildmode=plugin`, WASM): fragile or dependency
  heavy. Interpreted Go plus compiled built-ins covers the ground; see
  *Later*.
- **Joins as two-input nodes.** A streaming join needs memory or spill;
  `lookup` (a side query cached per fragment) covers the common case.
- **A JavaScript framework.** The canvas is vanilla JS on the plan.js
  pattern, under the existing CSP (no inline script, nothing external).

## Decisions to take to the user

The recommendation is first in each row; the rest of the plan assumes it.

| Fork | Recommended | Alternatives |
|---|---|---|
| Source of truth for a pipeline | **A JSON spec file** (`pipelines/<name>.json`); Go appears as inline node code, plugin files, the `sdb` builder, and *Export as Go* | A `.go` script as the only truth, with the UI editing it through the AST (round-tripping hand-written Go is where visual editors die) |
| Spec format | **JSON** — the browser edits it natively, Monaco has a JSON mode with schema validation, nested node configs read well | TOML (nicer by hand, worse for nested node graphs and for the browser) |
| Where runs execute | **A server-wide `jobs.Engine`** owned by the process (web `Server`, TUI `Model`, headless `main`), not a workspace's run slot; several jobs run at once, a tab is never "busy" because a job is | Through `workspace.RunScript` (gets cancel/history/grid for free, but one run per tab and the tab is blocked) |
| Who fires schedules | **`dbc web`** (it is the long-lived process; `--no-open` makes it a daemon). External cron calls `dbc job run NAME`. The TUI does not schedule | A separate `dbc jobs serve` daemon (a second long-lived process to manage; could be added later as an alias of `dbc web --no-open --no-ui`) |
| Run history storage | **One JSON file per run** under `~/.config/dbc/runs/<kind>/<name>/<id>.json`, written atomically, re-written every 2 s while running; an index is rebuilt by scanning | A `runs.bytdb` (the global rule prefers bytdb, but bytdb's file lock is exclusive per process: a cron-driven `dbc job run` could not record while `dbc web` holds it, and the TUI could not read while either runs. See *Storage: why files* below) |
| Rows between fragments | **Only through the database.** Small scalars pass as variables (`${frag.orders.rows}`) | In-memory hand-off (fast, but a pipeline then cannot restart at fragment 3, and memory is unbounded) |
| Execution inside a fragment | **A synchronous batch loop** in v1: source yields a batch, the tree of transforms and sinks consumes it. Node interfaces are designed so a goroutine-per-node driver can replace the loop later without changing a plugin | Goroutine per node with bounded channels from day one (more throughput for heavy transforms; harder to reason about errors, cancellation and interpreter thread safety, which is unverified for yaegi) |
| Batch size | **1000 rows** default, per fragment; a `sql.read(pg) → sql.write(pg)` fragment with no transforms collapses to `etl.Copy`'s direct COPY and reports it | Per-node sizes (over-configurable) |
| Fan-in semantics | **All upstreams succeeded** (AND). A failed upstream skips everything downstream; the job fails after the other branches finish (`on_failure: finish_branches`), or at once (`stop`) | OR / any-of (needs a merge policy; nobody asked) |
| Overlapping runs of one job | **Skip** the new one and log why; `overlap: queue` as an option | Run concurrently (two nightly loads into the same table) |
| Cron parsing | **An in-house 5-field parser** (`jobs/cron.go`, ~200 lines with tests), in the house style of `sqlsplit` and `sqlcomplete` | `github.com/robfig/cron/v3` (mature; one more dependency and a `@every` dialect nobody needs) |
| Plugin authoring | **Compiled Go for built-ins; interpreted Go files for user plugins; inline Go for one-off nodes.** Plugin entry points are plain functions, not interface implementations, so they cross the interpreter boundary without wrapper types | Only inline Go (no reuse); only files (ceremony for a 3-line map) |
| Where the TUI edits a spec | **`$EDITOR` on the JSON, `Check` on return** (as scripts do) | A TUI form editor (large; the browser is the editor) |
| DAG layout | **A small `dag` layout package extracted from `erd/layout.go`'s ranking and ordering**, used by `erd` and by the jobs canvas (the server returns positions; the client draws) | A client-side layered layout in JS (a second implementation of the same algorithm) |

### Storage: why files, and where bytdb fits

The author's standing rule prefers bytdb for persistence. It is the right
call for `web.bytdb` because exactly one process owns that state. Run
history has at least three writers and readers that overlap in time: `dbc
web` firing a schedule, a cron-driven `dbc job run`, and a TUI listing runs.
bytdb's exclusive sidecar lock (`web/store.go:30-37`) makes any shared file
the property of whichever process opened it first, and the others would
fall back to memory and silently record nothing. One JSON file per run has
no such contention (each run writes its own file; a listing is a readdir),
survives a crash as a partial record, and is `cat`-able and `jq`-able from
cron. The same argument the scripts plan made for `.go` files applies to
pipeline and job specs: they must be runnable headless and diffable in git.

bytdb still has a place in this feature, on the read side: `dbc runs --sql`
(Phase 3) loads the run files into an in-memory bytdb and runs a query over
them, so "which fragment is slowest this month" is one SQL statement, and a
`dbc-runs` demo-style connection can expose the same tables to a query tab.
If the author would rather have `runs.bytdb` anyway, the engine can be made
the single writer (headless runs post to a running `dbc web` over HTTP and
run locally only when none is up), at the cost of that fallback logic.

## Design

### 1. Package `pipeline`: batches, nodes, plugins, spec, runners

**Batch.** The unit that moves between nodes.

```go
// Batch is a slice of rows sharing one column list. Values are what
// etl.Reader yields (int64, float64, bool, string, time.Time, []byte, nil);
// a transform may put in anything etl.Writer accepts for the destination
// engine (Postgres takes slices, maps and Valuers too; the others do not —
// see the asymmetry note in etl/pgtext.go).
type Batch struct {
	Cols []Col
	Rows [][]any
}
type Col struct {
	Name   string
	DBType string // the source driver's type name, upper case; "" when unknown
}
func (b *Batch) Len() int
func (b *Batch) Col(name string) int                 // -1 when absent
func (b *Batch) Get(row int, name string) any
func (b *Batch) Set(row int, name string, v any)
func (b *Batch) AddCol(name, dbType string, fill func(row int) any)
func (b *Batch) Drop(names ...string)
func (b *Batch) Keep(names ...string)                // select + reorder
func (b *Batch) Filter(keep func(row int) bool)
func (b *Batch) Clone() *Batch                       // for fan-out: each consumer gets its own rows
```

**Node interfaces.** Four kinds; a plugin builds one of them.

```go
type Env struct {
	Ctx     context.Context
	S       *sdb.S                 // connections, Print, Explain — the script API
	Params  map[string]string      // pipeline params after substitution
	Vars    map[string]string      // ${frag.<name>.<key>} values from earlier fragments
	Batch   int                    // the fragment's batch size
	Logf    func(string, ...any)   // a line tagged with run/pipeline/fragment/node
}

type Source interface {
	Open(env *Env) error
	Next(env *Env) (*Batch, error) // nil, nil at the end
	Close(ok bool) error           // ok=false: the fragment failed; a Postgres read rolls back
}
type Transform interface {
	Open(env *Env) error
	Apply(env *Env, b *Batch) (*Batch, error) // may mutate and return b; nil, nil drops the batch
	Flush(env *Env) (*Batch, error)           // last words at the end (aggregations); usually nil
	Close() error
}
type Sink interface {
	Open(env *Env, cols []Col) error // called lazily on the first batch, so transforms can shape the columns
	Write(env *Env, b *Batch) error
	Commit(env *Env) (Stats, error)  // commit, or nothing is visible
	Abort() error                    // idempotent; a no-op after Commit
}
type Action interface { // a rowless fragment: a statement, a script, a check
	Run(env *Env) (Stats, error)
}
```

`Stats{Rows, Skipped int64; Direct bool; Note string; Vars map[string]string}`
is what a sink or an action reports; `Vars` become `${frag.<name>.<key>}`
for later fragments (a `sql.write` publishes `rows`; a `sql.exec` publishes
`affected` and, with RETURNING of one row, each column).

**Plugins and the registry.** A plugin is data plus a constructor. The
`Fields` drive the inspector form in the web, the `--set k=v` flags
headless, `Check`'s validation, and the docs page — one declaration, four
consumers.

```go
type Plugin struct {
	Name   string   // "sql.read"
	Kind   Kind     // source | transform | sink | action
	Label  string   // "SQL query"
	Doc    string   // one paragraph
	Fields []Field
	New    func(cfg Config) (any, error) // returns a Source, Transform, Sink or Action
}
type Field struct {
	Name     string
	Type     FieldType // string | text | int | bool | duration | enum | conn | table | columns | sql | go
	Doc      string
	Default  string
	Required bool
	Enum     []string
	Lang     string    // for sql/go fields: the Monaco mode; "go" fields also get a Check
}
func Register(p Plugin)               // built-ins at init; user plugins at load
func Plugins() []Plugin               // for the palette, `dbc plugins`, the assistant
func Lookup(name string) (Plugin, bool)
```

`Field.Type` is what lets the UI be *generated* rather than written per
plugin: `conn` renders the connection picker, `table` the catalog's table
picker for the chosen connection, `columns` a multi-select from the
upstream node's columns (known after a preview), `sql` and `go` a Monaco
mini-editor with the right mode.

**Built-in plugins** (Phase 1 ships the bold ones; Phase 6 the rest).

| Name | Kind | Does |
|---|---|---|
| **`sql.read`** | source | A query on a connection, through `etl.Read`; `$1`/`?` args from params. This *is* the PGReader when the connection is Postgres |
| **`sql.table`** | source | A whole table with optional `where`, `columns`, `order` — `etl.copySource` as a node |
| `csv.read`, `jsonl.read` | source | A file; header, delimiter, type sniffing off by default (everything is text unless `cast` follows) |
| **`go.source`** | source | Inline Go: `func Next(e *sdb.Env) (*sdb.Batch, error)` — generate rows, call an API with `net/http`, read anything the stdlib can |
| **`cols.select`** | transform | Keep, drop, rename, reorder |
| **`rows.filter`** | transform | Column `op` value rules (`=`, `!=`, `<`, `>`, `in`, `like`, `null`, `notnull`) AND-ed; for anything else, `go.transform` |
| **`go.transform`** | transform | Inline Go: `func Apply(b *sdb.Batch) (*sdb.Batch, error)`, with optional `Open`/`Flush`. The one escape hatch that covers map, filter, derive, aggregate |
| `cols.cast` | transform | Column → int/float/bool/time/text with a layout; errors or nulls on failure (policy field) |
| `cols.add` | transform | A constant, a param, `now()`, a uuid, the run id |
| `text.clean` | transform | trim / lower / upper / collapse spaces on chosen columns |
| `lookup` | transform | Join against a query on another connection: key columns → appended columns, the side table cached per fragment (the `copy_table.go` sample as a node) |
| `rows.dedupe` | transform | By key columns, first wins, within the run (a set in memory; documented) |
| `rows.limit` | transform | First N rows, then stop the source early (used by Preview) |
| **`sql.write`** | sink | `etl.NewWriter`: table, `create`, `truncate`, `setup` statements, batch size; later `mode: insert|upsert` (Postgres and SQLite `ON CONFLICT`) |
| `csv.write`, `jsonl.write` | sink | Streaming file writers (new: `export` renders whole results in memory) |
| **`preview`** | sink | Keeps the first N rows and `Show`s them as a result: the grid is the debugger |
| `discard` | sink | Counts rows; for measuring a source or transform |
| **`sql.exec`** | action | One or more statements on a connection; publishes `affected` |
| **`script.run`** | action | An existing dbc script by name — `func Run(s *sdb.S) error`, unchanged — so every script written so far is a valid fragment |
| `go.action` | action | Inline `func Run(s *sdb.S) error` |
| `pipeline.check` | action | Assert a query returns a value satisfying a rule (row count ≥ N, no nulls): a gate between fragments |

**Go nodes and the interpreter.** Each `go.*` node compiles once per run
(`newInterp` plus `Eval`, as `script.run` does), looks up its entry points
by name (`main.Apply`, `main.Open`, `main.Flush`) and keeps the extracted
func values for every batch — the per-call reflection cost is paid per
batch, not per row. Calls are wrapped in `recover`, as `script.run` does.
The only compile-time check yaegi gives is the first error, so
`pipeline.Check` runs `script.Check`'s passes with a per-kind signature
table instead of the hard-coded `Run(*sdb.S) error`. Interpreted globals
hold per-node state (a lookup map, a counter). One interpreter is never
called from two goroutines: the v1 loop is single-threaded per fragment,
and the parallel driver in *Later* must keep a node on one goroutine.

A yaegi benchmark is a Phase 1 deliverable: a `go.transform` that
lower-cases one column over 1M SQLite rows, against the same in
`text.clean`. The number goes in the README so users know when to reach
for a built-in. (Expect roughly 10–50× slower than compiled for the
interpreted part; the batch boundary is what keeps the total tolerable.)

**The spec** (`pipeline.Spec`, JSON; `$schema` served by the web for Monaco).

```json
{
  "name": "orders-nightly",
  "desc": "Yesterday's orders from prod into the warehouse staging table, then merged",
  "params": { "days": { "default": "1", "doc": "How many days back" } },
  "fragments": [
    {
      "name": "orders", "batch": 1000,
      "nodes": [
        { "id": "src",   "plugin": "sql.read",     "cfg": { "conn": "prod-pg",
            "query": "SELECT id, customer_email, total, created_at FROM orders WHERE created_at >= now() - ${days} * interval '1 day'" } },
        { "id": "clean", "plugin": "go.transform", "cfg": { "code": "func Apply(b *sdb.Batch) (*sdb.Batch, error) {\n\tc := b.Col(\"customer_email\")\n\tfor i := range b.Rows {\n\t\tif s, ok := b.Rows[i][c].(string); ok { b.Rows[i][c] = strings.ToLower(strings.TrimSpace(s)) }\n\t}\n\treturn b, nil\n}" } },
        { "id": "dst",   "plugin": "sql.write",    "cfg": { "conn": "warehouse", "table": "stg.orders", "create": "true", "truncate": "true" } },
        { "id": "peek",  "plugin": "preview",      "cfg": { "rows": "50" } }
      ],
      "edges": [ ["src", "clean"], ["clean", "dst"], ["clean", "peek"] ],
      "ui": { "src": [40, 80], "clean": [260, 80], "dst": [480, 40], "peek": [480, 140] }
    },
    {
      "name": "merge",
      "nodes": [ { "id": "up", "plugin": "sql.exec", "cfg": { "conn": "warehouse",
          "sql": "INSERT INTO orders SELECT * FROM stg.orders ON CONFLICT (id) DO UPDATE SET total = EXCLUDED.total" } } ]
    }
  ]
}
```

Rules `Check` enforces (`pipeline.Check(spec) []Diag`, stateless, fast,
the `script-check` pattern): names valid (the script-name rule); a fragment
is either one action or a tree rooted at exactly one source (every node has
at most one incoming edge, sources none, sinks no outgoing, no cycles);
every node's plugin exists and its required fields are set; `conn` fields
name a configured connection; `${…}` references resolve to a param or an
earlier fragment's var; `go` fields pass `script.Check` with the kind's
signature; `sql` fields split into the expected number of statements. `ui`
positions are the canvas's and never affect a run.

**The fragment runner** — the heart of the batch model.

```
run(fragment):
  src.Open
  for every sink:  nothing yet — sinks open lazily on their first batch
  loop:
    b := src.Next                    ← one batch, ≤ fragment.batch rows
    if b == nil: break
    push(srcNode, b)
  for every transform, in topological order: fb := t.Flush; if fb != nil: push(t, fb)
  for every sink, in node order:   st := sink.Commit      ← all-or-nothing per sink
  src.Close(ok)                    ← last: a Postgres source's transaction commits after the loads,
                                     so DELETE … RETURNING stays a safe move (etl.Copy's order)
  on any error: every sink.Abort, src.Close(false); the error names node, batch number and row

push(node, b):
  children := edges from node
  for i, child := range children:
    cb := b; if i < len(children)-1: cb = b.Clone()     ← fan-out: no shared mutation
    switch child:
      transform: out := child.Apply(cb); if out != nil: push(child, out)
      sink:      child.Open(cols) once; child.Write(cb)
    counters[child].in += len(cb.Rows); .out += len(out.Rows); .batches++; .elapsed += …
```

The runner recognises the **direct shape** — `sql.read` or `sql.table` on
Postgres → `sql.write` on Postgres, no transforms, no args — and runs
`etl.Copy` instead, reporting `Direct: true` in the fragment's stats, the
same way `CopyStats` does. Progress is reported per batch through the
events (§4), replacing `etl`'s exact-multiple `ProgressEvery`.

**The pipeline runner.** Fragments in order; each one's `Stats.Vars` go
into `Env.Vars` for the next; `${param}` and `${frag.x.key}` are
substituted into every string field just before a node is built, with
`${…}` in `sql` fields turned into bind parameters where the plugin
supports it (`sql.read` args), and refused in positions that would be
injection (table names) unless the value matches an identifier. A fragment
that fails stops the pipeline (`on_error: stop`; `continue` per fragment is
an option for best-effort steps). Parameters come from the spec defaults,
then the job's node params, then `--param k=v` or the run dialog.

**Preview** (`RunPreview(spec, fragment, n)`): the same runner with a
`rows.limit` node spliced after the source and every sink replaced by a
`preview` — so a fragment can be tried against a real connection without
writing anything, and the grid shows what the last transform produced.
This is the UI's inner loop.

**Exposure to scripts** (`sdb`): the spec and the builder.

```go
// A script can run a saved pipeline …
st, err := s.RunPipeline("orders-nightly", sdb.Params{"days": "7"})
// … or build one. The builder is the spec with a Go face; Go funcs may stand
// in for go.* nodes, which is something the JSON cannot say.
p := sdb.NewPipeline("adhoc").Param("days", "1")
f := p.Fragment("orders").Batch(2000)
src := f.Node("sql.read", sdb.Cfg{"conn": "prod-pg", "query": "SELECT …"})
clean := f.Transform(func(b *sdb.Batch) (*sdb.Batch, error) { …; return b, nil })
dst := f.Node("sql.write", sdb.Cfg{"conn": "warehouse", "table": "stg.orders", "truncate": "true"})
f.Wire(src, clean, dst)
p.Fragment("merge").Action("sql.exec", sdb.Cfg{"conn": "warehouse", "sql": "INSERT …"})
st, err = s.Run(p)
```

`pipeline.Gen(spec) string` writes the builder form of a spec as a script
(*Export as Go* in the web, `dbc pipeline export NAME` headless): the proof
that the UI draws nothing a script cannot do. New exported types (`Batch`,
`Col`, `Env`, `Pipeline`, `Fragment`, `Cfg`, `Params`, `RunStats`) go into
`script/engine.go`'s export map, and `go generate ./sdb/sdbapi` refreshes
`api.json` so completion and the assistant know them.

### 2. Package `jobs`: the DAG, the engine, the scheduler, run records

**The spec** (`jobs.Spec`, `~/.config/dbc/jobs/<name>.json`).

```json
{
  "name": "nightly",
  "desc": "Load, then sync the dimensions in parallel, then report",
  "root": "extract",
  "pipelines": [
    { "id": "extract",   "pipeline": "orders-nightly" },
    { "id": "customers", "pipeline": "customers-sync", "after": ["extract"] },
    { "id": "products",  "pipeline": "products-sync",  "after": ["extract"] },
    { "id": "report",    "pipeline": "daily-report",   "after": ["customers", "products"],
                         "params": { "day": "${run.date}" } }
  ],
  "triggers": { "manual": true, "schedule": ["0 2 * * *"], "webhook": true },
  "policy": { "on_failure": "finish_branches", "max_parallel": 3, "overlap": "skip", "timeout": "2h" }
}
```

`Check`: the graph is acyclic (Kahn), exactly one node has no `after` and
it is `root`, every node is reachable from the root, every `pipeline`
exists and its `Check` passes, every cron expression parses, `max_parallel`
≥ 1. The run-level variables `${run.id}`, `${run.date}`, `${run.started}`
are available to every node's params.

**Execution** (`jobs.Engine`, one per process):

```go
type Engine struct { mgr *db.Manager; cfg *config.Config; store Store; sched *scheduler; … }
func New(mgr, cfg, store, Options{Sink func(Event)}) *Engine
func (e *Engine) StartJob(name string, t Trigger, params map[string]string) (runID string, err error)
func (e *Engine) StartPipeline(name string, params map[string]string) (runID string, err error)
func (e *Engine) Cancel(runID string) error
func (e *Engine) Running() []RunView                 // live
func (e *Engine) Runs(f Filter) ([]RunSummary, error) // live + on disk
func (e *Engine) Run(id string) (*Run, error)         // the full record, live or on disk
func (e *Engine) Reload() error                      // specs and plugins changed on disk
func (e *Engine) Close(grace time.Duration)          // cancel everything, write records
```

A job run is a readiness walk: every pipeline node has a counter of
unfinished upstreams; the root starts at once; when a pipeline *succeeds*
each downstream's counter drops and those at zero start, up to
`max_parallel` at a time (an `errgroup` with a limit — `golang.org/x/sync`
is already a dependency). When a pipeline *fails* or is *canceled*, every
transitive downstream is marked `skipped`; with `finish_branches` the
branches that do not depend on it run to completion and the job ends
`failed`; with `stop` the run context is canceled and the job ends `failed`
now. `timeout` cancels the run context. Each pipeline run gets its own
`sdb.S` over the shared `db.Manager`, with `LogDDL` on and `Release` at the
end, exactly as `script.run` does — so a crashed node never leaves a
Writer's transaction open.

States, at every level (job, pipeline, fragment, node):
`queued → running → succeeded | failed | canceled | skipped`, plus
`interrupted` for a record found `running` when the process starts again.

**Connection pressure** is the one resource fan-out contends for: a running
fragment holds a Reader and a Writer connection (and a third on the direct
path). `max_parallel` is the knob; the default is 2. The in-memory SQLite
demo pool is capped at 3 connections, so a fan-out of three fragments on
`demo-sqlite` would block — the engine raises that pool's cap for the run
(`db.Manager.SetMemoryPool`) and says so in the log.

**Triggers.**
- *Manual*: both UIs and `dbc job run NAME [--param k=v]`.
- *Schedule*: `jobs/cron.go` parses standard five-field expressions
  (`min hour dom mon dow`, with `*`, lists, ranges, `*/n`, month and day
  names) into a `Schedule` with `Next(after time.Time) time.Time`. The
  scheduler goroutine keeps the next fire time per job, sleeps until the
  earliest, fires, recomputes. Missed fires while the process was down are
  not replayed (`catch_up: false`; `true` runs the most recent missed one
  on startup). The local time zone, with `"tz"` on the trigger for another.
- *Webhook*: `POST /api/v1/jobs/:name/run` with the existing bearer secret
  (`web/auth.go:84-90`), body `{params}` → `{run}`; `GET /api/v1/runs/:id`
  for the result. Off-loopback listening already warns; nothing new.
- *From a script*: `s.RunJob("nightly", params)` — a job can be a step in a
  script, which closes the loop with the existing scripting layer.

**Run records** (`jobs.Run`): the record the monitoring view reads.

```go
type Run struct {
	ID        string    // 20261009-020000-7f3a: sorts by time, unique enough per machine
	Kind      string    // job | pipeline
	Name      string
	Trigger   Trigger   // manual | schedule | webhook | script | cli, with who/what
	Params    map[string]string
	Started, Ended time.Time
	Status    State
	Error     string
	Pipelines []PipelineRun // for a job; one entry for a bare pipeline run
	Log       []Line        // capped at 2000 lines per run; the rest is in the UI log only
}
type PipelineRun struct { ID, Name string; Status State; Started, Ended time.Time; Error string; Fragments []FragmentRun }
type FragmentRun struct { Name string; Status State; Started, Ended time.Time; Error string; Direct bool; Nodes []NodeRun; Vars map[string]string }
type NodeRun struct { ID, Plugin string; In, Out, Batches int64; Elapsed time.Duration; Error string }
```

Written by `userdata.SaveRun` (atomic temp-and-rename) at start, every 2 s
while running (so a crash leaves a partial record), and at the end; a run
found `running` at startup is rewritten `interrupted`. `userdata.ListRuns`
scans `runs/<kind>/<name>/` and returns summaries (one `os.ReadDir` plus the
file headers; a 10 KB record × 200 runs × a few jobs is nothing).
`runs_keep` (config, default 200 per name) prunes the oldest after each run.

### 3. Persistence and the file layout

```
~/.config/dbc/
  pipelines/<name>.json      the specs; .trash/ as scripts have
  jobs/<name>.json
  plugins/<name>.go          user plugins (Phase 6)
  runs/job/<name>/<id>.json  run records
  runs/pipeline/<name>/<id>.json
```

`userdata/pipelines.go`, `jobs.go`, `plugins.go`, `runs.go` follow
`userdata/scripts.go` exactly: `List`, `Read` (text + rev), `Save` with
base-rev conflict detection, `Rename`, `Trash`/`Restore`, name validation,
`writeAtomic`. Config gains `pipelines_dir`, `jobs_dir`, `plugins_dir`,
`runs_dir` (all defaulting beside `scripts_dir`, resolved the same way) and
`runs_keep`. Every change to a spec broadcasts a `pipelines` or `jobs`
window event, as `scripts` does, so an open browser relists.

Examples ship in the binary as read-only, like the script examples: three
pipelines (`copy-table`, `clean-and-load` with a Go transform, `report`)
and one job (`nightly`, the diagram above), all on the demo connections, so
the whole feature can be tried in a fresh install with nothing configured.

### 4. Events and the hosts

The engine's `Sink` receives `jobs.Event`s; each host turns them into its
own messages, as `workspace` events are handled today.

| Event | Carries | Frequency |
|---|---|---|
| `RunStarted` | the `Run` header | once |
| `StateChanged` | run id, path (`pipeline/fragment/node`), old → new state, error | per transition |
| `Progress` | run id, fragment, per-node counters, batches/s | coalesced to one per 250 ms per run (the `tick` pattern) |
| `Line` | run id, path, level, text | as logged; `Env.Logf` and `S.Print` both land here |
| `Preview` | run id, fragment, a `*model.Result` | per preview sink at the end |
| `RunDone` | the final `Run` | once |

- **Web**: `Server` owns the `Engine` (as it owns `dumps`), stops it in
  `Shutdown`, broadcasts every event window-wide as `job.run`,
  `job.state`, `job.progress`, `job.line`, `job.preview`, `job.done`
  (new names, so they cannot be confused with the per-tab `run` event).
  `GET /api/v1/runs?live=1` is what a reloaded page uses to catch up
  (`dump.go`'s GET pattern).
- **TUI**: `Model` owns an `Engine` for the session; events arrive through
  `m.send` as a `jobMsg`; lines go to the connection's log pane with a
  `[nightly › orders]` prefix; a running job shows in the status bar.
- **Headless**: lines to stderr (or stdout for `text`), the final `Run` to
  stdout as `-t json`, exit 0/1/130; `--wait=false` returns the run id as
  soon as it has started (for a webhook-less external trigger that polls
  `dbc run show ID`).

One fix on the way: `runScriptHeadless` exits 130 only on `db.ErrCanceled`
(`main.go:1017`); an ETL cancel carries `context.Canceled`. Both exit 130
once `sdb.IsCanceled` is used there, and the jobs CLI uses the same.

### 5. Headless CLI

```
dbc pipelines                              list (name, desc, modified, kind: pipeline|example)
dbc pipeline run NAME [--param k=v]… [--preview N] [--fragment F] [-t json]
dbc pipeline check NAME…                   diags like `dbc script --check`; exit 1 on an error
dbc pipeline export NAME [-o file.go]      the Go script equivalent
dbc jobs                                   list (name, triggers, next fire, last run status)
dbc job run NAME [--param k=v]… [--wait=false] [-t json]
dbc job check NAME…
dbc runs [--job N | --pipeline N] [--status failed] [--since 7d] [-t json]
dbc runs --sql "SELECT …"                  the records as tables in an in-memory bytdb (Phase 3)
dbc run show ID [-t json]                  the drilldown as text: job → pipelines → fragments → nodes
dbc run cancel ID                          against a running `dbc web` (Phase 4, via its API)
dbc plugins                                built-ins and user plugins, with their fields
```

Names resolve as scripts do: a path, then the directory, then an example.

### 6. `dbc web`: API and UI

**Routes** (literal segments kept apart from `:name` params — the
radix-router rule, N-133):

| Route | |
|---|---|
| `GET/PUT/DELETE /api/v1/pipelines[/:name]`, `POST …/:name/rename`, `POST /api/v1/pipeline-trash/:id/restore` | the scripts protocol: `{text, rev}`, `{text, base}` → `{rev}` or `{conflict…}` |
| `POST /api/v1/pipeline-check` `{text}` → `{diags}` | unsaved text, on typing pauses |
| `POST /api/v1/pipeline-preview` `{text, fragment, rows}` → `{run}` | runs the preview; the result arrives as `job.preview` |
| `POST /api/v1/pipeline-run` `{name, params}` → `{run}` | |
| `GET /api/v1/pipeline-export/:name` | the Go script |
| `GET/PUT/DELETE /api/v1/jobs[/:name]`, `POST /api/v1/job-check`, `POST /api/v1/jobs/:name/run` | the last doubles as the webhook |
| `GET /api/v1/jobs/:name/layout` → `{nodes: {id: [x, y]}}` | the server-side DAG layout (§7) |
| `GET /api/v1/runs?…`, `GET /api/v1/runs/:id`, `POST /api/v1/runs/:id/cancel` | |
| `GET /api/v1/plugins` | the registry as JSON: the palette, the inspector, completion |
| `GET /api/v1/pipeline-schema` | the JSON schema for Monaco's JSON mode |

**The pipeline tab** — a third tab kind beside query and script
(`t.pipeline`, a `Tab.Pipeline` column, `.pipeline-mode`), opened from the
scripts browser, which gains *Pipelines* and *Jobs* sections and a *New
pipeline* template menu.

```
┌ palette ──┐┌ canvas ────────────────────────────────────────────┐┌ inspector ───────────┐
│ ⌕ filter  ││ ▸ orders  · batch 1000 · [Preview 50] [▶ Run]      ││ sql.write  "dst"     │
│ Sources   ││  ┌────────┐   ┌──────────┐   ┌────────────┐        ││ conn    [warehouse ▾]│
│  sql.read ││  │sql.read│●─►●go.transf.│●┬►●sql.write   │        ││ table   [stg.orders ]│
│  sql.table││  │prod-pg │   │clean     │ │ │warehouse   │        ││ create  [x]          │
│  csv.read ││  └────────┘   └──────────┘ │ └────────────┘        ││ truncate[x]          │
│  go.source││                            └►●preview 50 │        ││ setup   [ sql … ]    │
│ Transforms││ ▸ merge                                            ││ batch   [    1000 ]  │
│  …        ││  ┌─────────────────────────┐                       ││                      │
│ Sinks     ││  │sql.exec  INSERT INTO …  │                       ││ ⚠ table does not     │
│  …        ││  └─────────────────────────┘                       ││   exist; create is on│
│ Actions   ││ [+ fragment]                                       ││                      │
└───────────┘└────────────────────────────────────────────────────┘└──────────────────────┘
┌ results / preview grid ───────────────────────┐┌ run: orders ● running  12,000 rows 3.1k/s ┐
│ (the existing grid: the preview sink's rows)  ││ src 12,000 → clean 12,000 → dst 12,000     │
└───────────────────────────────────────────────┘└────────────────────────────────────────────┘
```

- **Canvas**: fragments are stacked lanes (a fragment = a lane; drag the
  lane handle to reorder); nodes are cards with an input port on the left
  and an output port on the right; drag a palette entry onto a lane to add
  a node; drag from an output port to an input port to connect (the rule
  "one input per node" is enforced on drop; a sink has no output port);
  click an edge to delete it; `Delete` removes a node; `Ctrl+D` duplicates;
  positions are saved in `ui`. Pan with the wheel/drag, zoom with
  Ctrl+wheel — `plan.js`'s stage, factored into a shared `stage.js`. All
  pointer events (not HTML5 DnD, whose ghost image and cross-browser
  quirks the splitters already avoid).
- **Inspector**: generated from `Fields`; `conn` → the connection picker,
  `table` → the catalog picker, `columns` → a multi-select fed by the last
  preview, `sql`/`go` → a Monaco mini-editor with the right mode and the
  check markers; edits update the spec and schedule a `pipeline-check`;
  node badges show diags.
- **Preview** runs the selected fragment with N rows and shows the preview
  sink's rows in the grid below (a `go.transform` can be iterated on in
  seconds with real data and nothing written).
- **JSON view** toggle: Monaco in JSON mode with the schema; edits
  round-trip to the canvas. **Export as Go** opens the generated script in
  a script tab.
- **Run panel**: the fragment's node counters inline, the log filtered to
  the run, Stop; a finished run links to its record in the Runs view.

**The jobs tab**: a DAG canvas of pipeline cards (drag a pipeline from the
list onto the canvas; drag card-to-card to add a dependency; the layout
comes from the server and is re-fetched after a change; the root is marked);
a triggers panel (manual, cron lines with the next five fire times shown as
you type, the webhook URL with a copy button); the policy fields; Run.

**Monitoring — the Runs view** (a topbar button and `Alt+R`; also the
landing view of a job tab after a run starts):

```
Runs                                   [job ▾] [status ▾] [since 7d ▾]      ● 1 running
nightly    schedule  02:00:00  3m 12s  ● running   extract ✓ customers ● products ● report ○
nightly    schedule  yesterday 4m 01s  ✓ succeeded 1.2M rows
nightly    manual    Oct 7     0m 40s  ✗ failed    products: sql.write: duplicate key …
orders-… pipeline  Oct 7     0m 12s  ✓ succeeded  (preview)
```

Click a run → the **run page**: the job DAG, cards coloured by state with
duration and rows, the critical path emphasised; click a pipeline → its
fragments as a timeline (bars on one time axis, so waits and overlaps are
visible), each with rows and a Direct badge; click a fragment → its nodes
with in/out rows, batches, rows/s, elapsed, and the error pinned to the
node that raised it, with the batch number and row; the log pane filtered
to that path; *Open preview* where a preview sink ran. Live runs update
over the `job.*` events; a reload catches up through `GET /api/v1/runs`.
The TUI's `dbc run show ID` prints the same drilldown as an indented tree.

### 7. Layout for the DAG

`erd/layout.go`'s ranking (longest path, back edges cut), barycenter
ordering (8 sweeps) and column placement are the generic part; the box
sizing and port rows are ERD-specific. Extract the generic part into
`dag/` (`Layout(nodes, edges, sizes) positions`), make `erd` call it, and
give the jobs tab `GET /api/v1/jobs/:name/layout`. Edge routing stays in
`erd` (a job DAG of a dozen cards does not need it; Bezier curves between
ports, as `plan.js` draws, are enough).

### 8. The TUI

- A *Pipelines & jobs* browser (`Ctrl+J`, to be checked against
  `tui/help.go` for a free chord; also in the menu), the scripts browser's
  twin: pipelines, jobs, examples, trash; `Enter` runs (a params prompt
  when the spec has params without defaults), `e` edits the JSON in
  `$EDITOR` and checks on return, `n` new from a template, `d` trash,
  `r` runs list.
- A *Runs* modal: the Runs table above as a list; `Enter` opens the run
  monitor.
- The *run monitor* modal: the tree `job → pipelines → fragments → nodes`
  with state glyphs (`○ ● ✓ ✗ ↷`), counters and elapsed, refreshed from
  events; `Ctrl+K` cancels the selected run; lines go to the log pane.
- Status bar: `● nightly 3m12s` while a job runs in this process.

### 9. Plugins from the user (`~/.config/dbc/plugins/*.go`)

A plugin file is a `package main` with a descriptor and plain functions —
no interfaces to implement, so nothing needs yaegi wrapper types:

```go
//go:build ignore

// Mask the e-mail column: keep the domain, hash the local part.
package main

import "github.com/rohanthewiz/dbc/sdb"

var Plugin = sdb.Plugin{
	Name: "mask.email", Kind: sdb.KindTransform, Label: "Mask e-mail",
	Fields: []sdb.Field{{Name: "column", Type: sdb.FieldColumns, Required: true}},
}

func Apply(e *sdb.Env, cfg sdb.Cfg, b *sdb.Batch) (*sdb.Batch, error) { … }
```

The loader (`pipeline.LoadPlugins(dir)`) compiles each file once per
process, reads `main.Plugin`, looks up the entry points the kind requires
(`Open`/`Next`/`Close`; `Open`/`Apply`/`Flush`/`Close`; `Open`/`Write`/
`Commit`/`Abort`; `Run`), wraps them in the host's interface types and
registers them beside the built-ins. `Check` validates a plugin file by
kind; a broken plugin is listed with its error and cannot be placed. The
browser lists them in the palette under *Yours*; `dbc plugins` lists them
with their fields; the scripts browser's editor opens them. The assistant
gets the registry's summary beside `sdbapi.Summary()`, so "add a node that
…" questions have the vocabulary.

### 10. Security

Nothing new in kind. Plugins and inline Go run with the process's rights,
as scripts do. The webhook uses the launch secret the API already requires;
its body is params only, never code. `${…}` substitution binds values as
parameters where the engine can, and refuses non-identifiers where it must
splice (table names). The specs directory follows the scripts directory's
modes (0700 dir, 0600 files).

## Phases

Each phase ends green (`go test ./...`), is checked in the real binary, and
gets a session doc. The UI arrives in Phase 2 — before jobs — because the
ask is UI-first, and because the editor is what tells us whether the
plugin descriptors are rich enough.

### Phase 1 — `pipeline` core, headless

- `pipeline/`: `Batch`, `Col`, `Env`, the four interfaces, `Plugin`,
  `Field`, the registry, `Spec` + JSON (un)marshalling, `Check`, the
  fragment runner (with the direct-COPY collapse), the pipeline runner,
  params and vars, preview, `Gen`.
- `pipeline/plugins/`: `sql.read`, `sql.table`, `sql.write`, `sql.exec`,
  `cols.select`, `rows.filter`, `rows.limit`, `go.transform`, `go.source`,
  `go.action`, `preview`, `script.run`.
- `sdb`: `Batch` et al. re-exported; `RunPipeline`, `Run(p)`, the builder;
  `script/engine.go` exports; `go generate ./sdb/sdbapi`.
- `userdata/pipelines.go`; config `pipelines_dir`.
- CLI: `dbc pipelines`, `dbc pipeline run|check|export`; the 130 exit fix.
- Three example pipelines, embedded.
- Tests: the runner on SQLite and bytdb (fan-out to two sinks, a failing
  transform rolls back every sink, cancel mid-batch, vars between
  fragments, params → bind args, preview writes nothing); `Check`'s rules
  one by one; `Gen` output runs through `script.RunSource` with the same
  result as the spec; a live Postgres test proves the direct collapse; the
  yaegi benchmark.
- Outcome: `dbc pipeline run clean-and-load` works from a shell with the
  demos, and a script can do the same with the builder.

  ✅ **Done 2026-10-09.** What landed, and where it differs from the sketch:
  - Package `pipeline` does not import `sdb`: nodes see a `Host` interface
    (`e.S`), which `*sdb.S` satisfies, and `sdb` aliases the types. The
    Go-code plugins live in `script/plugins.go` (they need the
    interpreter). `Env.SourceEngine` carries the source's engine to a sink
    creating a table, so Postgres-to-Postgres keeps exact column types.
  - Entry points of `go.*` nodes are plain funcs looked up by name; a
    snippet without a package clause is wrapped with imports for the
    standard packages it names (`script.WrapSnippet`), so a three-line
    `Apply` needs no boilerplate. `go.sink` is left for Phase 6.
  - A preview sink's rows do not count toward a fragment's rows beside a
    writing sink (`Stats.Shown`); `sql.exec` ignores DDL change counts.
  - A sink no rows reached, with nothing but the source above it, is still
    opened and committed, so a reload from an empty source still truncates.
  - Benchmark (`go test ./script -bench Transform`): an interpreted
    lower-case-and-trim over a 1000-row batch takes ~490 µs against ~32 µs
    compiled (≈15×; ~2M rows/s interpreted).
  - Also: `etl.CreateStmt` and `Reader.Abort` (exported for the plugins),
    `s.WithPaths` (where names resolve), `dbc plugins`, `pipelines_dir`,
    the cats completion manifest, and the headless 130 exit on an ETL
    cancel. README and the dbc skill cover the shell and script faces.

### Phase 2 — the pipeline tab in `dbc web`

- Routes from §6 for pipelines, check, preview, run, export, plugins,
  schema; a minimal `jobs.Engine` that runs single pipelines (its job
  parts come in Phase 3) owned by `Server`, with the `job.*` events.
- `stage.js` factored out of `plan.js`; `pipelines.js` (palette, canvas,
  inspector, run panel), the tab kind, the scripts browser's new sections,
  CSS.
- The grid shows preview results; the log shows run lines.
- Tests: `web/*_test.go` for every route (conflict, check diags, preview
  returns a run and a `job.preview` event); an e2e step that opens the
  example, drags a `preview` node onto a lane, connects it, previews, and
  sees rows in the grid; a JS error fails it.
- Outcome: a pipeline can be built, previewed and run without writing
  JSON or Go.

  ✅ **Done 2026-10-09.** What landed, and where it differs from the sketch:
  - Package `jobs` holds the engine's first half: `Engine.StartPipeline`
    (a `Request`: spec, params, fragment, preview rows, trigger, and the
    host's `Origin` and `Source`), `Cancel`, `Get`, `Running`, `Recent`,
    `Wait`, `Close`. Each run gets its own `sdb.S` (DDL log on, `Release`
    at the end) and a `Run` record shaped for Phase 3 (`Pipelines
    []PipelineRun`, the log capped at 2000 lines), kept in memory (the
    last 50). Refused: a spec Check rejects, a second real run of a
    running pipeline, a second run from the same origin (`ErrBusy`).
    Events: `RunStarted`, `Progress` (a state change at once, batch
    counters coalesced to 250 ms), `Logged`, `Preview`, `RunDone`, one run's
    in order. An error is logged with its serr context ("unknown
    connection — conn demo-sqlite · fragment clean · node src").
  - `pipeline.Options.Progress` now also reports each fragment's start
    and end (final status, skipped included), so a host draws states from
    the one callback.
  - dbc web: `Server.jobs`; routes for the store (the scripts protocol),
    `pipeline-check` (diags placed by line for the JSON view),
    `pipeline-preview` (the editor's text, unsaved), `pipeline-run` (the
    saved file or an example), `pipeline-export/:name`, `plugins`, `runs`,
    `runs/:id`, `runs/:id/cancel`; window events `pipelines`, `job.run`,
    `job.progress`, `job.line`, `job.preview`, `job.done`. A preview's rows
    land in the asking tab's grid through `workspace.ShowResult` (a
    script's s.Show path, keyed by the run). Saved tabs gained a
    `pipeline` column.
  - The page: `pipelines.js` (the tab's model, canvas, inspector, runs,
    JSON view), `stage.js` (pan and zoom), the tab kind (`t.pipeline`,
    `.pipe-mode` over `.script-mode`), the scripts browser's Pipelines
    section, pipeline examples and New pipeline. Previews and runs show
    on the canvas: counters on the cards, a state per lane, the busy mark
    on the tab that started the run, its ■ Stop. Saving and drafts follow
    the script tab; a save over a file changed elsewhere asks (keep this
    tab's, or load the file's), since a canvas has no undo to hand the
    other version to. ⇪ Go exports into a script tab.
  - Not as sketched: **no `pipeline-schema` route** — the vendored Monaco
    has no JSON language service to consume one (its bundle registers a
    `json` language whose module is not shipped), so the JSON view uses a
    Monarch grammar of its own (`dbcjson`, editor.js) and the check's
    markers. **`plan.js` was not refactored** onto `stage.js`: it is
    inlined alone into the standalone plan page under a CSP hash, and the
    shared part is a dozen lines. SQL and Go fields are code boxes, not
    Monaco mini-editors; a `table` field is a plain line (a pipeline tab
    is not connected, so there is no catalog to pick from); fragments are
    reordered from a lane's ⋯ menu rather than by dragging the lane.
  - Found on the way: the web e2e step "script tabs" had failed since
    Phase 1 (the suggest widget draws only the rows in view, and the new
    `S` methods pushed `Query` and `Show` out of it); it now narrows by
    prefix.

### Phase 3 — jobs, the scheduler, run records

- `jobs/`: `Spec`, `Check`, the engine (readiness walk, `max_parallel`,
  policies, cancel, timeout), `cron.go`, the scheduler, `Run` records,
  `userdata/jobs.go`, `userdata/runs.go`, retention; `s.RunJob`.
- CLI: `dbc jobs`, `dbc job run|check`, `dbc runs` (and `--sql`),
  `dbc run show`.
- `dbc web`: the engine completed, the scheduler started with the server
  and stopped in `Shutdown`, the jobs routes, the webhook.
- The example job.
- Tests: `cron.Next` against a table of expressions and instants
  (including DST days and month ends); DAG checks; an engine test with a
  diamond of four tiny pipelines on SQLite asserting fan-out concurrency
  (both middle pipelines observed running at once with `max_parallel: 2`)
  and fan-in order; failure policies; overlap skip; a record written at
  each state and `interrupted` after a simulated crash; the webhook with
  and without the secret.
- Outcome: `nightly` runs at 02:00 while `dbc web` is up, and
  `dbc job run nightly` runs it from cron.

  ✅ **Done 2026-10-09.** What landed, and where it differs from the sketch:
  - `jobs/spec.go`: `Spec` as sketched plus job-level `params` (a step's
    param values substitute `${param}` and `${run.…}` when it starts);
    `root` may be left out when one step has no `after`. `CheckJob` checks
    the DAG (Kahn), every step against its pipeline (found through
    `CheckOptions.Find`: `pipelines_dir`, then the examples), params both
    ways, cron lines (parse, and fire at all), the
    zone, the policy. **No `"manual"` trigger key**: by hand (UI, CLI,
    script) is always allowed; `webhook` is opt-in per job.
  - `jobs/cron.go`: five fields, names, steps, macros, Vixie's
    either-day rule. DST: a wall time the clocks skip fires at the jump, a
    repeated one once (the search walks wall time and maps each match to
    the earliest instant showing it).
  - The engine (`jobs/engine.go`, `job.go`): `StartJob` beside
    `StartPipeline` on one `Run` record shape (`Kind`, `By`,
    `PipelineRun.After`/`Params`, `Line.Pipeline`). The readiness walk with
    `max_parallel`, `finish_branches` / `stop`, `timeout` (a cause on the
    walk's context), `overlap: skip | queue` (one waits at most; a queued
    run's header says `queued`). One real run of a *pipeline* at a time
    engine-wide, a job's steps included. New events `State` (a step's or a
    queued run's state) and `Notice` (the scheduler's words). The sketch's
    `StartJob(name, …)` takes a parsed spec (`JobRequest`) like
    `StartPipeline`; `LoadJob` resolves names.
  - `${run.id|date|time|started|trigger|job}` (`pipeline.RunVars`) work in
    any node: `pipeline.Options.Run`, with the clock ones defaulted, and
    `Check` now refuses an unknown `run.` name.
  - Records (`jobs/records.go`, `userdata/runs.go`): written at the start,
    every 2 s, when a job step starts or ends, and at the end; never for a
    preview. **Interrupted is judged by the file's age** (a live record is
    rewritten every 2 s; one silent for 30 s has lost its writer), not a
    PID, so a `dbc web` starting while a cron `dbc job run` runs does not
    mark that run dead. Listings settle such records; `Recover` (called by
    dbc web and `dbc job run` at start) rewrites them. `runs_keep` prunes,
    never a record another process is still writing.
  - The scheduler (`jobs/scheduler.go`) is dbc web's only, for `jobs_dir`
    only (never the examples), rescans the directory each minute and on a
    save; a fire later than a minute is a miss (told, not run) unless
    `catch_up`, which also runs the latest fire missed while dbc web was
    down, judged by the job's newest record. Two dbc web processes on one
    machine claim each fire with an O_EXCL file in `runs_dir/.fires`, so
    it runs once.
  - `s.RunJob` (`sdb/jobs.go`): sdb cannot import jobs, so the runner is a
    hook — dbc web's workspaces get `Engine.ScriptRunner()`, any other
    host `sdb.DefaultJobRunner`, a private engine whose lines are the
    script's Print lines.
  - CLI (`jobscmd.go`): `dbc jobs`, `dbc job run|check`, `dbc runs` (with
    `--job`, `--pipeline`, `--status`, `--since`, `--limit`), `dbc runs
    --sql` (tables `runs`, `pipelines`, `fragments`, `nodes` in a
    throwaway bytdb file), `dbc run show` (`Run.Tree`). `dbc pipeline run`
    now goes through an engine too, so it leaves a record. **Not done**:
    `dbc job run --wait=false` (it needs a detached process to outlive the
    command; raised in the next-list).
  - dbc web (`web/jobs.go`): the store, `job-check` (with each cron line's
    next five fires), `POST /api/v1/jobs/:name/run` (manual with the
    session cookie, the webhook with the Bearer secret — 403 unless the
    job sets `webhook`), `GET /api/v1/runs` filters adding the history,
    `GET /api/v1/runs/:id` reading back from disk, window events `jobs`,
    `job.state`, `job.notice`. The page has no jobs tab yet (Phase 4):
    `pipelines.js` keeps job runs apart from pipeline runs and logs their
    start and end.
  - The example job `nightly` needed a fourth example pipeline,
    `breed-counts`, for its fan-out.

### Phase 4 — the jobs tab and the Runs view

- `dag/` extracted from `erd/layout.go` (erd's output unchanged, pinned by
  its existing picture tests); the layout route.
- `jobs.js`: the DAG canvas, triggers (next fire times), policy, Run.
- `runs.js`: the Runs table with filters and live rows; the run page with
  the three drilldown levels, the fragments timeline, the node table, the
  filtered log, *Open preview*; catch-up on reload.
- Tests: route tests; e2e: start the example job, watch the run page
  reach `succeeded`, open a fragment, see the node counters.
- Outcome: the monitoring view the ask describes, with drilldown.

  ✅ **Done 2026-10-09.** What landed, and where it differs from the sketch:
  - Package `dag` (`Ranks`, `Layers`, `Neighbours`, `Order`, `WrapTarget`,
    `Wrap`, `Place`, and `Layout` running them all): `erd/layout.go`'s
    steps 1–4 now call it, with its groups, boxes, widening and routing
    its own. erd's output is unchanged — not only by its tests: the
    layouts of the fixtures and of 300 random schemas (cycles, duplicate
    keys, self-references) were fingerprinted before and after, and match.
  - `jobs.LayoutSteps`, `Spec.Layout`, `Run.Layout`: a card per step,
    left to right by rank, never wrapped (a wrapped rank would read as
    "runs after"); the card size is the server's (`Layout.Card`). Routes:
    `GET /api/v1/jobs/:name/layout` as sketched, and — what the tab uses —
    `job-check` returns the layout of the text being edited, and
    `GET /api/v1/runs/:id` a job run's as it ran.
  - The jobs tab (`jobs.js`, `t.job`, `#jobp`, a saved tab's `job`
    column): the DAG canvas over the editor, as a pipeline's is. **No
    positions of the user's**: the cards go where the layout puts them,
    re-laid at once after a change of shape. A palette of pipelines (the
    user's, then the examples); drop on a card = a step after it; an
    output ● dragged to a card = a dependency (a loop refused); Delete,
    Ctrl+D. The inspector: the job's params, cron lines each with its next
    five fires as you type, the zone, catch-up, the webhook (its URL and a
    ⧉ curl), the policy; a step's pipeline (↗ opens it), afters, and the
    pipeline's params with their defaults. ▶ Run saves, runs and turns the
    tab to its Runs face on the new run's page; the design cards show the
    newest run's states. JSON view, drafts, conflicts as a pipeline tab's.
    The scripts browser has Jobs, Job examples and job trash; + New ▾ → Job.
  - The Runs view (`runs.js`): a dialog (`Alt+R`, ◷ Runs on the top bar)
    and a job tab's face — **not a tab of its own**. The list (filters:
    which job or pipeline, status, age; live rows over the history). The
    run page: the DAG with states, times, rows and the critical path in
    bold; a step's fragments as bars on the run's time axis; a fragment's
    nodes (in, out, batches, rows/s, time, the error pinned); the log
    narrowed to the selection; Stop, copy id, open the file. Live from the
    `job.*` events; a run another process runs is re-read every 2 s.
    **Open preview** cannot bring back a past run's preview rows (they are
    not recorded): it goes to the tab whose grid has them (the run's
    origin), or previews the fragment again.
  - `dbc run cancel ID` asks a running dbc web through its API (`--url`,
    `--secret` / `$DBC_WEB_SECRET`: dbc web keeps its secret in memory
    only), waits for the run to end and says how. Found on the way:
    `Engine.Cancel` of a run another process was running answered as if
    stopped; it is now `ErrElsewhere`, a 409.
  - `jobs.Spec.JSON` puts arrays of scalars on one line, as a pipeline's
    (`pipeline.CompactArrays`), and so does the canvas (`jobText`).
  - Tests: `dag`, `jobs` (`TestLayoutSteps`, `TestSpecJSONCompact`), web
    routes (`TestJobLayout`, `TestRunRecordLayoutAndCancelElsewhere`,
    `TestJobTabSaved`), the CLI's client (`TestWebClient`), and the web e2e
    step "job tabs and the runs view": the example copied and laid out, a
    drag and a wire, a save, a run to succeeded, a step, a fragment and
    its counters, the dialog, a reload, a new job run live and stopped.

### Phase 5 — the TUI

- The browser (`Ctrl+J`), the Runs modal, the run monitor tree, `$EDITOR`
  with `Check`, the status-bar indicator; `Model` owns an `Engine`.
- The TUI e2e suite gets a step: open the browser, run the example
  pipeline, see its lines in the log and the monitor reach `✓`.
- Outcome: everything but drawing works in the terminal.

  ✅ **Done 2026-10-09.** What landed, and where it differs from the sketch:
  - **The Model owns an engine** (`tui/jobs.go`), records in `runs_dir`
    (memory under `Options.NoPersist`), `Recover` at start (its count in
    the startup log), `Close` on quit — every run canceled and rolled
    back, said on stderr. Its events reach Update through a **pump**: an
    unbounded FIFO with a goroutine of its own, because Bubble Tea's
    `Send` blocks and the engine sends `RunStarted` from inside the
    `StartPipeline` that Update called — a sink calling `Send` directly
    would deadlock the program. Query tabs' workspaces get
    `Engine.ScriptRunner()`, so a TUI script's `s.RunJob` runs there too.
    `tui.Run` raises the in-memory SQLite pool to 16, as dbc web does.
  - **The browser** (`tui/pipes.go`): jobs, pipelines, the examples and
    both trashes, with the scripts browser's keys — so **not** the plan's
    `d` trash and `r` runs: `d` duplicates and `r` renames there, and the
    two browsers sit side by side. `Enter` runs (prompting only for a
    param with no default), `p` runs prompting for every param, `e` edits
    the JSON in `$EDITOR` (an example: copied first), `n` makes a pipeline
    (a source into a preview on the tab's connection) or a job (one step),
    `d` duplicates or copies an example — the spec's own `name` set to its
    file's, as dbc web does — `h` the row's runs. An example **runs as it
    is** (unlike a script example, a spec example runs by name headless).
    The check after `$EDITOR` logs each finding as `path:line:col: where:
    msg`: the web's `locate` moved to `pipeline.Locate` / `ParseErrorAt`,
    with a new `jobs.Locate` for a job's wheres, and the web's
    connection list for the check to `jobs.CheckConns`.
  - **No toolbar button**: a tenth button pushes a 120-column terminal
    from the "▶ Run" labels down to bare glyphs. The scripts browser has a
    `⇉ Pipelines & jobs ^J` chip instead, and the new browser `ƒ Scripts
    ^O` back (`Ctrl+O` / `Ctrl+J` switch between them too).
  - **The run monitor** (`tui/runs.go`): run → pipelines → fragments →
    nodes, with `dbc run show`'s glyphs; each row's time and rows (a
    node's in → out) in columns, then its afters, direct COPY, batches,
    rows/s (only past 100ms) or error; `Enter`/`←`/`→` fold. The row under
    the cursor narrows the log below it (a step's lines; a fragment's: its
    step's logged while it ran, or naming it) and pins its error in full,
    as the web's run page does. Live by applying the run's events to a
    copy (a line appended, a fragment's counters replaced, a state change
    re-read); a run another process runs is re-read every 2s. `^K` stops
    (another process's: refused with where it runs), `y` / `Y` copy the id
    / the run as `Run.Tree` text, `Backspace` back to the list.
  - **The Runs list** (`Alt+J` — the plan named none; dbc web's `Alt+R` is
    the TUI's run-all): `Engine.History`, filtered by typing, narrowed to
    one spec from the browser's `h` (`a` widens it). It re-reads the
    records only when `userdata.RunsStamp` (the newest record directory's
    mtime) has moved, since every listing reads every record whole.
  - **Logs and the status bar**: a run's lines go to the log on screen
    tagged `[nightly › clean] …`; the status bar's right end says `●
    nightly 3m12s` (`+N` for more), a click opens the run (or the list);
    `Ctrl+C` does not quit over a run, `Ctrl+Q` does and stops it. A
    preview's rows land in the grid of the tab the run was started from
    (`Workspace.ShowResult`, as dbc web lands them), marked • when it is
    in the background. Manual runs from the terminal are `By: terminal`.
  - Tests: `tui/jobs_test.go` (a pipeline and a job run from the browser
    with their lines, preview, monitor and list; params asked; a stop from
    the monitor and the status bar; new / edit / check / copy / trash /
    restore; quitting stops a run; the pump's order and that it never
    blocks), `jobs.TestLocate`, `pipeline.TestLocate`,
    `userdata.TestRunsStamp`; the TUI e2e step "Ctrl+J: …" in the real
    binary (a pipeline, the Runs list with a headless `dbc pipeline run`'s
    run in it, a job, a stop, `$EDITOR` and the check, and `dbc runs`
    listing the TUI's runs).

### Phase 6 — the plugin SDK and the rest of the built-ins

- `pipeline.LoadPlugins`, `userdata/plugins.go`, `Check` per kind,
  `dbc plugins`, the palette's *Yours* section, the assistant summary.
- Built-ins: `csv.read`, `jsonl.read`, `csv.write`, `jsonl.write`,
  `cols.cast`, `cols.add`, `text.clean`, `lookup`, `rows.dedupe`,
  `discard`, `pipeline.check`; `sql.write` gains `mode: upsert` on Postgres
  and SQLite.
- Docs: a README chapter (*Pipelines and jobs*), the dbc skill's
  `scripting.md` extended, a `plugins/` example file embedded.
- Outcome: the plugin story is complete and documented.

## Testing

- Unit tests per package as listed; the runner and engine tests use SQLite
  files and bytdb in temp dirs, never the network.
- `etl`'s live suites (`DBC_LIVE_PG_DSN`, `DBC_LIVE_MYSQL_DSN`) gain
  pipeline cases: the direct collapse, a Go transform between engines, a
  cancel during COPY.
- The web and TUI e2e suites gain one step each per phase, as above.
- A throughput check kept in the README: rows/s for direct COPY, for a
  built-in transform, and for a `go.transform`, on the author's machine.

## Risks

- **Interpreter cost.** If the benchmark says a `go.transform` is too slow
  for real loads, the answer is more built-ins (compiled) and the *Later*
  parallel driver, not a different interpreter. The batch API keeps the
  interpreter boundary crossing per batch, which is the main lever.
- **Scope.** This is the largest plan in `ai_docs/plans/`. The phase cut
  keeps each step shippable: Phase 1 alone is a usable headless ETL tool;
  Phases 1–2 a visual one; 1–3 a scheduler. Nothing later is needed for
  what came before to be worth having.
- **Two specs to keep honest.** The JSON spec and the Go builder must stay
  equivalent; `Gen`'s round-trip test (spec → Go → run → same stats) is
  the guard, and new plugin fields are declared once, in `Fields`.
- **Fan-out and connections.** A `max_parallel` too high for a small pool
  blocks rather than fails; the log says which pipeline waits on which
  connection. Documented, with the default at 2.
- **Non-atomic DDL on MySQL and bytdb** (today's `etl` caveat) now applies
  per fragment: a failed load can leave an empty created table. Unchanged
  behavior, said in the fragment's error.
- **The canvas is new UI ground** for this codebase (nothing drag-and-drops
  today). `plan.js` gives the stage; ports and edges are new. Keep it
  pointer-event based and small; an e2e step from Phase 2 on keeps it
  working in Chrome, and N-061's Safari/Firefox check extends to it.
- **Cron edge cases** (DST, `dow` and `dom` both set) are where in-house
  parsers go wrong; the test table is the defence, and the UI's "next five
  fire times" lets a user see what they wrote.

## Later

- **A parallel fragment driver**: a goroutine per node with bounded
  channels; a transform flagged `pure` may run N copies on N interpreters.
- **Commit every N batches** (`checkpoint`) on `sql.write`, with resume
  from the last checkpoint on retry — the answer to "100M rows failed at
  99M"; needs an idempotent write mode (`upsert`) to be safe.
- **Retries with backoff** per pipeline (`retry: 3, backoff: 1m`) for
  transient failures (lock timeouts, a dropped connection).
- **Notifications** on failure or success: a webhook out, an e-mail, a
  macOS notification from dbc.app.
- **`dbc run cancel` and `dbc job run --remote`** talking to a running
  `dbc web`, so a cron job and the server share one history writer.
- **Secrets** for plugin configs (`${secret:NAME}` from the keychain).
- **Row-level error handling**: `on_row_error: skip|dead-letter` with a
  dead-letter sink, instead of all-or-nothing per fragment.
- **Lineage**: which tables a run read and wrote, from the plugins'
  declarations, shown on the ERD.
- **Versioned specs**: a `.history/` beside a spec, or just git.
- **A WASM plugin host** for compiled, sandboxed plugins, if interpreted
  Go ever proves too slow or too open.
