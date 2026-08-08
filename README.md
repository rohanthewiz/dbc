# dbc — a TUI database client, scriptable in Go

`dbc` is a terminal database client for Postgres, MySQL, SQLite, and
[bytdb](https://github.com/rohanthewiz/bytdb) with a twist: instead of a
bespoke macro language, you script it in **Go**. Drop a
`.go` file in the scripts directory, loop over parameters, run queries against
any configured connection, and export the results — CSV, Markdown, HTML,
JSON — to a file or straight to the clipboard.

## Build

```sh
go build -o dbc .
```

## Configure connections

Copy `dbc.example.toml` to `./dbc.toml` (or `~/.config/dbc/config.toml`):

```toml
scripts_dir        = "scripts"
max_rows           = 1000   # rows fetched from the server
max_display_rows   = 2000   # rows the results table draws (0 = all)
default_connection = "local-pg"

[[connection]]
name   = "local-pg"
driver = "postgres"          # postgres | mysql | sqlite | bytdb
dsn    = "postgres://postgres:${PGPASS}@localhost:5432/mydb?sslmode=disable"
```

Two of the four drivers are embedded — there is no server to start, and the
DSN is just a file:

```toml
[[connection]]
name   = "scratch"
driver = "sqlite"
dsn    = "file:scratch.db"

[[connection]]
name   = "notes"
driver = "bytdb"
dsn    = "notes.bytdb"       # created on first open
```

[bytdb](https://github.com/rohanthewiz/bytdb) is an embedded relational store
over an ordered key-value engine, with a Postgres-flavored dialect (`$1`
placeholders, `pg_catalog` introspection) and serializable transactions. dbc
opens it in-process, so `Ctrl+T`, cancellation, exports, and scripts work
against it exactly as they do against the other three. Unlike SQLite it has no
in-memory mode, so the DSN is always a real path.

Env vars in DSNs are expanded, so secrets can stay out of the file. A DSN
referencing an unset var warns by name at startup (a typo'd `${PGPASS}` will
not silently become an empty password). Duplicate connection names and a
`default_connection` that names no connection are rejected at load.

With **no config at all**, dbc starts on two built-in connections, one per
embedded engine, each seeded with the same `cats` table — so you can try
everything immediately, and compare the two engines on the same query:

| Connection | Engine | Storage |
| --- | --- | --- |
| `demo-bytdb` | bytdb | `demo.bytdb` in the OS cache dir (`~/Library/Caches/dbc`, `~/.cache/dbc`) — persists between runs |
| `demo` | SQLite | in-memory, fresh every run |

`demo-bytdb` is the active one out of the box. `-demo sqlite` (or
`DBC_DEMO=sqlite`) makes `demo` active instead; either way both are in the
connections list, so `Ctrl+L` — or `-c demo` headless — switches between them.
The flag is ignored once a config file exists, since a config names its own
`default_connection`.

If the bytdb demo cannot be opened — another dbc already holds the file, or the
cache directory is not writable — it is dropped with a warning and dbc starts
on the SQLite demo rather than failing.

## The TUI

```sh
./dbc
```

Layout: connections sidebar · SQL editor · results table · log pane · status bar.

| Key | Action |
| --- | --- |
| `Ctrl+R` | Run the statement under the cursor (or the selection) |
| `Ctrl+K` | Stop the running query or script — same as the **■ Stop** button |
| `Ctrl+E` | Export the last result (format + clipboard/file dialog) |
| `Ctrl+O` | Pick and run a Go script from `scripts_dir` |
| `Ctrl+P` | Query history — filter, then `Enter` to drop one in the editor |
| `Ctrl+T` | List the tables and views on the active connection |
| `Ctrl+L` | Jump to the connections list (`Enter` activates one) |
| `y` / `Y` | *(results table)* Copy the selected cell / the whole row |
| `Tab` / `Shift+Tab` | Cycle focus: editor → results → connections |
| `Esc` | Close a dialog |
| `Ctrl+C` | Stop what's running; quit when idle |
| `Ctrl+Q` | Quit |

Non-SELECT statements (INSERT/UPDATE/DDL…) run as exec and report rows
affected. A real SQL `NULL` is drawn in the muted color, so it cannot be
confused with a column holding the string `"NULL"`.

Two caps, deliberately separate: `max_rows` is how many rows are fetched from
the server, `max_display_rows` how many of those the table draws. Drawing
costs a widget per value, so raising `max_rows` to 50k for an export would
otherwise make scrolling crawl. The rows past the display cap are still in the
result and still go into `Ctrl+E`; the status bar says how many are on screen.

`Ctrl+T` is the `\dt`: it runs the active driver's catalog query and drops the
tables and views into the results table as `table_schema · table_name ·
table_type` — the same three columns on all four drivers. It is an
ordinary query, so it is cancelable and exportable like any other, and the
editor buffer is left alone.

The results table selects by cell, so the arrow keys walk a wide result in
both directions. `y` copies the cell under the cursor to the system clipboard;
`Y` copies the whole row, tab-separated, which pastes into a spreadsheet as
cells. (`Ctrl+E` is still the way to export the whole result.)

The interface wears a muted green theme — dark gray-green surfaces with a
single green accent, shared with [cdx](https://github.com/rohanthewiz/cdx).
The accent is the focus cue: the pane holding the keys is the one whose
border and title are lit.

### Query history

Every statement you run is recorded. `Ctrl+P` opens the history newest first;
type to filter it — over the SQL and the connection name — `↑`/`↓` to pick,
and `Enter` drops the statement into the editor at the cursor. It is *not* run:
a recalled query can be edited first, which is usually why you went looking
for it.

Each statement of a multi-statement run is recorded on its own, so any one of
them is recallable. A statement identical to the one before it is not recorded
twice, and the app's own catalog query (`Ctrl+T`) never is.

History lives in `~/.config/dbc/history.jsonl` — one JSON object per line, so
multi-line SQL comes back exactly as it was written — capped at the last 500
entries and readable only by you. Delete the file to forget everything.

### Multi-statement buffers

The editor buffer persists across sessions: it is saved to
`~/.config/dbc/buffer.sql` on quit and restored at the next launch, so the
scratchpad is still there tomorrow.

Keep a whole scratchpad of SQL in the editor and run one statement at a time:
`Ctrl+R` executes only the statement the cursor sits in, and the log says
which one (`running statement 2/4 …`). Select a region first and `Ctrl+R`
runs exactly that instead — a selection holding several statements runs them
in order, stopping at the first failure, with the last result shown in the
table.

Statements are separated on semicolons, ignoring the ones inside strings,
quoted identifiers, comments, and PostgreSQL `$$` bodies — so a function
definition stays in one piece.

Editor runs share one pinned database session per connection, just like a
headless multi-statement buffer: `BEGIN` in one `Ctrl+R` and `COMMIT` in a
later one bracket a real transaction, and `SET`, `PRAGMA`, and temp tables
persist between runs. Switching connections releases the session — along with
any transaction it had open.

### Stopping a long query

While a query or script runs, the status bar shows a live elapsed time and a
**■ Stop** button appears at its right edge. `Ctrl+K` or a click on the button
cancels it: the statement is aborted on the server (Postgres, MySQL) or
interrupted in process (SQLite, bytdb), and the run is reported as stopped
rather than failed.

A canceled script unwinds through its own error handling — the query in flight
is aborted and every later `s.Query`/`s.Exec` fails immediately. A script that
loops without touching the database can still check `s.Canceled()`, and
`sdb.IsCanceled(err)` tells a stop apart from a real failure:

```go
r, err := s.Query("demo", bigQuery)
if err != nil {
	if sdb.IsCanceled(err) {
		return nil // stopped by the user, not a failure
	}
	return err
}
```

## Scripting in Go

A script is a plain Go file defining one function:

```go
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	for _, minAge := range []int{1, 3, 5} {
		r, err := s.Query("demo",
			"SELECT id, name, breed, age FROM cats WHERE age >= ? ORDER BY age", minAge)
		if err != nil {
			return err
		}
		s.Print("min age %d → %d cats", minAge, len(r.Rows))
		s.Show(r) // lands in the TUI results table
	}
	return nil
}
```

Scripts are interpreted at runtime (via yaegi) — no compile step, edit and
re-run. The full Go standard library is available. The `//go:build ignore`
line just keeps `go build` from compiling script files if they live inside a
Go module; dbc runs them regardless.

### The `sdb.S` API

| Method | Purpose |
| --- | --- |
| `s.Conns() []string` | Configured connection names |
| `s.Query(conn, sql, args...) (*sdb.Result, error)` | Run a query with params |
| `s.Exec(conn, sql, args...) (int64, error)` | Run a statement, get rows affected |
| `s.DB(conn) (*sql.DB, error)` | Raw `database/sql` handle — transactions, prepared stmts, anything |
| `s.Show(r)` | Push a result to the results table (stdout when headless) |
| `s.Print(format, args...)` | Log to the TUI log pane (stdout when headless) |
| `s.Export(r, format, path)` | Export a result; empty path → clipboard |
| `s.Canceled() bool` | True once the user has stopped this run |
| `s.Ctx() context.Context` | The run's context, for `select` on `Done()` |
| `sdb.IsCanceled(err) bool` | Tells a stop apart from a real query failure |

`sdb.Result` gives you `Columns []string`, `Rows [][]string`, `Raw [][]any`,
`Duration`, and `Affected`. Use the placeholder style of the target driver
(`$1` postgres/bytdb, `?` mysql/sqlite).

Sample scripts live in [`scripts/`](scripts/): parameter loops, multi-host
sweeps, and CSV/HTML report generation.

## Headless mode

Everything works without the TUI, for cron jobs and shell pipelines:

```sh
./dbc "SELECT * FROM cats"                       # aligned text table
./dbc -c local-pg -f csv "SELECT * FROM users"   # any format to stdout
./dbc -f html -o report.html "SELECT ..."        # straight to a file
./dbc "INSERT ...; SELECT ..."                   # several statements, in order
./dbc script scripts/loop_params.go              # run a Go script
```

Flags go before the SQL: `-config path`, `-c connection`, `-f format`,
`-o outfile`, `-demo bytdb|sqlite`.
Use `--` before SQL that starts with a comment, so the flag parser leaves it
alone. `Ctrl+C` cancels a running query or script and exits 130.

### Scripts headless

`-f` and `-o` apply to scripts too:

```sh
./dbc -f json script scripts/loop_params.go        # one JSON array on stdout
./dbc -f csv -o report.csv script scripts/loop.go  # straight to a file
```

The results a script pushes with `s.Show` are collected and rendered together
when it finishes, exactly as the statements of a multi-statement query are —
so `-f json` yields one array rather than a run of separate documents.

`s.Print` output is progress, not data, and streams as it happens. It shares
stdout with a `text` table, but moves to stderr when a machine-readable format
has stdout to itself, so `./dbc -f json script … | jq` parses.

### Multi-statement runs

The SQL argument may hold several statements, split the same way the editor
splits them — semicolons inside strings, comments, and `$$` bodies don't count.
They run in order on one connection, and every result is rendered:

```sh
./dbc "INSERT INTO cats (name, breed, age) VALUES ('Zed', 'Tabby', 4);
       SELECT breed, count(*) AS n FROM cats GROUP BY breed ORDER BY n DESC"
```

```
-- 1/2 │ demo │ 1 rows affected in 50µs
-- INSERT INTO cats (name, breed, age) VALUES ('Zed', 'Tabby', 4)
rows_affected
-------------
1

-- 2/2 │ demo │ 3 rows in 150µs
-- SELECT breed, count(*) AS n FROM cats GROUP BY breed ORDER BY n DESC
breed       n
----------  -
Tabby       3
Siamese     2
Maine Coon  2
```

A run stops at the first statement that fails — later statements are skipped —
but the results collected before it are still written, and the error names the
one that broke (`statement=2/3`). Exit status is 1 for a failure, 130 for a
Ctrl+C.

The statements share one pinned database session, so session-scoped SQL means
what it says across them: nothing is committed until you say so in

```sh
./dbc "BEGIN; UPDATE cats SET age = age + 1; SELECT * FROM cats; COMMIT"
```

and `SET`, `PRAGMA`, and temp tables set up by one statement are still there
for the next. Nothing is wrapped in a transaction for you — without a `BEGIN`
each statement commits on its own.

Each format keeps its own shape across statements:

| Format | Multi-statement output |
| --- | --- |
| `text`, `markdown` | a banner per statement (position, connection, rows, time) above each table |
| `csv`, `tsv` | blocks separated by a blank line, each with its own header row — no banners to break parsing |
| `html` | one page, a section per statement |
| `json` | an array of envelopes: `{"statement", "conn", "duration", "columns", "rows"}`, or `"rows_affected"` for a non-query |

A single statement renders exactly as it always did, in every format. A
result that hit `max_rows` is noted on stderr, so a truncated export cannot
pass for the full set while the data stream stays clean. In `json`, duplicate
column names are suffixed (`a`, `a_2`) rather than silently collapsed.

## Export formats

`csv`, `tsv`, `markdown`, `html` (styled standalone page), `json`
(array of objects), `text` (aligned table) — from the `Ctrl+E` dialog
(clipboard or file), from scripts via `s.Export`, or headless via `-f`.

The HTML page wears the same muted green as the TUI, surface for surface.
Both read the palette from [`theme/`](theme/theme.go), so a change to those
constants reaches the app and its exports together.
