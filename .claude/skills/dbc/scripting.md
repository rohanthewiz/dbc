# dbc scripts — reference

Full manual: README "Scripting in Go" and "ETL across connections". The
API surface is `sdb/sdb.go` and `sdb/etl.go`; read those when this page and
the code disagree.

## Shape

```go
//go:build ignore

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
dbc -t json script s.go | jq .  # one JSON array of every Show; Print → stderr
dbc -t csv -o out.csv script s.go
```

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
  an empty new table. MySQL `Truncate` is a `DELETE`.
- `Create` keeps exact types only Postgres → Postgres; otherwise columns get
  broad types and the source PK. No defaults, sequences, other indexes.
- bytdb needs a primary key (create the table first to copy a query into
  it) and has no exact decimal: `numeric` lands as `double precision`.
- `Reader` values: `int64`, `float64`, `bool`, `string`, `time.Time`,
  `[]byte`, `nil`; Postgres `numeric`/`uuid`/arrays as text. An unclosed
  Writer is rolled back when `Run` returns, never committed.

## yaegi pitfalls

- **Two-value assignment into a map element stores nothing.**
  `m[k], _ = v.(string)`, `m[k], _ = other[k]`, `m[k], _ = f()` silently
  drop the write; `m[k], _ = <-ch` panics (traefik/yaegi#1655). Assign to a
  local first:

  ```go
  v, _ := x.(string)
  m[k] = v
  ```

- A script is interpreted: keep it to stdlib + `sdb`. Third-party imports
  are not available.
