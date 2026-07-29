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
| `Ctrl+R` | Run the editor's SQL on the active connection |
| `Ctrl+E` | Export the last result (format + clipboard/file dialog) |
| `Ctrl+O` | Pick and run a Go script from `scripts_dir` |
| `Ctrl+L` | Jump to the connections list (`Enter` activates one) |
| `Tab` / `Shift+Tab` | Cycle focus: editor → results → connections |
| `Esc` | Close a dialog |
| `Ctrl+Q` / `Ctrl+C` | Quit |

Non-SELECT statements (INSERT/UPDATE/DDL…) run as exec and report rows
affected. Results are truncated at `max_rows`.

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
./dbc script scripts/loop_params.go              # run a Go script
```

Flags go before the SQL: `-config path`, `-c connection`, `-f format`, `-o outfile`.

## Export formats

`csv`, `tsv`, `markdown`, `html` (styled standalone page), `json`
(array of objects), `text` (aligned table) — from the `Ctrl+E` dialog
(clipboard or file), from scripts via `s.Export`, or headless via `-f`.
