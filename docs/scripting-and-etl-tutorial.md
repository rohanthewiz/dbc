# Scripting and ETL with dbc — a tutorial

This tutorial builds a small, working data flow with dbc, one layer at a
time. You start with a Go script that queries one database and end with a
scheduled job that rebuilds a warehouse, exports CSVs and keeps an audit
trail. Every command and every output below was run against a fresh build.

> **Follow along interactively.** `dbc tutorial init` makes the workspace
> from §0 for you. Then, in it, `dbc tutorial step 0` shows a lesson,
> `dbc tutorial step 0 --run` writes its files and runs its commands, and
> `dbc tutorial check 0` says whether it worked. `dbc tutorial` lists every
> lesson with ✓ or ✗.

```
   shop (SQLite)                              warehouse (bytdb)
 ┌───────────────┐   dbc copy / s.Copy      ┌──────────────────┐
 │ customers     │ ───────────────────────► │ customers        │
 │ orders        │   Reader → Go → Writer   │ order_facts      │
 └───────────────┘ ───────────────────────► │ dim_customer     │
         │                                  │ load_log         │
         │        pipeline: shop-sync       └──────────────────┘
         └──────────────────────────────►  data/exports/*.csv
```

dbc offers four ways to move data. Each one builds on the one before it:

| Tool | Best for | You write |
| --- | --- | --- |
| `dbc copy` | Copying one table between connections, in a shell or cron | A command line |
| Go scripts (`sdb.S`) | Anything: loops, reports, joins across servers, custom ETL | A `Run(s *sdb.S) error` function |
| Pipelines | Repeatable ETL made of reusable nodes, which you can preview, check and draw | JSON, a canvas in dbc web, or a Go builder |
| Jobs | Several pipelines in a dependency graph, on a schedule or a webhook | JSON, or a canvas in dbc web |

The README is the reference manual. The relevant sections are "Scripting in
Go", "ETL across connections", "Pipelines", "Jobs", "Plugins of your own"
and "Headless mode". This page is the guided tour.

---

## 0. Set up a workspace

Keep the tutorial in its own directory. A `dbc.toml` there is picked up when
you run dbc from that directory. Every `*_dir` setting in it is relative to
the file, so the scripts, pipelines, jobs, run records and output files all
stay together:

```sh
mkdir ~/dbc-tutorial && cd ~/dbc-tutorial
```

`dbc.toml`:

```toml
# Tutorial workspace: everything lives beside this file.
scripts_dir   = "scripts"     # dbc script NAME, Ctrl+O
pipelines_dir = "pipelines"   # dbc pipeline run NAME, Ctrl+J
jobs_dir      = "jobs"        # dbc job run NAME
runs_dir      = "runs"        # one JSON record per pipeline/job run
plugins_dir   = "plugins"     # your own pipeline node kinds
files_dir     = "data"        # where relative file paths land (exports, CSVs)
default_connection = "shop"

[[connection]]
name   = "shop"
driver = "sqlite"
dsn    = "file:shop.db"

[[connection]]
name   = "warehouse"
driver = "bytdb"
dsn    = "warehouse.bytdb"    # created on first open
```

Both drivers are embedded, so you don't need a server. Everything below
works the same way on Postgres and MySQL. Only the placeholders differ (`?`
for SQLite/MySQL, `$1` for Postgres/bytdb), and Postgres → Postgres copies
get faster (see §5).

Seed the shop. Save this as `seed.sql`:

```sql
CREATE TABLE customers (id INTEGER PRIMARY KEY, email TEXT NOT NULL, region TEXT);
INSERT INTO customers VALUES
 (1, ' Ada@Example.com ', 'emea'),
 (2, 'grace@example.com', 'amer'),
 (3, 'LINUS@example.com', 'emea'),
 (4, 'ken@example.com', NULL);
CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER NOT NULL,
  total REAL NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL);
INSERT INTO orders VALUES
 (101, 1, 120.50, 'paid',     '2026-10-01'),
 (102, 2,  35.00, 'paid',     '2026-10-02'),
 (103, 1,   9.99, 'refunded', '2026-10-03'),
 (104, 3, 250.00, 'paid',     '2026-10-05'),
 (105, 4,  42.00, 'pending',  '2026-10-07'),
 (106, 2,  18.25, 'paid',     '2026-10-09');
```

```sh
dbc -f seed.sql                              # runs on default_connection (shop)
dbc -t markdown "SELECT * FROM orders"
```

The emails are messy on purpose (spaces, capitals, a missing region). The
ETL steps below clean them up.

---

## 1. Your first script

A script is a Go file with one function. dbc interprets it with
[yaegi](https://github.com/traefik/yaegi), so there is no compile step and
the whole standard library is available. Third-party imports are not
available.

`scripts/order_summary.go`:

```go
//go:build ignore

// Summarize orders by status, then list the big ones.
package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	r, err := s.Query("shop", `SELECT status, count(*) AS orders, sum(total) AS revenue
		FROM orders GROUP BY status ORDER BY revenue DESC`)
	if err != nil {
		return err
	}
	s.Print("%d statuses in %s", len(r.Rows), r.Duration)
	s.Show(r)

	// Placeholders follow the driver: ? for SQLite/MySQL, $1 for Postgres/bytdb.
	big, err := s.Query("shop", "SELECT id, customer_id, total FROM orders WHERE total >= ? ORDER BY total DESC", 40)
	if err != nil {
		return err
	}
	s.Show(big)
	return nil
}
```

What each part does:

- **`//go:build ignore`** keeps `go build ./...` from compiling scripts that
  live inside a Go module. dbc runs the file either way.
- **The comment above `package main`** is the script's description. Its
  first sentence appears in `dbc scripts` and in both UIs' script browsers.
- **`s.Query(conn, sql, args...)`** takes a connection *name* from the
  config, never a DSN. A script can't define its own connections. Results
  are capped at `max_rows` (default 1000). §5 covers uncapped streaming.
- **`s.Print`** writes progress. **`s.Show`** emits a result: in the TUI it
  becomes a results tab, and headless it goes to stdout.

Run it by name. dbc looks in `scripts_dir`, with `.go` optional:

```text
$ dbc script order_summary
3 statuses in 82.875µs
-- #1 │ shop │ 3 rows in 80µs
-- SELECT status, count(*) AS orders, sum(total) AS revenue FROM orders GR…
status    orders  revenue
--------  ------  -------
paid      4       423.75
pending   1       42
refunded  1       9.99

-- #2 │ shop │ 3 rows in 40µs
-- SELECT id, customer_id, total FROM orders WHERE total >= ? ORDER BY tot…
id   customer_id  total
---  -----------  -----
104  3            250
101  1            120.5
105  4            42
```

Output formats work as they do for plain SQL. With a machine-readable `-t`,
`s.Print` lines move to stderr, so stdout holds one clean document:

```sh
dbc -t json script order_summary | jq '.[0].rows'
dbc -t csv -o summary.csv script order_summary
```

`dbc scripts` lists your scripts, followed by the built-in examples. You can
run the examples by name too (`dbc script loop_params`):

```text
$ dbc scripts
1 script(s) in ~/dbc-tutorial/scripts, and 5 built-in example(s)
name              modified          size  description                                                   kind
----------------  ----------------  ----  ------------------------------------------------------------  -------
order_summary.go  2026-10-11 00:22  649   Summarize orders by status, then list the big ones.           script
copy_table.go                       4227  Copy a Postgres table from one connection to another, then …  example
export_report.go                    1200  Loop over params and export each result to CSV, plus one co…  example
loop_params.go                      651   Run a parameterized query in a loop.                          example
...
```

### The `sdb.S` API at a glance

| Method | Purpose |
| --- | --- |
| `s.Conns()` | Configured connection names |
| `s.Query(conn, sql, args...)` | Query → `*sdb.Result` (`Columns`, `Rows [][]string`, `Raw [][]any`, `Duration`) |
| `s.Exec(conn, sql, args...)` | Statement → rows affected |
| `s.DB(conn)` | Raw `*sql.DB`, for transactions and prepared statements |
| `s.Explain(conn, sql, analyze)` | A query plan, with findings |
| `s.Show(r)` / `s.Print(f, …)` | Emit a result / a progress line |
| `s.Export(r, format, path)` | `csv`, `tsv`, `markdown`, `html`, `json`, `text`; an empty path copies to the clipboard |
| `s.Path(p)` | Where a relative file path really is (`files_dir`) |
| `s.Canceled()`, `s.Ctx()` | Honor Ctrl+K / Ctrl+C in long loops |
| `s.Copy`, `s.Reader`, `s.Writer` | ETL (§5) |
| `s.RunPipelineNamed`, `s.RunPipeline`, `s.RunJob` | Orchestration (§9) |

---

## 2. Reports and files

`scripts/region_report.go` writes one CSV per region, plus an HTML overview:

```go
//go:build ignore

// Export one CSV per region, plus an HTML overview.
package main

import (
	"fmt"

	"github.com/rohanthewiz/dbc/sdb"
)

// Knobs live at the top of the file: a script takes no arguments.
const minTotal = 20.0

func Run(s *sdb.S) error {
	regions, err := s.Query("shop", "SELECT DISTINCT coalesce(region, 'none') FROM customers ORDER BY 1")
	if err != nil {
		return err
	}
	for _, row := range regions.Rows {
		if s.Canceled() { // Ctrl+K in the TUI, Ctrl+C headless
			return s.Ctx().Err()
		}
		region := row[0]
		r, err := s.Query("shop", `SELECT o.id, c.email, o.total, o.status
			FROM orders o JOIN customers c ON c.id = o.customer_id
			WHERE coalesce(c.region, 'none') = ? AND o.total >= ?
			ORDER BY o.id`, region, minTotal)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("reports/orders_%s.csv", region)
		if err := s.Export(r, "csv", name); err != nil { // relative → files_dir
			return err
		}
		s.Print("%-5s %d orders → %s", region, len(r.Rows), s.Path(name))
	}

	all, err := s.Query("shop", "SELECT coalesce(c.region,'none') AS region, count(*) AS orders, sum(o.total) AS revenue FROM orders o JOIN customers c ON c.id = o.customer_id GROUP BY 1 ORDER BY 1")
	if err != nil {
		return err
	}
	s.Show(all)
	return s.Export(all, "html", "reports/overview.html")
}
```

```text
$ dbc script region_report
amer  1 orders → ~/dbc-tutorial/data/reports/orders_amer.csv
emea  2 orders → ~/dbc-tutorial/data/reports/orders_emea.csv
none  1 orders → ~/dbc-tutorial/data/reports/orders_none.csv
-- #1 │ shop │ 3 rows in 40µs
...
```

**Files go through `files_dir`.** A relative path given to `s.Export` lands
in `files_dir`, not in the shell's working directory. `files_dir` defaults
to `$HOME`; here it is `data/`. Pipelines' file nodes use the same
directory, so a script gives the same result whether it runs from a shell,
the TUI, dbc web or a schedule. Open your own files the same way:

```go
f, err := os.Create(s.Path("exports/raw.txt"))  // not os.Create("exports/raw.txt")
```

`s.Export` creates missing directories. `s.Path` only resolves the name,
so call `os.MkdirAll(filepath.Dir(p), 0o755)` before you write somewhere
new.

**Parameters.** A script takes no command-line arguments. Put the knobs in
constants at the top of the file, as the built-in examples do, or read a
small settings file through `s.Path`, or read an environment variable:
`os.Getenv` sees the environment of the dbc that runs the script (your
shell's for `dbc script`; `dbc web`'s own for a script run in the browser,
so a variable set in a terminal later doesn't reach it). The script gets a
copy, so its `os.Setenv` changes nothing outside it. When you need real
runtime parameters (`-p key=value`), use a pipeline (§6).

```sh
LIMIT=2 dbc script ./report.go   # os.Getenv("LIMIT") == "2" inside
```

---

## 3. Check before you run

`dbc script --check` parses and compiles a script without connecting to
anything. It also checks `Run`'s signature and lints for interpreter bugs.
Because it never runs the script, it is safe to use on scripts that write
data, and it works as a CI gate:

```sh
dbc script --check region_report            # silent when clean; exit 1 on an error
dbc script --check -t json scripts/*.go     # [{file,line,col,severity,msg}, …]
```

Here it catches the most important interpreter pitfall:

```text
$ dbc script --check bad.go
bad.go:14:3: warning: the script interpreter drops a two-value assignment into a map element (yaegi#1655): assign to a variable, then m[k] = v. (A slice element is fine.)
```

§10 lists this pitfall and the other one.

---

## 4. One-command ETL: `dbc copy`

To copy one table you don't need a script:

```text
$ dbc copy --from shop --to warehouse --create orders
copied 6 rows shop:orders → warehouse:orders in 8ms (row by row)

$ dbc copy --from shop --to warehouse --create --truncate --where "status = 'paid'" orders paid_orders
copied 4 rows shop:orders → warehouse:paid_orders in 4ms (row by row)
```

| Flag | Meaning |
| --- | --- |
| `--create` | Create the destination table if it's missing |
| `--truncate` | Empty the destination first, in the same transaction |
| `--where COND` | Copy only matching rows (written in the *source's* SQL) |
| 2nd table name | The destination table (default: same name) |

**The load is one transaction.** Running the first copy again without
`--truncate` hits the primary key. The copy fails and exits 1, and the
destination still has exactly its six rows. It is never half-loaded:

```text
$ dbc copy --from shop --to warehouse orders
level=error msg="duplicate primary key" ... from="shop:orders" to="warehouse:orders"
$ dbc -c warehouse -t csv "SELECT count(*) FROM orders"
count(*)
6
```

That makes `dbc copy` safe in a crontab:
`0 * * * * dbc copy --from prod --to replica --truncate public.orders`.

---

## 5. ETL in a script: `Copy`, `Reader`, `Writer`

Scripts give you three levels of control. The script below uses two of them.

`scripts/load_warehouse.go`:

```go
//go:build ignore

// Load the shop into the warehouse: clean customers, then a per-region order fact table.
package main

import (
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	// Level 2: Copy with a Transform. Each row arrives as []any in the
	// Columns order; return the edited row, or nil to skip it.
	st, err := s.Copy("shop", "warehouse", "customers", sdb.CopyOpts{
		Columns:  []string{"id", "email", "region"},
		Create:   true,
		Truncate: true,
		Transform: func(row []any) ([]any, error) {
			email, _ := row[1].(string)
			email = strings.ToLower(strings.TrimSpace(email))
			row[1] = email
			if row[2] == nil {
				row[2] = "unknown"
			}
			return row, nil
		},
	})
	if err != nil {
		return err
	}
	s.Print("%s", st)

	// Level 3: Reader + Writer — a join no single server can run. The
	// lookup comes from the warehouse, the facts from the shop.
	region := map[int64]string{}
	lk, err := s.Reader("warehouse", "SELECT id, region FROM customers")
	if err != nil {
		return err
	}
	for lk.Next() {
		row := lk.Row()
		id, _ := row[0].(int64)
		r, _ := row[1].(string) // into a local first: see "yaegi pitfalls"
		region[id] = r
	}
	if err := lk.Err(); err != nil {
		return err
	}

	// The Writer's table must exist; bytdb wants a primary key.
	if _, err := s.Exec("warehouse", `CREATE TABLE IF NOT EXISTS order_facts
		(order_id bigint PRIMARY KEY, region text NOT NULL, total double precision, paid boolean)`); err != nil {
		return err
	}
	w, err := s.Writer("warehouse", "order_facts",
		[]string{"order_id", "region", "total", "paid"}, sdb.WriteOpts{Truncate: true})
	if err != nil {
		return err
	}
	defer w.Abort() // rolls back on any early return; a no-op after Close

	rd, err := s.Reader("shop", "SELECT id, customer_id, total, status FROM orders ORDER BY id")
	if err != nil {
		return err
	}
	defer rd.Close()
	for rd.Next() {
		row := rd.Row() // typed: int64, float64, string, time.Time, nil, ...
		cust, _ := row[1].(int64)
		paid := row[3] == "paid"
		if err := w.Write([]any{row[0], region[cust], row[2], paid}); err != nil {
			return err
		}
	}
	if err := rd.Err(); err != nil {
		return err
	}
	n, err := w.Close() // the commit: nothing is visible until here
	if err != nil {
		return err
	}
	s.Print("order_facts: %d rows", n)
	return nil
}
```

```text
$ dbc script load_warehouse
DDL warehouse: CREATE TABLE IF NOT EXISTS "customers" (
	"id" bigint,
	"email" text,
	"region" text,
	PRIMARY KEY ("id")
)
DDL warehouse: TRUNCATE TABLE "customers"
copied 4 rows shop:customers → warehouse:customers in 6ms (row by row)
DDL warehouse: CREATE TABLE IF NOT EXISTS order_facts
		(order_id bigint PRIMARY KEY, region text NOT NULL, total double precision, paid boolean)
DDL warehouse: TRUNCATE TABLE "order_facts"
order_facts: 6 rows

$ dbc -c warehouse -t markdown "SELECT * FROM customers ORDER BY id"
| id | email | region |
| --- | --- | --- |
| 1 | ada@example.com | emea |
| 2 | grace@example.com | amer |
| 3 | linus@example.com | emea |
| 4 | ken@example.com | unknown |
```

The `DDL …` lines are the **DDL log**. dbc logs every `CREATE`, `ALTER`,
`DROP`, `TRUNCATE`, and similar statement a script runs, just before it
runs, including the statements `Copy` and `Writer` issue on your behalf.
Scripts can't turn it off.

### Choosing a level

| Level | Use when | Notes |
| --- | --- | --- |
| `s.Copy(src, dst, table, opts)` | Table or query → table, maybe with per-row edits | `Columns`, `Where`+`Args`, or `Query`; `Create`, `Truncate`, `Transform`, `ProgressEvery` |
| `s.Reader(conn, sql, args...)` | You need to read rows uncapped and typed | `for rd.Next() { rd.Row() }`, then `rd.Err()` |
| `s.Writer(conn, table, cols, opts)` | You produce rows yourself | `Write(row)` …, `Close()` commits, `defer w.Abort()` |

### How the rows move (and what that guarantees)

- **Postgres → Postgres** with no `Transform` and no `Args` streams `COPY …
  TO STDOUT` straight into `COPY … FROM STDIN`. The rows are never decoded,
  so the copy is lossless and runs as fast as the servers allow. Other
  copies into Postgres use `COPY FROM STDIN` in text format. Copies into
  MySQL, SQLite or bytdb use batched multi-row `INSERT`s. The summary line
  says which path ran: `(direct COPY)` or `(row by row)`.
- **One transaction.** A failed, canceled or panicking copy leaves the
  destination as it was. The exception: on bytdb and MySQL, `CREATE TABLE`
  runs before the load, so a failure there can leave an *empty* new table,
  but never a partial one.
- **`Create`** keeps exact column types only for Postgres → Postgres.
  Between other engines, columns get broad types plus the source's primary
  key. Defaults, sequences and other indexes are not copied.
- **bytdb needs a primary key.** To copy a *query* into bytdb, create the
  table first. bytdb has no exact decimal type, so `numeric` lands as
  `double precision`.
- **A move instead of a copy.** On Postgres, `Query: "DELETE FROM jobs WHERE
  done RETURNING *"` archives rows. The delete commits only after the load
  does.
- **Unclosed means rolled back.** A `Writer` the script never `Close`s is
  rolled back when `Run` returns.

> [`scripts/copy_table.go`](../scripts/copy_table.go) is the Postgres version
> of this section: a direct COPY, a filtered copy with a `Transform`, and a
> join across two servers. It's a built-in example: `dbc script copy_table`
> shows it once you've edited its connection names.

---

## 6. Pipelines: ETL as data

A script is code. A **pipeline** describes the same kind of work as data:
small reusable nodes wired into a tree. In exchange you get things a script
doesn't have:

- runtime **parameters** (`-p key=value`)
- **previews** that write nothing
- **validation** before anything runs
- **run records** you can query
- a **canvas** to view and edit the pipeline in dbc web

### Anatomy

```
pipeline ─┬─ fragment "customers"   source → transforms → sinks   (one tree)
          ├─ fragment "gate"        an action (a check)
          ├─ fragment "big_orders"  source → transforms → sinks
          └─ fragment "log"         an action (SQL)
```

- **Fragments run in order.** Each one is either one source → transforms →
  sinks (a tree: one input per node, with sinks at the leaves) or a single
  action.
- **Rows move in batches** (1000 by default). The source yields a batch,
  each transform reshapes it, each sink loads it. **All sinks of a
  fragment commit together at its end**, so each fragment is
  all-or-nothing.
- **Nothing passes between fragments** except what the sinks committed and
  a few published values: `${frag.<name>.rows}`, a `csv.write`'s
  `${frag.<name>.path}`, and a check's `${frag.<name>.value}`.
- **Every `cfg` value is a string.** Inside one you can reference
  `${param}`, `${frag.…}` and `${run.date}`, `${run.time}`,
  `${run.started}`, `${run.id}`, `${run.trigger}` and `${run.job}`.

`dbc plugins` lists every node kind and its fields:

```text
$ dbc plugins
plugin          kind       label                    fields
csv.read        source     CSV file                 path* delimiter header columns empty_null
go.source       source     Go source                code*
jsonl.read      source     JSON Lines file          path* columns
sql.read        source     SQL query                conn* query* args
sql.table       source     Table                    conn* table* columns where order
cols.add        transform  Add columns              columns*
cols.cast       transform  Cast columns             casts* on_error
cols.select     transform  Select columns           keep drop rename
go.transform    transform  Go transform             code*
lookup          transform  Look up                  conn* query* args key* match add prefix missing max_rows
rows.dedupe     transform  Drop duplicates          key
rows.filter     transform  Filter rows              rules*
rows.limit      transform  Limit rows               rows*
text.clean      transform  Clean text               columns trim collapse case empty_null
csv.write       sink       CSV file (write)         path* delimiter header null
discard         sink       Discard
go.sink         sink       Go sink                  code*
jsonl.write     sink       JSON Lines file (write)  path*
preview         sink       Preview                  rows title
sql.write       sink       Table (load)             conn* table* create truncate mode key setup batch
go.action       action     Go action                code*
pipeline.check  action     Check                    conn* query* args rule* message
script.run      action     Run a script             name*
sql.exec        action     SQL statement            conn* sql*
```

### Write one

`pipelines/shop-sync.json` has four fragments. It loads clean customers
into the warehouse, refuses to go on if that produced nothing, exports paid
orders above a threshold to a dated CSV (enriched by a lookup), and records
the export in an audit table:

```json
{
  "name": "shop-sync",
  "desc": "Customers into the warehouse, then paid orders over min_total to a dated CSV.",
  "params": {
    "min_total": { "default": "20", "doc": "Smallest order total to export" }
  },
  "fragments": [
    {
      "name": "customers",
      "nodes": [
        { "id": "src",   "plugin": "sql.table",  "cfg": { "conn": "shop", "table": "customers", "order": "id" } },
        { "id": "tidy",  "plugin": "text.clean", "cfg": { "columns": "email", "trim": "true", "case": "lower" } },
        { "id": "fill",  "plugin": "go.transform", "cfg": { "code": "func Apply(b *sdb.Batch) (*sdb.Batch, error) {\n\tc := b.Col(\"region\")\n\tfor i := range b.Rows {\n\t\tif b.Rows[i][c] == nil {\n\t\t\tb.Rows[i][c] = \"unknown\"\n\t\t}\n\t}\n\treturn b, nil\n}" } },
        { "id": "dst",   "plugin": "sql.write",  "cfg": { "conn": "warehouse", "table": "dim_customer", "create": "true", "truncate": "true", "key": "id" } }
      ],
      "edges": [ ["src", "tidy"], ["tidy", "fill"], ["fill", "dst"] ]
    },
    {
      "name": "gate",
      "nodes": [
        { "id": "check", "plugin": "pipeline.check", "cfg": {
            "conn": "warehouse", "query": "SELECT count(*) FROM dim_customer",
            "rule": ">= 1", "message": "no customers loaded" } }
      ]
    },
    {
      "name": "big_orders",
      "nodes": [
        { "id": "src",    "plugin": "sql.read", "cfg": { "conn": "shop",
            "query": "SELECT id, customer_id, total, status FROM orders WHERE total >= ${min_total} ORDER BY id" } },
        { "id": "paid",   "plugin": "rows.filter", "cfg": { "rules": "status = paid" } },
        { "id": "who",    "plugin": "lookup", "cfg": { "conn": "warehouse",
            "query": "SELECT id, email, region FROM dim_customer", "key": "customer_id", "match": "id" } },
        { "id": "stamp",  "plugin": "cols.add", "cfg": { "columns": "run_id = '${run.id}'\nexported_at = now()" } },
        { "id": "csv",    "plugin": "csv.write", "cfg": { "path": "exports/big_orders_${run.date}.csv" } },
        { "id": "peek",   "plugin": "preview",   "cfg": { "rows": "10" } }
      ],
      "edges": [ ["src", "paid"], ["paid", "who"], ["who", "stamp"], ["stamp", "csv"], ["stamp", "peek"] ]
    },
    {
      "name": "log",
      "nodes": [
        { "id": "log", "plugin": "sql.exec", "cfg": { "conn": "warehouse",
            "sql": "CREATE TABLE IF NOT EXISTS load_log (id bigserial PRIMARY KEY, started text, run_id text, exported bigint, file text);\nINSERT INTO load_log (started, run_id, exported, file) VALUES ('${run.started}', '${run.id}', ${frag.big_orders.rows}, '${frag.big_orders.path}')" } }
      ]
    }
  ]
}
```

The `big_orders` fragment is a tree. One transform feeds two sinks:

```
src ─► paid ─► who ─► stamp ─┬─► csv    (written to a temp file, renamed in at commit)
                             └─► peek   (shown, never written)
```

Notes on the nodes used:

- **`text.clean`** trims, collapses spaces, changes case, and turns empty
  strings into NULL. It runs at compiled speed.
- **`go.transform`** is inline Go, called once per batch, for anything the
  built-ins can't do. Standard packages it uses are imported for you.
- **`lookup`** reads a side query once, holds it in memory, and joins it to
  each row (`key` = `match`). It can read from any connection, including a
  different engine from the source.
- **`pipeline.check`** is a gate. If the rule fails, the fragments after it
  don't run.
- **`csv.write`** with a relative path writes into `files_dir`. The file is
  renamed into place only when the fragment commits, so a failed run leaves
  the previous file alone.
- **`run.id` is unique per run.** When the jobs engine runs the pipeline
  (`dbc pipeline run`, `dbc job run`, the UIs) it is the id of the run's
  record (`dbc run show <id>`). A script's `s.RunPipelineNamed` gets a
  fresh id of the same shape, with `run.trigger` = `script`, but leaves no
  record to look it up in. `load_log` keys on a `bigserial` all the same.

### Check, preview, run

```sh
dbc pipelines                    # yours, then the examples
dbc pipeline check shop-sync     # silent when clean; exit 1 on an error
```

`check` validates plugin names, required fields, every `${…}` reference,
connection names and the fragment's shape. It catches a typo'd
`${frag.big_order.rows}` before anything runs.

A **preview** sends the first N source rows through the transforms. Every
sink becomes a preview, actions are skipped, and *nothing is written*. Use
`--fragment` to preview a single fragment:

```text
$ dbc pipeline run shop-sync --preview 2 --fragment big_orders
fragment big_orders: sql.read → rows.limit → rows.filter → lookup → cols.add → preview → preview
[big_orders/who] side query: 4 rows, 4 keys
-- #1 │ big_orders │ 2 rows in 0s
-- preview csv
id   customer_id  total  status  email              region  run_id                exported_at
---  -----------  -----  ------  -----------------  ------  --------------------  --------------------------------
101  1            120.5  paid    ada@example.com    emea    20261011-002430-d7c5  2026-10-11T00:24:30.030155-05:00
102  2            35     paid    grace@example.com  amer    20261011-002430-d7c5  2026-10-11T00:24:30.030155-05:00
...
```

> A preview writes nothing, *including the earlier fragments' sinks*. On a
> fresh warehouse, previewing the whole pipeline stops at `lookup` ("the
> side query: no such table") because `dim_customer` doesn't exist yet. Run
> it once for real, or preview fragment by fragment.

Run it for real, overriding the parameter:

```text
$ dbc pipeline run shop-sync -p min_total=30
fragment customers: sql.table → text.clean → go.transform → sql.write
DDL warehouse: CREATE TABLE IF NOT EXISTS "dim_customer" (...)
DDL warehouse: TRUNCATE TABLE "dim_customer"
fragment customers done: 4 rows in 11ms
fragment gate: pipeline.check
[gate/check] check passed: 4 >= 1
fragment gate done: 0 rows in 0s
fragment big_orders: sql.read → rows.filter → lookup → cols.add → csv.write → preview
[big_orders/who] side query: 4 rows, 4 keys
-- #1 │ big_orders │ 3 rows in 0s
-- preview big_orders/peek
id   customer_id  total  status  email              region  run_id                exported_at
101  1            120.5  paid    ada@example.com    emea    20261011-002422-1c9c  2026-10-11T00:24:22…
102  2            35     paid    grace@example.com  amer    20261011-002422-1c9c  2026-10-11T00:24:22…
104  3            250    paid    linus@example.com  emea    20261011-002422-1c9c  2026-10-11T00:24:22…
fragment big_orders done: 3 rows in 4ms
fragment log: sql.exec
DDL warehouse: CREATE TABLE IF NOT EXISTS load_log (...)
fragment log done: 1 rows in 7ms
pipeline shop-sync: 4 fragments, 8 rows in 23ms (succeeded)
  customers    succeeded        4 rows     11ms  src 0→4, tidy 4→4, fill 4→4, dst 4→4
  gate         succeeded        0 rows       0s  check 0→0
  big_orders   succeeded        3 rows      4ms  src 0→4, paid 4→3, who 3→3, stamp 3→3, csv 3→3, peek 3→3
  log          succeeded        1 rows      7ms  log 0→1
```

The table at the end shows rows in → out for every node. Here `paid 4→3`
is the filter at work. Other useful forms:

```sh
dbc pipeline run shop-sync -t json | jq .status       # the run's stats as one document
dbc pipeline run shop-sync --fragment customers       # re-run one fragment
dbc pipeline export shop-sync -o shop_sync.go         # the pipeline as an editable Go script
```

**File paths from the shell.** A relative `path` in a file node is in
`files_dir`, never the current directory. To read a file you just made
here, pass an absolute path: `-p in=$PWD/orders.csv`.

---

## 7. Your own node kind: a plugin

When you find yourself pasting the same `go.transform` into several
pipelines, turn it into a plugin. A plugin is a Go file in `plugins_dir`
with a descriptor and plain functions. There's no interface to implement
and no build step.

`plugins/order_tier.go`:

```go
//go:build ignore

// Adds a tier column (small, medium, large) from a numeric column's value.
package main

import (
	"fmt"
	"strconv"

	"github.com/rohanthewiz/dbc/sdb"
)

var Plugin = sdb.Plugin{
	Name:  "order.tier",
	Kind:  sdb.KindTransform,
	Label: "Order tier",
	Fields: []sdb.Field{
		{Name: "column", Type: sdb.FieldString, Required: true, Doc: "The numeric column to grade."},
		{Name: "medium", Type: sdb.FieldString, Default: "50", Doc: "Lowest value that counts as medium."},
		{Name: "large", Type: sdb.FieldString, Default: "200", Doc: "Lowest value that counts as large."},
	},
}

// Package-level state is per node: every node gets its own interpreter.
var medium, large float64

// Open runs once per node before the first batch: parse settings here.
func Open(e *sdb.Env) error {
	var err error
	if medium, err = strconv.ParseFloat(e.Cfg.Str("medium", "50"), 64); err != nil {
		return fmt.Errorf("medium: %w", err)
	}
	if large, err = strconv.ParseFloat(e.Cfg.Str("large", "200"), 64); err != nil {
		return fmt.Errorf("large: %w", err)
	}
	return nil
}

// Apply runs once per batch.
func Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error) {
	col := e.Cfg.Str("column", "")
	if b.Col(col) < 0 {
		return nil, fmt.Errorf("no column %q", col)
	}
	b.AddCol("tier", "text", func(row int) any {
		var v float64
		switch n := b.Get(row, col).(type) {
		case float64:
			v = n
		case int64:
			v = float64(n)
		default:
			return nil // NULL or not a number
		}
		switch {
		case v >= large:
			return "large"
		case v >= medium:
			return "medium"
		}
		return "small"
	})
	return b, nil
}
```

```text
$ dbc plugins --check            # every file in plugins_dir; exit 1 on an error
$ dbc plugins | grep order.tier
order.tier      transform  Order tier               column* medium large    Adds a tier column (small, medium, large) …
```

Use it like any built-in node. `pipelines/order-tiers.json`:

```json
{
  "name": "order-tiers",
  "desc": "Grade every order with the order.tier plugin and preview it.",
  "fragments": [
    {
      "name": "tiers",
      "nodes": [
        { "id": "src",  "plugin": "sql.table",  "cfg": { "conn": "shop", "table": "orders", "columns": "id, total", "order": "id" } },
        { "id": "tier", "plugin": "order.tier", "cfg": { "column": "total", "medium": "40" } },
        { "id": "peek", "plugin": "preview",    "cfg": { "rows": "10" } }
      ],
      "edges": [ ["src", "tier"], ["tier", "peek"] ]
    }
  ]
}
```

```text
$ dbc pipeline run order-tiers
fragment tiers: sql.table → order.tier → preview
-- #1 │ tiers │ 6 rows in 0s
-- preview tiers/peek
id   total  tier
---  -----  ------
101  120.5  medium
102  35     small
103  9.99   small
104  250    large
105  42     medium
106  18.25  small
```

Each kind of plugin provides its own functions. Optional ones are in
brackets:

| Kind | Functions |
| --- | --- |
| `sdb.KindSource` | `Next(e) (*sdb.Batch, error)` until nil · [`Open`, `Close`] |
| `sdb.KindTransform` | `Apply(e, b) (*sdb.Batch, error)` · [`Open`, `Flush`, `Close`] |
| `sdb.KindSink` | `Write(e, b) error` · [`Open(e, cols)`, `Commit(e) (sdb.Stats, error)`, `Abort`] |
| `sdb.KindAction` | `Run(e) error` or `Run(e) (sdb.Stats, error)` (its `Stats.Vars` become `${frag.<f>.<name>}`) |
| any | [`Check(cfg sdb.Cfg) []string`, which validates settings at check time] |

Inside those functions, `e.Cfg` holds the node's settings with `${…}`
substituted and defaults filled in. `e.S` is the session (`e.S.Query`), and
`e.Logf` writes to the run's log. `e.Path(p)` resolves a file path the way
the built-ins do. Four examples, one per kind, ship in the binary. Copy one
from the `Ctrl+O` browser's Plugins section.

---

## 8. Jobs: pipelines in a dependency graph

A **job** runs pipelines as a DAG with one root. A step starts when every
step it waits for (`after`) has succeeded. Jobs can run by hand, on a
**cron schedule** (while `dbc web` is running), from a **webhook**, from
the system crontab via `dbc job run`, or from a script.

First, two small pipelines for the job to run. Note that `script.run`
turns the script from §5 into a fragment, so every saved script can be a
pipeline step.

`pipelines/warehouse-facts.json`:

```json
{
  "name": "warehouse-facts",
  "desc": "Rebuild the warehouse's customers and order_facts with the load_warehouse script.",
  "fragments": [
    { "name": "load", "nodes": [ { "id": "run", "plugin": "script.run", "cfg": { "name": "load_warehouse" } } ] }
  ]
}
```

`pipelines/revenue-report.json`:

```json
{
  "name": "revenue-report",
  "desc": "Paid revenue per region from the warehouse, to a dated CSV.",
  "fragments": [
    {
      "name": "revenue",
      "nodes": [
        { "id": "src",  "plugin": "sql.read",  "cfg": { "conn": "warehouse",
            "query": "SELECT region, count(*) AS orders, sum(total) AS revenue FROM order_facts WHERE paid GROUP BY region ORDER BY region" } },
        { "id": "csv",  "plugin": "csv.write", "cfg": { "path": "exports/revenue_${run.date}.csv" } },
        { "id": "peek", "plugin": "preview",   "cfg": { "rows": "20", "title": "Revenue by region" } }
      ],
      "edges": [ ["src", "csv"], ["src", "peek"] ]
    }
  ]
}
```

Now the job, `jobs/nightly-shop.json`:

```json
{
  "name": "nightly-shop",
  "desc": "Rebuild the warehouse, then the export and the revenue report side by side.",
  "root": "facts",
  "params": { "min_total": { "default": "20", "doc": "Passed to shop-sync" } },
  "pipelines": [
    { "id": "facts",   "pipeline": "warehouse-facts" },
    { "id": "sync",    "pipeline": "shop-sync",      "after": ["facts"], "params": { "min_total": "${min_total}" } },
    { "id": "revenue", "pipeline": "revenue-report", "after": ["facts"] }
  ],
  "triggers": { "schedule": ["30 2 * * *"], "tz": "America/Chicago", "catch_up": false },
  "policy": { "on_failure": "finish_branches", "max_parallel": 2, "overlap": "skip", "timeout": "30m" }
}
```

```
            facts              facts first; then sync and revenue
           ╱     ╲             side by side (max_parallel 2)
       sync       revenue
```

| Setting | Meaning |
| --- | --- |
| `pipelines[].after` | The steps this one waits for. Fan-out and fan-in follow from it |
| `pipelines[].params` | The pipeline's params. These may use the job's params (`${min_total}`) and `${run.*}` |
| `triggers.schedule` | Cron lines (`m h dom mon dow`, plus `@daily` etc.) in `tz`. Only a running `dbc web` fires them |
| `triggers.catch_up` | Run a fire that was missed while dbc web was down, once |
| `triggers.webhook` | Opens `POST /api/v1/jobs/<name>.json/run` (Bearer secret) |
| `policy.on_failure` | `finish_branches` (default: skip what's downstream of a failure, let the other branches finish) or `stop` |
| `policy.overlap` | `skip` (default) or `queue` a start that arrives while the job is already running |
| `policy.timeout` | Cancel the whole run after this long |

```sh
dbc job check nightly-shop       # the DAG, the cron lines, and every pipeline it runs
dbc jobs                         # schedule, next fire, last run
```

```text
$ dbc jobs
name               schedule                      next              last run  pipelines  description
nightly-shop.json  30 2 * * * (America/Chicago)  2026-10-11 02:30            3          Rebuild the warehouse, then …
nightly.json       0 2 * * *                                                 4          The demo pipelines as a DAG …
```

Run it here and wait. Log lines carry their step, and the run's tree comes
last:

```text
$ dbc job run nightly-shop -p min_total=100
job nightly-shop: 3 pipelines from facts, 2 at a time (cli)
[facts] ▶ facts: pipeline warehouse-facts
[facts] fragment load: script.run
[facts] copied 4 rows shop:customers → warehouse:customers in 3ms (row by row)
[facts] order_facts: 6 rows
[facts] pipeline warehouse-facts: 1 fragment, 0 rows in 10ms (succeeded)
[revenue] ▶ revenue: pipeline revenue-report
[sync] ▶ sync: pipeline shop-sync
...
job nightly-shop succeeded in 24ms: facts ✓ 0 rows, sync ✓ 7 rows, revenue ✓ 2 rows
run 20261011-002445-aa85 · job nightly-shop · cli · ✓ succeeded · 24ms
  params: min_total=100
  ✓ facts   warehouse-facts     0 rows     10ms
      ✓ load           0 rows     10ms   run 0→0
  ✓ sync    shop-sync           7 rows     12ms   after facts
      ✓ customers      4 rows      5ms   src 0→4 · tidy 4→4 · fill 4→4 · dst 4→4
      ✓ gate           0 rows       0s   check 0→0
      ✓ big_orders     2 rows      4ms   src 0→2 · paid 2→2 · who 2→2 · stamp 2→2 · csv 2→2 · peek 2→2
          ◎ preview big_orders/peek: 2 rows kept of 2
      ✓ log            1 rows      3ms   log 0→1
  ✓ revenue revenue-report      2 rows      3ms   after facts
      ✓ revenue        2 rows      3ms   src 0→2 · csv 2→2 · peek 2→2
          ◎ preview Revenue by region: 2 rows kept of 2
```

Ways to trigger it:

```sh
# Without dbc web: the system crontab
30 2 * * * cd ~/dbc-tutorial && /usr/local/bin/dbc job run nightly-shop

# Fire and forget (CI, another scheduler): prints the run id and returns
dbc job run nightly-shop --wait=false

# Stop a run, from this machine or a running dbc web's
dbc run cancel 20261011-002445-aa85
```

Exit codes are `0` succeeded, `1` failed and `130` interrupted. An
interrupted run rolls back every load in flight first.

---

## 9. Orchestrating from a script

A script can do everything above, with Go control flow around it.

`scripts/orchestrate.go`:

```go
//go:build ignore

// Run a saved pipeline, then an ad-hoc one built in Go.
package main

import (
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	// A saved pipeline by name, with params.
	st, err := s.RunPipelineNamed("shop-sync", sdb.PipelineOpts{Params: sdb.Params{"min_total": "100"}})
	if err != nil {
		return err
	}
	s.Print("shop-sync: %s, %d rows", st.Status, st.Rows())

	// A pipeline built in code; ThenFunc is a transform written as a Go func.
	p := sdb.NewPipeline("emails")
	f := p.Fragment("domains")
	f.Node("sql.read", sdb.Cfg{"conn": "warehouse", "query": "SELECT id, email FROM dim_customer ORDER BY id"})
	f.ThenFunc(func(b *sdb.Batch) (*sdb.Batch, error) {
		b.AddCol("domain", "text", func(row int) any {
			e, _ := b.Get(row, "email").(string)
			_, d, _ := strings.Cut(e, "@")
			return d
		})
		return b, nil
	})
	f.Then("preview", sdb.Cfg{"rows": "10"})
	st, err = s.RunPipeline(p, sdb.PipelineOpts{})
	if err != nil {
		return err
	}
	s.Print("emails: %s", st.Status)

	// A whole job, waited for.
	run, err := s.RunJob("nightly-shop", sdb.Params{"min_total": "50"})
	if err != nil {
		return err
	}
	for _, p := range run.Pipelines {
		s.Print("  step %-8s %-10s %d rows", p.ID, p.Status, p.Rows())
	}
	s.Print("job %s: %s", run.Job, run.Status)
	return nil
}
```

- `ThenFunc` / `Func` nodes are the one thing JSON can't express: a
  closure over the script's own variables. A pipeline that contains one
  can't be exported back to JSON.
- `sdb.Batch` is `Cols` + `Rows [][]any`, with helpers `Col`, `Get`, `Set`,
  `AddCol`, `Drop`, `Keep`, `Rename`, `Filter` and `Clone`.
- The preview sinks of `RunPipeline` and `RunJob` show their rows as the
  script's own `s.Show` results.
- `dbc pipeline export NAME` (§6) is the fastest way to start such a
  script from an existing pipeline.

---

## 10. Run records

Every pipeline or job run, from any process (a shell, cron, dbc web's
scheduler, the TUI), leaves a JSON record in `runs_dir`. dbc reads them
back as a list, as a tree, or as SQL tables:

```text
$ dbc runs --limit 5
id                    kind      name          trigger  started              took  status     rows
20261011-002445-aa85  job       nightly-shop  cli      2026-10-11 00:24:45  24ms  succeeded  9
20261011-002430-acb1  pipeline  shop-sync     cli      2026-10-11 00:24:30  10ms  succeeded  4
20261011-002422-1c9c  pipeline  shop-sync     cli      2026-10-11 00:24:22  24ms  succeeded  8

$ dbc runs --sql "SELECT step, status, seconds, rows FROM pipelines ORDER BY seconds DESC LIMIT 5"
3 run(s) loaded as tables runs, pipelines, fragments, nodes
step       status     seconds   rows
shop-sync  succeeded  0.022985  8
sync       succeeded  0.012196  7
...
```

```sh
dbc run show 20261011-002445-aa85                   # one run as a tree
dbc runs --job nightly-shop --status failed --since 7d
dbc runs --sql "SELECT plugin, avg(seconds) FROM nodes GROUP BY plugin" -t json
```

The four tables are `runs`, `pipelines`, `fragments` and `nodes`. Each has
`status`, times, `seconds` and row counts, so "which node got slow this
week?" is a single query.

---

## 11. The same things in the UIs

Everything above is plain files, so the TUI and the browser workbench see
it immediately:

| | TUI (`dbc`) | Browser (`dbc web`) |
| --- | --- | --- |
| Scripts browser | `Ctrl+O`: Enter runs, `e` edits in `$EDITOR` (checked when the editor exits) | `Ctrl+O`: a Go tab with completion, live checking, `Ctrl+S` |
| Pipelines and jobs | `Ctrl+J`: run, edit JSON, live run monitor | Pipeline/job **canvases**: drag nodes, inspector forms, ▶ Run |
| Runs | `Alt+J` | `Alt+R`: DAG → fragments on a time axis → node counters → log |
| Schedules | — | Cron triggers fire while dbc web runs |

A script's `s.Show` results land in the results grid, and `s.Print` lines
go to the connection's log. A pipeline's preview sinks land in the grid of
the tab that started the run.

---

## 12. Pitfalls and quick fixes

| Symptom | Cause | Fix |
| --- | --- | --- |
| A map is missing values you assigned | yaegi drops `m[k], _ = v.(T)` (and any two-value assignment into a map element) | `v, _ := x.(T); m[k] = v`. `--check` warns |
| A `go.transform` returns no rows | yaegi mis-stores an operator's result put straight into a row: `b.Rows[i][c] = s + "!"` | `v := s + "!"; b.Rows[i][c] = v`. Calls like `strings.ToLower(s)` are fine |
| `os.Getenv` is empty in a script run from `dbc web` | The script sees `dbc web`'s environment, from when it started, not your terminal's | Set the variable before starting `dbc web`, or use constants, a settings file via `s.Path`, or pipeline params |
| A file isn't where you expected | Relative paths resolve to `files_dir` (default `$HOME`), not the cwd | `s.Path(...)` in scripts; `$PWD/...` in `-p` |
| Preview fails at `lookup`/`sql.read` of a table an earlier fragment makes (the error's hint names the fragments that would have written) | Previews write nothing, earlier fragments included | Run once for real, or `--fragment` |
| bytdb refuses a copy | bytdb tables need a primary key | Create the destination first, with a PK |
| A `Query` result is cut off | `s.Query` is capped at `max_rows` | `s.Reader` streams uncapped |
| Wrong placeholder errors | `?` vs `$1` | `?` for SQLite/MySQL; `$1` for Postgres/bytdb |
| Third-party import fails | Scripts are interpreted: stdlib + `sdb` only | Write a plugin, or do the work in SQL |

---

## Where next

- `scripts/` in this repo holds the built-in examples: `loop_params.go`,
  `sweep_conns.go` (the same query on every connection),
  `export_report.go`, `copy_table.go` and `plan_check.go` (fail a CI job on
  a bad query plan).
- `scripts/pipelines/` and `scripts/jobs/` hold the example pipelines and
  job, which run on the built-in demo databases. Try `dbc pipeline run
  clean-and-load` in an empty directory.
- `scripts/plugins/` has one example plugin of each kind.
- The README sections named at the top of this page are the full
  reference.
