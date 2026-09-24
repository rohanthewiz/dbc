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
./dbc                 # the Bubble Tea UI: mouse-first, with the AI assistant
./dbc -ui classic     # the original tview UI (also DBC_UI=classic)
```

```
 dbc  ▶ Run  ■ Stop  ⧉ Copy ▾  ⤓ Export  ⌕ History  ƒ Scripts  ▦ Tables  ✦ Ask     ● conn ▾
╭ Connections ╮╭ Query ─────────────────────────────╮╭ ✦ Copilot · model ▾ ─ ⟲ new ✕ ╮
│● local-pg   ││ 1  SELECT …                         ││ ❯ why is this slow?           │
╰─────────────╯╰─────────────────────────────────────╯│ …                             │
╭ Tables · 12 ╮╭ Results · 8 rows · 1.2ms ───────────╮│ ┌ sql ─── ⤓ insert  ⧉ copy ┐ │
│ cats        ││  │ id │ name     │ …                 ││ [✓] with: query, 10 rows      │
│ owners      │╰─────────────────────────────────────╯│ ask about the query…   ⏎ send │
╰─────────────╯╭ Log ───────────────────────────────╮╰───────────────────────────────╯
 ● conn │ 8 rows in 1.2ms                                ^R run  ^K stop  ^A ask  ^E export
```

A toolbar, a connections and tables sidebar, the SQL editor, the results
grid, a log, and — when you open it — the AI assistant. Everything has a
mouse gesture and every gesture has a key.

### Mouse

| Where | Gesture | Does |
| --- | --- | --- |
| anywhere | click | focuses that pane — the keyboard follows the mouse |
| toolbar | click | Run, Stop, Copy ▾, Export, History, Scripts, Tables, Assistant; `● conn ▾` switches connection |
| connections | click | connects |
| tables | click / double-click / right-click | select / preview the first 100 rows / insert name, copy name |
| editor | click, drag, double-, triple-click | caret, selection, word, line |
| editor | right-click | run, copy, cut, select all, undo, history, ask the assistant |
| results | click, drag, shift-click | cell, rectangular range, extend |
| results | row number click | selects the row |
| results | header click | sorts by that column (again: descending; again: result order) |
| results | double-click | inspects the value in full (JSON is pretty-printed) |
| results | right-click | copy cell / row / range / whole result as **HTML table**, Markdown, CSV, TSV, JSON; sort; export; ask the assistant |
| any pane | wheel, shift+wheel | scrolls what is under the pointer, vertically / sideways |
| scrollbars | click, drag | jump, drag |
| pane borders | drag | resize the sidebar, the editor/results split, the log, the assistant |
| assistant | `⤓ insert` on a code block | puts that SQL in the editor at the caret |

Mouse reporting takes the terminal's own text selection away, so every pane
that shows text has its own copy. To select raw terminal text anyway, hold
**Shift** (most terminals) or **⌥ Option** (iTerm2, Terminal.app) while
dragging.

### Keys

| Key | Action |
| --- | --- |
| `Ctrl+R` | Run the statement under the caret (or the selection); the gutter marks which |
| `Ctrl+K` | Stop the running query or script — or the assistant's answer |
| `Ctrl+A` | Open the assistant / move between it and the editor |
| `Ctrl+E` | Export the result (format picker; file, or clipboard) |
| `Ctrl+O` | Pick and run a Go script from `scripts_dir` |
| `Ctrl+P` | Query history — filter, then `Enter` inserts (never runs) |
| `Ctrl+T` | List the tables and views on the active connection |
| `Ctrl+L` | Jump to the connections list |
| `Ctrl+G` | *(inside Cats)* Hand the statement to an agent in another pane |
| `y` / `Y` / `c` | *(results)* Copy the cell or range / the row / open the copy menu |
| `Enter` | *(results)* Inspect the value under the cursor |
| `Tab` / `Shift+Tab` | Cycle focus through the panes |
| `Esc` | Close a dialog or menu |
| `Ctrl+C` | Stop what's running; quit when idle |
| `Ctrl+Q` | Quit |

The editor is a code editor, not a text box: it keeps the indent on
`Enter`, highlights SQL (keywords, strings, numbers, comments, parameters —
from the same scanner that splits statements, so what looks like a string is
one), and undoes typing a word at a time (`Ctrl+Z`, redo `Alt+Z`). Long lines
scroll sideways rather than wrap, so a click lands exactly where you point.

In terminals that deliver the kitty keyboard protocol, `⌘E`, `⌘P`, and
`⌘G` are equivalents for export, history, and handing to an agent.

### Copying results — including into Teams

Right-click a result (or `⧉ Copy ▾`) and pick **Table for Teams / Outlook /
Docs (HTML)**: dbc puts a real HTML table on the clipboard, with inline
styles, so it pastes into Teams, Outlook, Slack or Google Docs as a formatted
table rather than as markup. Markdown, CSV, TSV and JSON are one row down.
The scope is the selected range when there is one, or the whole result.

The rich copy needs the local clipboard (macOS, Windows, or Linux with
`wl-copy`/`xclip`). Over SSH dbc falls back to the terminal's clipboard
protocol, which carries plain text only, and says so in the log.

### AI assistant

`✦ Ask` (or `Ctrl+A`) opens a chat about the query in the editor and the
result in the grid. It runs GitHub Copilot by default through its official
language server, speaking the Agent Client Protocol, so dbc holds no
credential: sign in to Copilot once from any editor that uses the language
server (ced, VS Code, Neovim) and dbc uses the same sign-in.

```sh
npm install -g @github/copilot-language-server   # if you do not have it
```

**What is sent.** Each question carries the statement under the caret and,
if its last run failed, the error. It also carries the **schema** of the
tables involved: every table the statement or the question names (up to
eight) is described by its column names and declared types, read from the
database's catalog when you press Enter, so the SQL that comes back uses
your real columns. Schema is the table's shape, not its contents, so it goes
on every connection. **Result rows are not sent unless the connection allows
it** — rows are your data:

```toml
ai_context_rows = 10        # cap on rows per question (0 = none)

[[connection]]
name    = "scratch"
ai_rows = true              # this connection may send result rows
```

Without `ai_rows` only the column names go. The chip above the input says
what the next question will carry (click it to send the question alone), and
the transcript records what each one did carry.

The assistant can answer but not act: dbc declines every request from the
agent to run a command or touch a file. SQL in an answer gets `⤓ insert`,
which puts it in the editor for you to read and run — there is deliberately
no run-it-for-me button. Click the title for the model picker (Copilot's
premium multiplier is shown beside each model) or to switch assistant:
`ai_agent = "claude"` uses Claude Code (`claude-code-acp`), `"gemini"` uses
the Gemini CLI.

### Everything else

Non-SELECT statements (INSERT/UPDATE/DDL…) run as exec and report rows
affected. A real SQL `NULL` is drawn muted and italic, so it cannot be
confused with a column holding the string `"NULL"`.

Queries run on a session pinned to the active connection, so `BEGIN` …
`COMMIT`, `SET`, and temp tables carry across runs as they do in psql.

`max_rows` is how many rows are fetched from the server; `max_display_rows`
how many of those the grid shows. The grid is virtualized, so drawing is cheap
either way, but the cap keeps both UIs showing the same thing; rows past it
are still in the result and still go into an export or a whole-result copy.

`Ctrl+T` is the `\dt`: it runs the active driver's catalog query and drops
the tables and views into the results as `table_schema · table_name ·
table_type` — the same three columns on all four drivers.

The editor buffer and the query history persist between sessions under
`~/.config/dbc`, shared by both UIs.

The interface wears a muted green theme — dark gray-green surfaces with a
single green accent, shared with [cdx](https://github.com/rohanthewiz/cdx).
The accent is the focus cue: the pane holding the keys is the one whose
border and title are lit.

### Cats integration

When dbc runs inside [Cats](https://github.com/rohanthewiz/cats), it detects
the host automatically; no dbc configuration is required. Its colors follow
the active Cats theme, including live theme changes. Outside Cats — or if the
host control socket is unavailable — dbc continues as the standalone client
described above.

Cats labels the dbc pane with its current state: **working** while a query or
script runs and **idle** when it finishes. That lets Cats show its normal
completion badge, toast, or notification for a long-running task.

`Ctrl+G` (or `⌘G`) gathers the selected SQL, or the statement under the
cursor, along with its connection, driver, and most recent error. Choose a
sibling agent pane to stage the question there and switch to it; dbc never
presses Enter on your behalf. The Cats chat panel is always available as an
alternative and receives the question immediately.

#### As a Cats plugin

dbc is also a Cats plugin of type `db_client`, declared in
[`cats-plugin.toml`](cats-plugin.toml):

```sh
catctl plugin install rohanthewiz/dbc   # or, from this checkout: catctl plugin link .
catctl plugin run rohanthewiz.dbc       # opens the TUI in a new tab
```

Installing builds `bin/dbc` and links it as `~/.cats/bin/dbc`, so `dbc`
typed in any shell is the same build the plugin launches. `catctl completion`
picks up dbc's subcommands and flags as well.

The plugin tab opens in your current directory, so a project's own
`./dbc.toml` is used when there is one, otherwise
`~/.config/dbc/config.toml`, otherwise the demo connections. The integration
described above works the same either way. Its declared type is what keeps
Cats from offering the dbc pane as a place to drop an agent prompt.

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
`-o outfile`, `-demo bytdb|sqlite`, and `-driver name -dsn string` for a
one-off connection that is in no config file.
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

## Migrations

`dbc migrate` applies versioned SQL migrations in the goose file format, with
goose's `goose_db_version` table — so a database goose has been migrating
carries straight over, and the goose binary stops being a thing to install.

```sh
./dbc -c local-pg migrate status                  # each file, applied or pending
./dbc -c local-pg migrate up                      # apply everything pending
./dbc -c local-pg migrate down                    # roll back the latest
./dbc -c local-pg migrate create add_users_table  # new timestamped file
./dbc -c local-pg -f json migrate version         # for scripts
```

Verbs: `status`, `version`, `up`, `up-by-one`, `up-to VERSION`, `down`,
`down-to VERSION`, `redo`, `create NAME`.

The migrations directory is `-dir`, else the connection's `migrations` setting
in the config, else the working directory:

```toml
[[connection]]
name       = "church-dev"
driver     = "postgres"
dsn        = "postgres://devuser:secret@localhost:5432/church_development?sslmode=disable"
migrations = "db/migrate"
```

With no config file at all, `-driver` and `-dsn` name the database on the
command line — goose's whole invocation, one flag longer:

```sh
./dbc -driver postgres -dsn "$DATABASE_URL" -dir db/migrate migrate up
```

A migration file is `NNN_name.sql` with `-- +goose Up` and `-- +goose Down`
sections. Statements are split by the same scanner the editor uses, so a
`$$`-quoted function body is one statement without help; the goose
`StatementBegin`/`StatementEnd` block and `NO TRANSACTION` annotations are
honored too. Each migration runs in a transaction with its version row, so a
failure leaves neither behind. `up` refuses a pending migration older than the
current version — the merge-of-two-branches case — unless `-allow-missing` is
given, exactly as goose does.

Works on all four engines. bytdb runs DDL outside transactions, so there each
statement commits on its own.

## Export formats

`csv`, `tsv`, `markdown`, `html` (styled standalone page), `json`
(array of objects), `text` (aligned table) — from the `Ctrl+E` dialog
(clipboard or file), from scripts via `s.Export`, or headless via `-f`.

To the **clipboard**, `html` is not the page but a self-styled `<table>`
offered as the clipboard's HTML flavor, so it pastes into Teams, Outlook and
Docs as a table (see *Copying results* above). That holds for a script's
`s.Export(r, "html", "")` too.

The HTML page wears the same muted green as the TUI, surface for surface.
Both read the palette from [`theme/`](theme/theme.go), so a change to those
constants reaches the app and its exports together.
