---
name: dbc
description: "Drive dbc — this repo's TUI database client for Postgres, MySQL, SQLite and bytdb — from the shell: run SQL headless with machine-readable output, run Go scripts (sdb.S), copy tables between connections, explain plans, draw ERDs, and apply goose-format migrations. Also how to build dbc, run its unit and opt-in test suites, and check a change in the real binary. Use when asked to query a database with dbc, write or run a dbc script, copy/ETL a table, explain a slow query, produce a schema diagram, run migrations, or verify a dbc change end to end."
---

# dbc

`dbc` is a terminal database client (Postgres, MySQL, SQLite, bytdb) that is
scripted in Go. With no SQL it opens the TUI; with SQL (argument, `-f FILE`,
or piped stdin) or a subcommand it runs **headless** and exits. Headless is
the mode to use from a shell or an agent — the TUI needs a human or the
`tui/e2e` harness.

The README is the full manual; this skill is the working subset. Section
names below are README headings.

## Before running anything

1. **Use a fresh build, not `./dbc`.** The checked-in-tree binary is often
   stale. Build to a scratch path:

   ```sh
   go build -o "$SCRATCH/dbc" .
   ```

2. **Strip `CATS_*` from the environment.** Sessions run inside a live Cats
   pane; a dbc launched with those vars talks to the host's control socket
   (theme, pane labels). Unset them for every launched binary and every test:

   ```sh
   for v in $(env | grep -o '^CATS_[A-Z_]*'); do unset $v; done
   ```

3. **Isolate when experimenting.** `HOME=$SCRATCH/h` keeps the run off the
   user's `~/.config/dbc/` (config, `connections.toml`, history) and puts the
   demo database in `$HOME/Library/Caches/dbc/`. Run from a scratch cwd too,
   so the repo's own `./dbc.toml` (if any) is not picked up.

4. **Ask before writing to a real connection.** Anything other than a
   scratch SQLite/bytdb file or the demo is someone's data. Reads are fine;
   `INSERT`/`UPDATE`/`DELETE`/DDL, `dbc copy --truncate`, and
   `migrate up/down` against a configured server need the user's go-ahead.

## Which database a run uses

Config resolution: `--config FILE`, else `./dbc.toml`, else
`~/.config/dbc/config.toml`. Connections added in the TUI or `dbc web` live
in `~/.config/dbc/connections.toml` and are usable by name too.

- `-c NAME` picks a connection; otherwise `default_connection`.
- `--driver D --dsn S` is a one-off connection in no config file
  (`--driver sqlite --dsn file:/tmp/x.db`). Its name in banners is `dsn`.
- With no config at all, dbc falls back to two built-in demos —
  `demo-bytdb` (default) and `demo-sqlite` — each with a `cats` table
  (`id, name, breed, age, adopted`). `--demo sqlite` or `DBC_DEMO` picks
  which is active.
- On Postgres/MySQL, `NAME/otherdb` reaches another database on the same
  server with the connection's credentials.
- `${VAR}` in a DSN is expanded from the environment; an unset var warns by
  name.

Placeholders follow the driver: `$1` for postgres/bytdb, `?` for
mysql/sqlite.

## Headless SQL

```sh
dbc -c NAME -t json "SELECT …"            # parse this, don't scrape text
dbc -c NAME -t csv -o out.csv "SELECT …"  # straight to a file
dbc -c NAME -f report.sql                 # SQL from a file (- = stdin)
dbc -c NAME --tx -f backfill.sql          # all-or-nothing
dbc -c NAME -k -f many.sql                # continue past failures, exit 1 at end
dbc -- "-- leading comment\nSELECT 1"     # -- before SQL starting with a comment
```

Formats (`-t`): `text` (default), `csv`, `tsv`, `markdown`, `html`, `json`.
Prefer `json` for reading results programmatically: one statement gives an
array of row objects; several give an array of envelopes
`{"statement","conn","duration","columns","rows"}` (or `"rows_affected"`).
Duplicate column names become `a`, `a_2`.

Rules that trip people up:

- The SQL is **one quoted argument**. Two positional args is an error, not a
  truncated query.
- `-f` is the SQL file. The output format is `-t` (`dbc -f csv` errors out
  with a hint).
- Several statements run in order on **one pinned session**: `BEGIN … COMMIT`,
  `SET`, temp tables carry across. Nothing is wrapped in a transaction unless
  `--tx`, which refuses SQL that manages its own transaction and doesn't mix
  with `-k`. MySQL DDL commits implicitly — keep it out of `--tx`.
- A run stops at the first failing statement (earlier results are still
  written); the error names it (`statement=2/3`).
- Results are capped at `max_rows` (config, default 1000); a capped result is
  noted on **stderr**, so check stderr before trusting a count.
- Errors are logged on stderr as structured lines (`level=error msg=… error=…`).
  Postgres `NOTICE`s also go to stderr.

Exit codes: `0` ok, `1` failure, `2` bad usage (incl. unknown connection),
`3` `explain --fail-on` tripped, `130` Ctrl+C.

## Subcommands

| Command | Use | README section |
| --- | --- | --- |
| `dbc script FILE.go\|NAME` | run a Go script headless (`-t`/`-o` apply); a bare NAME is looked up in `scripts_dir` | Scripts headless |
| `dbc script --check NAME\|FILE…` | check scripts without running them (parse, `Run`'s signature, yaegi compile, the map-assign lint); exit 1 on an error, `-t json` for JSON | Scripts headless |
| `dbc scripts [-t json]` | list `scripts_dir` (default `~/.config/dbc/scripts`); the directory goes to stderr | Scripts headless |
| `dbc copy --from A --to B [--create] [--truncate] [--where C] SRC [DST]` | copy a table across connections/engines in one transaction | Copy headless |
| `dbc explain [-a] [-t text\|json\|markdown\|html\|pdf\|png\|jpeg\|mermaid] [--fail-on warn\|crit] "SQL"` | plan + findings; one statement | Explain headless |
| `dbc erd [-t mermaid\|markdown\|png\|jpeg] [--table T] [--depth N] [--views]` | schema diagram | Diagrams headless |
| `dbc migrate status\|version\|up\|up-by-one\|up-to V\|down\|down-to V\|redo\|create NAME` | goose-compatible migrations; `--dir`, `--allow-missing` | Migrations |
| `dbc web [--no-open] [--listen ADDR]` | browser workbench on 127.0.0.1:8450 | The browser workbench |
| `dbc version` | version | Build |

Binary outputs (`pdf`, `png`, `jpeg`) need `-o` or a pipe — dbc refuses to
print them on a terminal. `explain -t json | jq .insights` is the
tool-friendly form. `copy` refuses `-c`, `-f`, `--tx`, `-k`, `-t`, `-o`.

## Go scripts

A script is a `.go` file with `//go:build ignore`, `package main`, and
`func Run(s *sdb.S) error`, interpreted by yaegi (no compile step, full
stdlib). Run it with `dbc [-c …] [-t FMT] [-o FILE] script path.go`.
`s.Print` is progress (moves to stderr when `-t` is machine-readable);
`s.Show(r)` is data.

See [scripting.md](scripting.md) for the `sdb.S` API, ETL
(`Copy`/`Reader`/`Writer`), and the yaegi map-assignment bug that silently
drops writes. Samples to copy from live in `scripts/`:
`loop_params.go`, `sweep_conns.go`, `export_report.go`, `copy_table.go`,
`plan_check.go`.

## Working on dbc itself

```sh
go build ./... && go vet ./... && go test ./...   # unit tests; strip CATS_* first
```

Opt-in suites (README "Tests"):

- **Live DBs** — `db/live_*`: set `DBC_LIVE_PG_DSN` / `DBC_LIVE_MYSQL_DSN` to
  throwaway servers, e.g. `docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=pw postgres`.
- **Web e2e** — `cd web/e2e && DBC_E2E=1 go test -count=1 -v .` (go-rod,
  headless Chrome; its own module). `DBC_E2E_HEADFUL=1` to watch.
- **TUI e2e** — `cd tui/e2e && DBC_TUI_E2E=1 go test -count=1 -v .`
  (pseudo-terminal + VT emulator; its own module).

To show a change working, prefer a headless run of a fresh build against the
demo or a scratch SQLite/bytdb file (see "Before running anything"); use the
e2e suites when the change is in the TUI or the web page.

Layout, for orientation: `main.go` (CLI, headless runner), `copycmd.go`,
`explain.go`, `erdcmd.go`, `migrate.go`, `webcmd.go` at the root; `db/`
(connection manager, drivers), `sdb/` + `script/` (script API and yaegi
host), `etl/` (copy engine), `explain/`, `erd/`, `tui/`, `web/`, `config/`,
`export/`, `sqlsplit/` (statement splitter shared by editor, headless and
migrations), `sqlcomplete/`.

Releasing is in the README "Releasing" section — don't push `release` or tags
without being asked.
