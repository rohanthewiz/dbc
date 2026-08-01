# dbc — a TUI database client, scriptable in Go

`dbc` is a terminal database client for Postgres, MySQL, and SQLite with a
twist: instead of a bespoke macro language, you script it in **Go**. Drop a
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
max_rows           = 1000
default_connection = "local-pg"

[[connection]]
name   = "local-pg"
driver = "postgres"          # postgres | mysql | sqlite
dsn    = "postgres://postgres:${PGPASS}@localhost:5432/mydb?sslmode=disable"
```

Env vars in DSNs are expanded, so secrets can stay out of the file.

With **no config at all**, dbc starts with a built-in in-memory SQLite `demo`
connection seeded with a `cats` table — so you can try everything immediately.

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
| `Ctrl+L` | Jump to the connections list (`Enter` activates one) |
| `Tab` / `Shift+Tab` | Cycle focus: editor → results → connections |
| `Esc` | Close a dialog |
| `Ctrl+Q` / `Ctrl+C` | Quit |

Non-SELECT statements (INSERT/UPDATE/DDL…) run as exec and report rows
affected. Results are truncated at `max_rows`.

The interface wears a muted green theme — dark gray-green surfaces with a
single green accent, shared with [cdx](https://github.com/rohanthewiz/cdx).
The accent is the focus cue: the pane holding the keys is the one whose
border and title are lit.

### Multi-statement buffers

Keep a whole scratchpad of SQL in the editor and run one statement at a time:
`Ctrl+R` executes only the statement the cursor sits in, and the log says
which one (`running statement 2/4 …`). Select a region first and `Ctrl+R`
runs exactly that instead.

Statements are separated on semicolons, ignoring the ones inside strings,
quoted identifiers, comments, and PostgreSQL `$$` bodies — so a function
definition stays in one piece.

### Stopping a long query

While a query or script runs, the status bar shows a live elapsed time and a
**■ Stop** button appears at its right edge. `Ctrl+K` or a click on the button
cancels it: the statement is aborted on the server (Postgres, MySQL) or
interrupted (SQLite), and the run is reported as stopped rather than failed.

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
(`$1` postgres, `?` mysql/sqlite).

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

Flags go before the SQL: `-config path`, `-c connection`, `-f format`, `-o outfile`.
Use `--` before SQL that starts with a comment, so the flag parser leaves it
alone. `Ctrl+C` cancels a running query or script and exits 130.

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

A single statement renders exactly as it always did, in every format.

## Export formats

`csv`, `tsv`, `markdown`, `html` (styled standalone page), `json`
(array of objects), `text` (aligned table) — from the `Ctrl+E` dialog
(clipboard or file), from scripts via `s.Export`, or headless via `-f`.

The HTML page wears the same muted green as the TUI, surface for surface.
Both read the palette from [`theme/`](theme/theme.go), so a change to those
constants reaches the app and its exports together.
