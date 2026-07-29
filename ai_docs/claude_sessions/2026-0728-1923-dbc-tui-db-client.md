# Session: dbc — TUI database client, scriptable in Go

Session id: `bfec305a-671c-4cb4-b61c-4e9faf6d0347`
Date: 2026-07-28

## Ask

Build a TUI database client in Go with:

- Regular query running
- Scripting in **Go** (replace params within a loop, etc.)
- Multiple DB host connections
- Copy-out of query results as CSV, HTML, Markdown, etc.

Mid-session direction: create it as a new sibling repo at `~/projs/go/dbc`.

## What was built

New repo `~/projs/go/dbc` (module `github.com/rohanthewiz/dbc`), built and
verified on go1.26.1 darwin/arm64.

### Architecture

| Package | Role |
| --- | --- |
| `model` | `Result` — columns, display rows, raw rows, duration, affected |
| `config` | TOML config (`dbc.toml` / `~/.config/dbc/config.toml`), env-var expansion in DSNs, built-in in-memory SQLite `demo` fallback when no config exists |
| `db` | lazy/pooled named connections (pgx, mysql, modernc sqlite — CGO-free), query-vs-exec detection, row-limit truncation, demo seeding (`cats` table) |
| `export` | csv, tsv, markdown, html (via `element`), json, aligned text; clipboard (atotto) or file |
| `sdb` | script API handle `S`: `Query/Exec/Conns/DB/Show/Print/Export` |
| `script` | yaegi engine: interprets script file, exposes `sdb` symbols + full stdlib, calls `main.Run`, recovers panics |
| `ui` | tview app: connections sidebar, SQL editor (TextArea), results table, log pane, status bar, export + script-picker modals |
| `main.go` | flag parsing; TUI by default; headless one-off query and `script` subcommand |

### Key decisions

- **tview** over bubbletea — batteries-included table/form/pages for a
  data-heavy client.
- **yaegi** for runtime Go scripting — no compile step; scripts are plain
  `.go` files defining `func Run(s *sdb.S) error`. Works fine on go1.26.
- Sample scripts carry `//go:build ignore` so `go build ./...` skips them
  (they are all `package main` with a duplicate `Run` symbol otherwise);
  yaegi's `EvalPath` ignores the tag — verified.
- `s.DB(conn)` exposes the raw `*sql.DB` as the escape hatch (transactions,
  prepared statements).
- Errors `serr`-wrapped throughout; `logger` at `warn` level so headless
  stdout stays pipe-clean (info chatter suppressed).
- In-memory shared-cache SQLite demo pins `SetMaxOpenConns(1)` so the DB
  survives for the process lifetime.

### TUI keys

`Ctrl+R` run · `Ctrl+E` export dialog (format → clipboard/file) · `Ctrl+O`
script picker · `Ctrl+L` connections · `Tab`/`Shift+Tab` cycle focus ·
`Esc` close dialog · `Ctrl+Q`/`Ctrl+C` quit.

### Headless

```sh
dbc "SELECT * FROM cats"              # aligned text
dbc -c local-pg -f csv "SELECT ..."   # any format to stdout
dbc -f html -o report.html "..."      # to file
dbc script scripts/loop_params.go     # run a Go script
```

## Verification (all passed)

- `go vet` + `gofmt` clean; deps tidy.
- Headless: text/csv/markdown/json/html renders; exec statements report
  rows affected; error path logs full serr context (function/location chains).
- Scripts via yaegi: `loop_params.go` (param loop → 3 result sets),
  `sweep_conns.go` (all-hosts sweep), `export_report.go` (per-breed CSVs +
  HTML report) — outputs checked on disk.
- TUI driven live under GNU `screen` (tmux absent): initial render, `Ctrl+R`
  query (8 rows in 410µs on status bar), `Ctrl+O` picker, script run inside
  the TUI (results landed in the table, prints in the log pane), `Ctrl+Q`
  clean exit. Note: old screen 4.00 hardcopy needs `-p 0` on detached
  sessions and mangles UTF-8 box borders in captures — cosmetic only.

## Loose ends / next ideas

- gopls flags the module when the editor workspace is rooted at another
  project — open `~/projs/go/dbc` as its own project (or add a `go.work`).
- Possible follow-ons: statement-under-cursor execution for multi-statement
  buffers, per-connection query history, result-cell inspection popup,
  cancelable long queries (context + Ctrl+K).
