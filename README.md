# dbc — a TUI database client, scriptable in Go

`dbc` is a terminal database client for Postgres, MySQL, SQLite, and
[bytdb](https://github.com/rohanthewiz/bytdb) with a twist: instead of a
bespoke macro language, you script it in **Go**. Write a `.go` file — in
dbc's own script editor or yours — loop over parameters, run queries against
any configured connection, and export the results — CSV, Markdown, HTML,
JSON — to a file or straight to the clipboard. And when a query is slow,
**explain it**: the plan as a tree or a flame graph, the step that costs, and
what to do about it, on every engine — in the terminal or as an interactive
page in the browser. And when you would rather have a browser than a
terminal, `dbc web` serves the whole workbench there.

## Build

```sh
go build -o dbc .
```

Or install a tagged release with `go install github.com/rohanthewiz/dbc@latest`,
through Cats ([As a Cats plugin](#as-a-cats-plugin)), or from the archives on
the GitHub Releases page. `dbc version` (or `dbc --version`) prints the version.

### Tests

`go test ./...` runs the unit tests. Four opt-in suites need more than Go:

- **Live databases** (`db/live_*`): set `DBC_LIVE_PG_DSN` and/or
  `DBC_LIVE_MYSQL_DSN` to a throwaway Postgres or MySQL.
- **dbc web in a browser** (`web/e2e`, a module of its own so go-rod never
  enters dbc's dependencies): `cd web/e2e && DBC_E2E=1 go test -count=1 -v .`
  It builds dbc, serves `dbc web` from a temporary HOME on two SQLite files,
  and drives a headless Chrome through it: sign-in, a run, the Tables list,
  Show columns, copies, switching, Disconnect, Refresh, the connection form,
  the Postgres in Docker dialog (opened, never started), tabs across a
  reload, script tabs, and a pipeline built on the canvas by hand
  (dragged, wired, previewed, saved, run, stopped). A JavaScript error on
  the page fails it. `DBC_E2E_CHROME` names the browser,
  `DBC_E2E_HEADFUL=1` shows it, `DBC_E2E_SHOTS=dir` keeps the screenshots
  the pipeline step takes, and `DBC_LIVE_PG_DSN` adds the Postgres schema
  picker.
- **Postgres in Docker** (`pgdocker`): `DBC_LIVE_DOCKER=1 go test -run
  LiveDocker -v ./pgdocker`, with Docker running, starts a real container,
  connects through the connection it registers, restarts it, then removes
  the container and its volume. `DBC_LIVE_DOCKER_PG` picks the version
  (default: the newest). A `dbc-pg<version>` container that is already
  there makes it skip rather than touch it.
- **The TUI in a terminal** (`tui/e2e`, also a module of its own):
  `cd tui/e2e && DBC_TUI_E2E=1 go test -count=1 -v .` It builds dbc, runs
  the TUI in a 140×45 pseudo-terminal on two SQLite files, and reads the
  screen through a VT emulator while it types and clicks: a run,
  completion, a table preview by double-click, a sort by header click, a
  connection switch and quitting.

### Releasing

Fast-forward the `release` branch to main and push it
(`git push origin main:release`). The Release workflow runs the tests, bumps
the patch number in `version/version.go` and `cats-plugin.toml`, tags
`v<x.y.z>`, and publishes linux/darwin archives. For a minor or major release,
edit both files by hand in the commit you push and the workflow uses that
number as-is. Merge `release` back into main afterwards.

## Configure connections

Copy `dbc.example.toml` to `./dbc.toml` (or `~/.config/dbc/config.toml`):

```toml
# scripts_dir      = "scripts"   # default ~/.config/dbc/scripts; relative = beside this file
# jobs_dir         = "jobs"      # pipelines_dir, jobs_dir, runs_dir likewise (see Jobs)
# runs_keep        = 200         # run records kept per job and per pipeline
max_rows           = 1000   # rows fetched from the server
max_display_rows   = 2000   # rows the results table draws (0 = all)
result_tabs        = 10     # result tabs per connection, per query tab (1–50; see Result tabs)
conn_idle_timeout  = "1h"   # close pooled connections idle this long ("0" = never)
connect_timeout    = "5s"   # give up opening a connection after this ("0" = no limit; Ctrl+K cancels)
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

### TLS

Postgres and MySQL connections take TLS settings of their own, spelled the
same way for both engines:

```toml
[[connection]]
name     = "prod"
driver   = "postgres"           # or mysql
dsn      = "postgres://app:${PGPASS}@db.example.com:5432/app"
tls      = "verify-full"        # disable | prefer | require | verify-ca | verify-full
tls_ca   = "certs/ca.pem"       # CA to verify against (blank: the system's trusted CAs)
tls_cert = "~/certs/client.pem" # client certificate, if the server asks for one
tls_key  = "~/certs/client.key" # its private key (PEM)
tls_key_password = "${PGKEYPASS}" # only for an encrypted key: the env var holding its passphrase
```

The modes are libpq's `sslmode` names, and mean the same on both engines:

| `tls` | Encrypted | Server certificate checked |
| --- | --- | --- |
| `disable` | no | — |
| `prefer` | if the server offers TLS (else plaintext) | no |
| `require` | yes | no; with `tls_ca`, the chain (as `verify-ca`) |
| `verify-ca` | yes | signed by the CA; host name not checked |
| `verify-full` | yes | signed by the CA **and** issued for the host |

Only `verify-full` stops a man in the middle. Leave `tls` out and TLS is up to
the DSN, as before (`sslmode=…` for Postgres, `tls=…` for MySQL). Set, it wins
over the DSN: `tls = "verify-full"` beside a copied `?sslmode=disable` still
verifies. For MySQL this is the only way to name a CA file or a client
certificate: its DSN cannot. Paths may use `~` and `${VAR}`; a relative path
is relative to the config file. A misspelled mode, or a `tls_ca` with no
`tls`, is rejected at load rather than silently connecting without TLS. A
CA, certificate or key that cannot be read fails the connect and names the
file.

An encrypted client key needs `tls_key_password`, which is always an
environment variable reference (`"${PGKEYPASS}"` or `"$PGKEYPASS"`), never the
passphrase itself: a literal is rejected at load, so the passphrase stays out
of the config file, the saved-connections file and the browser. The variable
is read when the connection opens; unset, the connect fails and names it. Both
encodings OpenSSL writes are read, on both engines: legacy PEM encryption
(`Proc-Type: 4,ENCRYPTED`, from `openssl rsa -aes256 -traditional`) and
PKCS#8 (`ENCRYPTED PRIVATE KEY`, OpenSSL 3's default) with PBKDF2 and AES-CBC.
A key encrypted with scrypt or DES is refused with the `openssl pkcs8` command
that re-encrypts it.

The same settings are in `dbc web`'s connection form, and for a one-off
connection there are `--tls`, `--tls-ca`, `--tls-cert`, `--tls-key` and
`--tls-key-password '${VAR}'` (single-quoted, so the shell leaves it to dbc).

With **no config at all**, dbc starts on two built-in connections, one per
embedded engine, each seeded with the same `cats` table — so you can try
everything immediately, and compare the two engines on the same query:

| Connection | Engine | Storage |
| --- | --- | --- |
| `demo-bytdb` | bytdb | `demo.bytdb` in the OS cache dir (`~/Library/Caches/dbc`, `~/.cache/dbc`) — persists between runs |
| `demo-sqlite` | SQLite | in-memory, fresh every run |

`demo-bytdb` is the active one out of the box. `--demo sqlite` (or
`DBC_DEMO=sqlite`) makes `demo-sqlite` active instead; either way both are in the
connections list, so `Ctrl+L` — or `-c demo-sqlite` headless — switches between them.
The flag is ignored once a config file exists, since a config names its own
`default_connection`.

If the bytdb demo cannot be opened — another dbc already holds the file, or the
cache directory is not writable — it is dropped with a warning and dbc starts
on the SQLite demo rather than failing.

## The TUI

```sh
./dbc                 # mouse-first, with the AI assistant
```

```
 dbc  ▶ Run  ■ Stop  ◈ Explain  ⧉ Copy ▾  ⤓ Export  ⌕ History  ƒ Scripts  ▦ Tables  ✦ Ask  ● conn ▾
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

Row counts in the tables list are off until you ask for them: tick the
**rows** box in the Tables heading in `dbc web`, or press `#` in the
terminal's tables list (also on its right-click menu; the pane's title then
reads `Tables · 12 · rows`). Each count is an exact `count(*)` — never the
database's statistics estimate, which can be far off — so it reads every
table, and on a big schema it can take a while; the box's label pulses
until the numbers land. They show as `cats (8)` in `dbc web`, where hovering
says "cats with 8 rows", and a right-aligned `8` in the terminal. The switch
is per query tab, and unticking it cancels a counting still running.

The same list can show the schema's stored functions and procedures
instead of its tables, on Postgres and MySQL: click **Routines** beside
**Tables** in the heading in `dbc web`, or press `f` in the terminal's
Tables pane (the title then reads `Routines · 3`; `f` again goes back). The
list is the schema the tables are of — pick another schema and its
routines come with it — and each row shows its kind (`fn`, `proc`, `agg`,
`trigger`, `window`), with the arguments of overloads that share a name.
`Enter` or a double-click on one shows its DDL: the `CREATE` statement as
the server renders it (`pg_get_functiondef`, MySQL's `SHOW CREATE
FUNCTION|PROCEDURE`), colored as SQL, with **Copy** (`y`) and **Insert
into editor** (`i`, at the caret — nothing in the editor is replaced). On
Postgres an aggregate has no such rendering and says so. Like the row
counts, the switch is per query tab, and the routines are read only while
it is on; a `CREATE FUNCTION` run in dbc relists them, as a `CREATE TABLE`
does the tables.

On Postgres the whole list is counted in one statement (`query_to_xml` runs
a `count(*)` per table server-side); if that fails — a table you can see
but not `SELECT` from, say — each table is counted on its own, so only that
one goes without a number. MySQL, SQLite and bytdb count table by table. A
single count gives up after 30 seconds and the whole counting after two
minutes; what is not counted by then shows no number. Views are not
counted.

Counts load after the list, never holding it up, and are cached per
connection for two minutes. While they are on, a run that may have changed
rows (an `INSERT`, `DELETE`, `TRUNCATE`, DDL, a `COMMIT`, a script) recounts
the list straight away, past the cache; a `SELECT`, `SET` or `BEGIN` does
not. In `dbc web` every query tab on that connection with counts on, in any
window, is recounted, not just the one that ran it; writes made by other
clients show at the next connect. A write inside an open transaction shows
once it is committed, since the counts are read outside your session.

When a connection's tables span more than one schema, `dbc web` puts a
schema filter under the Tables heading, so a big catalog can be narrowed to
the one you are working in. Click it (or tab to it) for the list of schemas,
each with its table count; type to narrow the list — names starting with
what you typed come first — then `Enter` or a click picks one, and `Enter`
again moves to its tables. `Tab` does both at once: it picks the schema
typed to (or arrowed to) and moves on to its tables, so `sal` `Tab` `ord`
`Enter` previews `sales.orders`. `Esc` drops the typing; emptying the box and
picking "all schemas" (or its `×`) shows everything again. The heading then
reads `· 12 / 340`. The pick is remembered per connection, across reloads
and windows. The heading and the filter stay put while the tables scroll.

Under the pickers, a **table** box finds a table in the list: type part of
its name and the list narrows to the matches, the ones starting with it
first, with the first selected. `↑`/`↓` move the selection while you keep
typing, `Enter` previews the selected table, and `Esc` clears the find (a
second `Esc` leaves the box). Typing `schema.` matches qualified names.
`Tab` goes on to the selected row, where `c` and `e` work. Typing on a row
starts a new find with what you type, except for `c` and `e`, which are
that row's own keys; `/` goes back to the box.

On Postgres the sidebar is loaded a level at a time, so connecting to a big
server costs what the sidebar shows, not what the server holds:

- **db**: the server's databases (those you may `CONNECT` to; templates
  left out). Picking one switches the tab to it, using the same host,
  credentials and TLS. A Postgres connection is bound to one database, so
  this opens a connection named `<conn>/<database>` (e.g.
  `ProdDr/analytics`). It shows in the header, and tabs, history and the
  schema pick remember it like any connection. Moving off it closes its
  connection to the server once no tab is on it, so browsing a server's
  databases does not leave one open per database visited. Picked with
  `Tab`, the keyboard moves on to the schema box once it is connected.
- **schema**: the database's schemas with their table counts, empty ones
  included. A connect opens on the first schema on `search_path` if it has
  tables (else the first schema that does), or on the schema last picked
  for that connection. "all schemas" is offered while the database has at
  most 5,000 tables.
- **tables**: the picked schema's tables, read from `pg_catalog`. That
  includes ones your role holds no grant on (as psql's `\dt` does), which
  `information_schema.tables` would hide, making the schema look empty.
  Names are schema-qualified whenever the database has more than one
  schema, so a preview works off the `search_path`.

MySQL has the **db** level too: the server's databases you hold a privilege
in (all of them with `SHOW DATABASES`), less the server's own
`information_schema`, `mysql`, `performance_schema` and `sys` unless your
DSN opened one. Picking one opens `<conn>/<database>` in the same way,
TLS settings and all. A MySQL database is its one schema, so there is no
schema level: the picked database's tables are listed whole. A DSN that
names no database (`user:pw@tcp(host:3306)/`) lists no tables until you
pick one.

The terminal UI has the same two levels, as two rows at the top of its
Tables pane: `⛁ analytics ▾` and `◫ sales · 40 ▾` (on MySQL, the first
only). Click one, or press `d` or `s` in the Tables pane, for a list you
can type into to narrow; `Enter` or a click picks. The schema picked is
remembered per connection, across restarts too (in
`~/.config/dbc/schema-picks.json`).

The terminal's Tables pane finds a table as the web's table box does: press
`/` in it for a `⌕` line over the list, and type — the list narrows to the
names holding what you typed, in any case, and the title reads
`Tables · 3 of 40`. While you type, the pane's letter keys are text; `↑` `↓`
move through the matches, `Enter` previews the one under the cursor (or
shows a routine's DDL, with the routines listed) and leaves the filter in
place, and `Esc` clears it with the cursor kept on the table you found.
Back in the pane, `/` (or a click on the line) goes on typing and `Esc`
clears. A new catalog — another connection, database or schema — starts
with no filter.

The assistant matches table names in the loaded schema, and also
`schema.table` names in any other schema of the database. The ERD
button diagrams the picked schema. No table list is cut at `max_rows`, which applies only to results you ask
for (Ctrl+T included). SQLite and bytdb list their tables whole, as before,
with no database picker.

### Mouse

| Where | Gesture | Does |
| --- | --- | --- |
| anywhere | click | focuses that pane — the keyboard follows the mouse |
| toolbar | click | Run, Stop, Explain, Copy ▾, Export, History, Scripts, Tables, Assistant; `● conn ▾` switches connection |
| connections | click | connects |
| connections | right-click (or the toolbar's `● conn ▾`) | **Disconnect** the active connection, **Refresh** it (read its databases, schemas and tables again, for what another client created or dropped — the session and any open transaction stay), then the list to connect to; on a row, **Edit…** and **Remove…** for a connection added in dbc (below), and **Stop container** on one Postgres in Docker made; **Dump database…** on a Postgres one ([below](#dumping-a-database)); **Add a connection…**; **Postgres in Docker…** ([below](#postgres-in-docker)) |
| tables | click `⛁ db ▾` / `◫ schema ▾` | *(Postgres; MySQL has `⛁ db` only)* pick another of the server's databases / another schema, from a list you can type into |
| tables | click / double-click / right-click | select / preview the first 100 rows / show columns, diagram it, diagram all tables, copy the ERD as Mermaid, insert name, copy name |
| tables | `c` on the selected table | show its columns: its `information_schema.columns` rows (name, type, nullable, default, length) in the grid, ready to copy |
| tables | `#` | show or hide each table's row count (an exact `count(*)` per table; off by default) |
| tables | `e` on the selected table | diagram it and its neighbours (an ERD, below), saved as a PNG and opened |
| tables | `f` | *(Postgres, MySQL)* list the schema's functions and procedures in place of its tables, and back |
| routines | `Enter` / double-click / right-click | show its DDL (`y` copies it, `i` inserts it at the editor's caret) / the same / show DDL, insert name, copy name, show tables |
| editor | click, drag, double-, triple-click | caret, selection, word, line |
| editor | right-click | run, copy, cut, select all, undo, history, ask the assistant; go to definition, usages, rename; the consoles |
| editor | `Ctrl`+click | go to where the alias or CTE under the pointer is declared |
| results | click, drag, shift-click | cell, rectangular range, extend |
| results | row number click | selects the row |
| results | header click | sorts by that column (again: descending; again: result order) |
| results | header border drag / double-click | resizes the column / fits it to its content (past the 40-cell auto-size cap) |
| results | header right-click | hide that column (or the selected range's); hidden ones are listed there to show again |
| results | double-click | inspects the value in full (JSON is pretty-printed) |
| results | right-click | copy cell / row / range / whole result as **HTML table**, Markdown, CSV, TSV, JSON; sort; transpose; hide, fit, show columns; export; ask the assistant |
| results, transposed | name click · row-number click · border drag | sorts by that column · selects the row · widens every row (or, beside `column`, the names) — see [Transposed](#copying-results--including-into-teams) |
| any pane | wheel, shift+wheel | scrolls what is under the pointer, vertically / sideways |
| scrollbars | click, drag | jump, drag |
| pane borders | drag | resize the sidebar, the editor/results split, the log, the assistant — the sizes are kept for the next start |
| `‹` on Connections · `›` on the left edge | click | fold the sidebar away · bring it back (`Ctrl+B`) |
| assistant | `⤓ insert` on a code block | puts that SQL in the editor at the caret |
| results title | click `Results` / `◈ Plan` | switches the results pane between the grid and the plan |
| result tabs (the results pane's bottom border) | click · right-click | shows that result · rerun its query, pin or unpin, share with the assistant, close, close the unpinned ones (see [Result tabs](#result-tabs-and-the-log-per-connection)) |
| ↻ after the result tab on screen | click | reruns its query into that tab, as `r` does (a statement that writes asks first) |
| log title | click `⧉ copy` / `✕ clear` | copies the connection's log to the clipboard / empties it |
| plan | click / double-click / right-click | select a step (fold it) / step menu: copy, zoom the flame graph, ask the assistant |
| plan | `▸`/`▾` beside a step | folds or unfolds it |
| plan | chips | Tree · Flame · Insights, size by time / cost / rows, ▶ Analyze, ↗ Browser, ⧉ Copy |
| flame graph | click / double-click | select a step / zoom into it (click the breadcrumb to zoom out) |
| insights | `go to step` · `⧉ copy` · `⤓ insert` | show the step · copy the suggested SQL · put it at the end of the editor |

Mouse reporting takes the terminal's own text selection away, so every pane
that shows text has its own copy. To select raw terminal text anyway, hold
**Shift** (most terminals) or **⌥ Option** (iTerm2, Terminal.app) while
dragging.

### Keys

| Key | Action |
| --- | --- |
| `Ctrl+R` | Run the statement under the caret (or the selection); the gutter marks which |
| `Ctrl+Shift+R` / `Alt+R` | Run every statement in the buffer, in order |
| `Ctrl+X` | Explain the statement under the caret (or the selected one) — the plan opens in the results pane |
| `Alt+X` / `Ctrl+Shift+X` | Explain **analyze**: run it and measure every step (a write is rolled back or not run — see below) |
| `Ctrl+K` | Stop the running query or script, a connect still dialing — or the assistant's answer |
| `Ctrl+A` | Open the assistant / move between it and the editor |
| `Ctrl+E` | Export the result (format picker; file, or clipboard) |
| `Ctrl+O` | The scripts browser: `Enter` runs, `e` edits in `$VISUAL`/`$EDITOR` (checked when the editor exits), `n` new from a template, `d` duplicate, `r` rename, `Del` trash, `/` filter; the built-in examples and the trash are listed too (its title names the directory) |
| `Ctrl+P` | Query history of this database (`Tab`: every database) — filter, then `Enter` inserts (never runs) |
| `Alt+T` · `Alt+W` · `Alt+1`…`9` | A new query tab · close the tab · go to tab N (see [Query tabs](#query-tabs)) |
| `Alt+N` · `Alt+C` | A new console of the database · the database's next console (see [Consoles](#consoles-and-multi-statement-buffers)) |
| `F12` · `Shift+F12` · `F2` | *(editor)* On an alias, a CTE name or a column the query names: go to its declaration · mark and list its uses · rename it (see [Go to definition, usages and rename](#go-to-definition-usages-and-rename)) |
| `Ctrl+Space` | *(editor)* Suggestions at the caret — they also open by themselves after `.`, `::` and two letters of a word (see below) |
| `Ctrl+T` | List the tables and views on the active connection |
| `Ctrl+L` | Jump to the connections list |
| `Ctrl+B` | Fold the sidebar away / bring it back (also the `‹` on the Connections box and the `›` left on the edge) |
| `F1` · `?` | Every key, in a dialog (`?` outside the editor and the assistant's input) |
| `x` | *(connections)* Disconnect without picking another connection. If the session may hold a transaction, dbc asks first, because disconnecting rolls it back |
| `r` | *(connections)* Refresh the active connection: read its databases, schemas and tables again, on the schema the list shows now. The session, and any transaction open on it, stays; if the tables cannot be read, the old list stays up |
| `a` · `+` · `e` | *(connections)* Add a connection · a menu of **Add a connection…** and **Postgres in Docker…** · edit the one under the cursor (see [Adding connections](#adding-connections-in-the-terminal)) |
| `d` / `s` | *(tables, Postgres; `d` on MySQL too)* Pick a database / a schema |
| `Ctrl+G` | *(inside Cats)* Hand the statement to an agent in another pane |
| `y` / `Y` / `c` | *(results)* Copy the cell or range / the row / open the copy menu |
| `<` / `>` / `=` | *(results)* Narrow / widen the column / fit it to its content |
| `-` / `+` | *(results)* Hide the column (or the range's columns) / show every hidden column |
| `Enter` | *(results)* Inspect the value under the cursor |
| `t` | *(results)* Transpose: each row a column, each column a line — copies and exports follow; `t` again turns it upright |
| `p` | *(results)* Switch between the result grid and the plan |
| `{` / `}` | *(results, plan)* The previous / next result tab of the connection (see [Result tabs](#result-tabs-and-the-log-per-connection)) |
| `P` · `x` | *(results, plan)* Pin the result tab (a run then opens a new one) or unpin it · close it |
| `r` | *(results, plan)* Rerun the result tab's query into that same tab, pinned or not (a statement that writes asks first); or click its ↻ |
| `S` | *(results, plan)* Share the result tab with the assistant, or stop sharing it (`s` too; needs `ai_rows` — see [Sharing a result](#ai-assistant)) |
| `[` / `]` | *(results)* After a script that showed several results (`s.Show`): the previous / next one within its result tab — also a click on its number in "Result 1 · 2 · 3" on the results title |
| `z` | *(results)* Give the results pane the whole column / give it back |
| `y` · `x` | *(log)* Copy the connection's log · clear it |
| `Tab` / `Shift+Tab` | Cycle focus through the panes |
| `Esc` | Close a dialog or menu |
| `Ctrl+C` | Stop what's running; quit when idle (a query running in another tab keeps it from quitting — `Ctrl+Q` quits anyway) |
| `Ctrl+Q` | Quit |

The editor is a code editor, not a text box: it keeps the indent on
`Enter`, highlights SQL (keywords, strings, numbers, comments, parameters —
from the same scanner that splits statements, so what looks like a string is
one), and undoes typing a word at a time (`Ctrl+Z`, redo `Alt+Z`). Long lines
scroll sideways rather than wrap, so a click lands exactly where you point.

In terminals that deliver the kitty keyboard protocol, `⌘E`, `⌘P`, and
`⌘G` are equivalents for export, history, and handing to an agent.

#### Adding connections in the terminal

`a` in the Connections pane, or **Add a connection…** in its right-click
menu or in the menu `+` opens there (beside **Postgres in Docker…**), opens
the same form as `dbc web`'s **+** (see
[the browser workbench](#the-browser-workbench-dbc-web)): name, driver,
*Enter as* **Fields** (host, port, user, password, database and options —
or a file for SQLite and bytdb) or the whole **DSN**, TLS for Postgres and
MySQL, and whether the assistant may see result rows. `Tab`/`Shift+Tab` (or
`↑`/`↓`) move between the controls, `←`/`→` or `Space` change a choice,
and a click works everywhere. **Test connection** (`Ctrl+T`) dials, pings
and closes, and says how long it took or the driver's error, inside the
form; `Enter` saves, `Esc` cancels. A save adds the connection to
`~/.config/dbc/connections.toml` and connects to it.

Connections added this way — here or in `dbc web`; they share the file —
show a muted `✎` before their driver (`· tls` after it when TLS is on). `e`
on one, or **Edit…** in its right-click menu, opens the form filled in, the
password withheld as in the browser (leave it empty to keep it; the DSN as
text starts empty, meaning "keep the saved one"). **Remove…** asks first.
The connection dbc is on can't be renamed, given a new DSN, driver or TLS,
or removed — disconnect (`x`) or switch first — though whether the
assistant may see its rows can change at any time. A rename takes the
connection's schema picks along. The config file's connections are changed
in the file: their Edit and Remove rows say so.

#### Postgres in Docker

**Postgres in Docker…** in the connections menu — the TUI's and
`dbc web`'s — starts a PostgreSQL server in a local container and adds the
connection to it, for a scratch database without installing anything but
Docker. Pick a version from the supported ones (PostgreSQL 18 back to 14;
each leaves the list at its [end of life](https://www.postgresql.org/support/versioning/)):
the TUI shows them as a menu, `dbc web` as a dropdown. Beside each is the
state of its container, if there is one (`running on :5518`, `stopped`).

A pick pulls `postgres:<version>` if Docker does not have it yet (about
150 MB; the log says so), runs it as the container `dbc-pg<version>`
(`dbc-pg18`) on `127.0.0.1` port `55<version>` (`5518`, or any free port
when that one is taken), with its data in the Docker volume
`dbc-pg<version>-data` and a generated password. Once the server accepts
connections, dbc adds `docker-pg<version>` to `connections.toml` (as the
image's `postgres` superuser on its `postgres` database) and connects to it.
Picking the same version again starts the container if it was stopped, and
reuses it and its connection as they are. If the container was removed and
made again, the connection is pointed at the new one. A config-file
connection that already has the name is left alone, and the new one is
`docker-pg18-2`.

**Stop container dbc-pg18** in the right-click menu of a connection made
this way stops its container (`docker stop`). It is refused while a query
tab is on the connection: disconnect first, as for **Remove…**. The data
stays, and picking the version again starts the container. dbc never
removes containers: `docker rm dbc-pg18` does, keeping the data in the
volume, which the next pick reattaches. `docker volume rm dbc-pg18-data` deletes the data.
A container named `dbc-pg18` that dbc did not create is refused, not used.
The `docker` CLI must be installed and running (Docker Desktop, colima,
Rancher Desktop…). dbc looks for it on `PATH`, then in the usual install
directories, so the macOS app finds it too.

#### Dumping a database

**Dump database…** in a Postgres connection's right-click menu — in the TUI
and in `dbc web` — dumps it with `pg_dump`, exactly as
[`dbc dump`](#dumps-headless-postgres) does, from a form:

```
╭─ Dump pg ───────────────────────────────────────────────────────── ✕ ╮
│ Format         plain   custom   directory   tar   split              │
│               one SQL file, for psql                                 │
│ To file       ~/Downloads/pg-20261009-1430.sql                       │
│ What           everything   schema only   data only                  │
│ Schemas       all — or patterns: public, sales*                      │
│ Tables        all — or patterns: orders, sales.*                     │
│ Skip tables   none                                                   │
│ Skip data of  none — tables kept without their rows                  │
│ Options       [ ] no owners   [ ] no grants   [ ] INSERTs, not COPY  │
│               [ ] CREATE DATABASE first   [ ] DROP before creating   │
│ More options  other pg_dump options, e.g. --no-comments              │
│  ⇩ Dump    Cancel                                                    │
╰──────────────────────────────────────────────────────────────────────╯
```

The file suggested is in Downloads (else your home directory), named for
the connection and the minute. Its suffix follows the format as you change
it, unless you have typed a name of your own. `~` is the home directory;
`dbc web` writes on the machine it runs on. **directory** and **split** take
a directory (new, or empty) and a **Jobs** count of tables dumped at once.
The archive formats hide the owner, create and drop boxes: those are
`pg_restore`'s choices when you restore. Patterns are `pg_dump`'s, separated
by commas or spaces.

**Dump** first checks the form, asks the server its version and finds a
`pg_dump` new enough. Anything wrong is said in the form, which stays open.
Then the dump runs in the background. Each step and each line `pg_dump`
writes go to the log, and the last line says where the dump went, its size,
and how to restore it. A dump that fails or is stopped leaves no partial
file behind. While one runs, the menu offers **Stop dump of X** instead (one
dump at a time). Quitting the TUI or stopping `dbc web` stops it too. To
point dbc at client tools it doesn't find itself, set `pg_bin` in the
config (or `$DBC_PG_BIN`).

### Completion

The editor suggests what comes next from the connection's schema — its
tables, columns, types and foreign keys, and its stored functions and
procedures, read once per connection — and from the dialect's vocabulary:

| Where | Suggested |
| --- | --- |
| `FROM ▮` · `UPDATE ▮` · `INTO ▮` | tables and views (the browsed schema's first), schemas, the statement's CTEs; then set-returning functions (`SETOF`, `TABLE(…)`) |
| `JOIN ▮` | first the tables a foreign key links to the ones already named, as whole clauses: `customers c ON c.id = o.customer_id` |
| `ON ▮` | the foreign key's condition between the table just joined and the others |
| `o.▮` | the columns of the table or CTE alias `o` stands for (a `SELECT *` CTE included); `public.▮` lists a schema's tables, and its routines of the kind the context calls for |
| `SELECT ▮` · `WHERE ▮` · `= ▮` … | the columns of every table the statement names — written after the caret too, so `SELECT ▮ FROM orders` works — qualified when two share a name; then functions (built-in and the database's own) and keywords |
| `CALL ▮` | the database's procedures |
| `DROP FUNCTION ▮` · `ALTER PROCEDURE ▮` · `EXECUTE FUNCTION ▮` … | the functions, procedures or (for a trigger) trigger functions, by name |
| `INSERT INTO t (▮` | `t`'s columns |
| `::▮` · `CAST(x AS ▮` | type names |
| after a finished phrase | the clauses that can follow it: `WHERE`, `GROUP BY`, `ILIKE`, `ON CONFLICT` … |

Each column shows its type and keys, and each table its DDL. Postgres gets
its keywords and its everyday functions with their signatures — aggregates,
window, string, regex, date/time, JSON/JSONB, array, full-text search and
admin (`pg_size_pretty`, `pg_terminate_backend` …). bytdb gets Postgres's
keywords but only the functions and casts it implements (a test runs every
one against bytdb), so nothing it would answer with "unknown function" is
suggested. MySQL and SQLite get their own. Names are quoted where the engine needs it
(`"Orders"`, `` `order` ``), and keywords follow the case you type in.
A table goes in bare when its bare name finds it on the connection's
Postgres `search_path` (read with the schema), and schema-qualified
otherwise: with `search_path = app, public`, `app.users` goes in as
`users`, and a `public.users` it shadows as `public.users`. That is the
path a new connection gets from the role and database, not a
`SET search_path` run in your session.

Stored functions and procedures come from `pg_proc` on Postgres and
`information_schema.routines` on MySQL (SQLite and bytdb have none to
list). A function goes in as a call with the caret between its
parentheses, qualified by the same `search_path` rule as a table, and its
overloads are one suggestion whose detail lists every signature. What no
statement calls is left out: the built-ins of `pg_catalog` (the
vocabulary has those), and an extension's type I/O and index-support
functions (those taking `internal` or `cstring`). Trigger functions are
offered only after `EXECUTE FUNCTION`, procedures only after `CALL` (and
`PROCEDURE`/`ROUTINE`).

In the terminal the list opens by itself after `.` or `::` and on the second
letter of a word, and follows your typing; `Ctrl+Space` asks anywhere. `↑`/`↓`
choose, `Tab` picks, `Enter` picks only when that changes the text (at the
end of a word already typed in full it is a new line), `Esc` closes. In dbc
web it is Monaco's suggest list, fed by the same rules. Running DDL
(`CREATE`, `ALTER`, `DROP` …) or a script makes the next suggestion read the
schema again.

### Go to definition, usages and rename

On a table's alias, a CTE's name, or a column the statement names itself,
`F12` (or `Ctrl`/`⌘`+click) goes to where it is declared, `Shift+F12` lists every use, and `F2` renames it. All
three are also in the editor's right-click menu, in the terminal as in
`dbc web`.

In the terminal, `F12` (or `Ctrl`+click) selects the declaration.
`Shift+F12` marks every use in the statement (underlined), names their
lines in the log, and opens a references list under the editor — one row
per use, its line number and text with the use underlined. `↑`/`↓` (or
`Shift+F12` again, wrapping round) step through it while the editor above
shows each use; `Enter` or a click puts the caret there and `Esc` goes back
to where you were. Pressed again on the same name the list reopens on the
next use. The marks outlive the list: `Esc` in the editor or any edit
clears them. `F2` opens
a prompt holding the current name: type the new one and `Enter` renames
every use as one edit, which one `Ctrl+Z` undoes. A refused rename keeps the
prompt open and says why under the field. They work within the
statement under the caret, and as SQL scopes names: a subquery that
aliases a table `o` keeps its own `o` when you rename the outer one, and a
correlated `o.id` inside it still follows the outer one. The new name is
quoted where the engine needs it (`"Ord"` on Postgres, `` `order` `` on
MySQL). A rename is refused if the name is already taken in the same query.

It works by scanning the text, not by parsing SQL, the same way
completion does. A table referenced without an alias is found (`orders.id`
goes to `FROM orders`) but not renamed: renaming the text would not rename
the table, only stop the query from finding it.

Columns are resolved when the statement itself names them:

- a CTE's or a derived table's output columns: in
  `WITH t AS (SELECT count(*) AS n FROM orders) SELECT t.n FROM t`, `t.n`
  goes to `AS n`, and renaming either renames both. A column list
  (`WITH t(n) AS …`, `(…) AS d(n)`) declares them instead. A column passed
  on by another CTE (`SELECT n FROM t`, `SELECT * FROM t`) is the same
  column, so a rename follows it through;
- a select-list alias used in `ORDER BY` (`count(*) AS n … ORDER BY n`).

A bare `n` (no `t.`) is resolved only where nothing else could own it:
every table in that part of the query must be a CTE or a subquery whose
columns are known, since one catalog table in scope could have an `n` too.
Catalog columns are not resolved: that needs the schema. A CTE column that
is just a table's column (`WITH t AS (SELECT o.id FROM orders o)`) is
found from `t.id` but not renamed, as a table is not; alias it
(`o.id AS order_id`) and rename that. A column rename is refused if it
would change what any other name in the statement refers to. An alias used bare, without a column (`SELECT o FROM orders o`,
MySQL's `DELETE o FROM …`), is left alone.

### Explaining a query

`Ctrl+X` (or `◈ Explain`) asks the database how it will run the statement
under the caret. The plan opens in a **◈ Plan** tab beside the results; the
rows from the last run stay one click (or `p`) away.

```
╭─ Results · 5 rows │ ◈ Plan · 26.8 ms · ▲1 ─────────────────────────────────────────╮
│ ◈ postgres · analyzed · execution 26.8 ms · planning 0.4 ms   was 131 ms → 26.8 ms ▼80% │
│  Tree   Flame   Insights ▲1   size by  time  cost  rows       ▶ Analyze  ↗ Browser  ⧉ Copy │
│ step                                   rows   self time    % share  │ ⋈ Hash Join       │
│ ▾ ⇅ Sort  (sum(o.total)) DESC        5 rows       16 µs   0% ▏      │ time  9.9 ms total │
│ └▾ Σ HashAggregate  u.city           5 rows      5.6 ms  21% ██▏    │ rows  36,650 · est │
│    └▾ ⋈ Hash Join  (o.user_id…  36,650 rows      9.9 ms  37% ███▊   │ Hash Cond  (o.use… │
│       ├─ ▤ Seq Scan · orders o ▲ 50,000 rows     6.4 ms  24% ██▍    │ ▲ Full scan of …   │
```

- **Tree** — every step with its rows, its *own* time (or cost) and share, a
  heat bar from green to red, and a marker on each step with a finding. The
  panel beside it shows everything the engine said about the selected step:
  time total and self, loops, rows actual against estimated, rows a filter
  threw away, buffers, spills, and every property in the engine's own words.
- **Flame** — an icicle graph: each step's width is its subtree's share, so a
  wide block with nothing under it is the step to look at. Double-click zooms
  in.
- **Insights** — findings in plain words, each with the step it is about, why
  it costs, what to try, and — where there is one — the statement that does it
  (`CREATE INDEX idx_orders_user_id ON orders (user_id);`), with `⧉ copy` and
  `⤓ insert`. Nothing is run for you. The rules look for a full scan that keeps
  a sliver of the table, a scan repeated inside a loop or a correlated
  subquery, an index that fetches rows only to discard them, a planner
  estimate off by ×10 or more (named where it starts, not everywhere it
  cascades), a sort or hash that spilled to disk, a temporary index built on
  every run, and where most of the time goes. Small tables stay quiet.

Explain the statement again after adding an index and the headline compares
the two runs: `was 131 ms → 26.8 ms ▼80%`.

`Alt+X` explains **analyze**: the statement is run and every step measured.
What that means depends on the engine, and on whether the statement writes:

| Engine | Plan | Analyze a read | Analyze a write |
| --- | --- | --- | --- |
| Postgres | `EXPLAIN (FORMAT JSON)`; a `$1` statement gets a generic plan (16+) | per-step time, rows, loops, buffers | inside a transaction dbc **rolls back** — or a savepoint, if you have one open, so your own work survives |
| MySQL | `EXPLAIN FORMAT=TREE`, else the classic table (MariaDB) | `EXPLAIN ANALYZE` (8.0.18+) | not run — the estimate, with a note |
| SQLite | `EXPLAIN QUERY PLAN`, plus each table's size | the whole statement, timed | not run — the estimate, with a note |
| bytdb | `EXPLAIN` | the whole statement, timed | not run — the estimate, with a note |

The explain runs on the editor's pinned session, so your `SET`s, temp tables
and open transaction shape the plan just as they would the next `Ctrl+R`.
Engines that report no costs (SQLite, bytdb) are sized by a labeled heuristic,
never by invented numbers.

Already typed `EXPLAIN ANALYZE SELECT …`? `Ctrl+X` unwraps it (keeping the
ANALYZE), and running an EXPLAIN with `Ctrl+R` opens its output in the Plan
tab too, with the raw rows still in Results.

`y` copies the plan as text, `Y` the engine's own output. The assistant, asked
about an explained statement, gets its plan and findings too.

#### The plan in your browser

Every plan can also be opened as an **interactive web page** — a bigger canvas
than a terminal pane, and a file you can send to someone.

**Opening it**

| From | How |
| --- | --- |
| the TUI | `↗ Browser` in the Plan tab, `b`, or right-click ▸ **Open in browser (interactive)** |
| the shell | `dbc explain --open "SELECT …"` — explain and open in one step |
| the shell, to keep | `dbc explain -t html -o plan.html "SELECT …"` — write the page, open it yourself |

The TUI and `--open` save the page as `plan-<connection>-<date-time>.html`
in a `dbc-plans` folder in the system temp directory, and the log (or, for
`--open`, the terminal) says exactly where. The file is readable only by
you, since a plan quotes your SQL.

**Sharing it where a page won't go.** A plan is also a PDF, a picture, or a
Mermaid chart: the same graph, cards and heat colors, with the statement
above and the findings below, drawn by dbc itself (no browser needed, so it
works from the shell and on CI too).

| Form | For | From |
| --- | --- | --- |
| PDF | a document, an email, a printer | web: ⤓ Save ▸ PDF · TUI: right-click ▸ **Save as PDF** · shell: `-t pdf -o plan.pdf` |
| JPEG / PNG | a chat, a ticket, a slide | web: ⤓ Save ▸ JPEG / PNG · TUI: right-click ▸ **Save as JPEG** / **Save as PNG** · shell: `-t jpeg` / `-t png` |
| Mermaid | a pull request or wiki (GitHub, GitLab and Notion draw ` ```mermaid ` blocks) | web: `m` or ⤓ Save ▸ Copy as Mermaid · TUI: right-click ▸ **Copy as Mermaid chart** or **Save as Mermaid chart (.mmd)** · shell: `-t mermaid` |

The web's files are drawn as the view is showing them: sized by its metric,
in its light or dark, with the before/after comparison when there is one.
The shell's and the TUI's use dbc's dark palette, like the page — or light,
for a PDF headed to a printer: `plan_theme = "light"` in the config sets it
for both, and `dbc explain --theme light|dark` for one run. The PDF is a
picture on a page sized to fit the plan, so its text is not selectable; `y`
copies the plan as text.

**What you see**

- **Header** — the engine, how the plan was obtained (estimated or analyzed),
  execution and planning time, the EXPLAIN dbc sent, any notes (a rolled-back
  write, JIT time), and your statement, collapsible, with SQL highlighting.
- **Graph** (the default tab) — one card per step, laid out as a tree from
  the final result at the top to the table reads at the bottom. Each card
  shows the step, the table and index, its key condition, its rows, a
  `×N↑`/`×N↓` badge where the estimate was off, and its own time (or cost)
  with a share bar. Card color runs from green to red with that share, so the
  expensive step is the one that stands out. Edges get thicker with the rows
  flowing through them and are labeled with the join side (Outer/Inner) or
  subplan. A step with a finding carries a severity mark; a step that never
  ran is dashed.
- **Flame** — the same plan as an icicle: each block's width is its share of
  the total including everything under it, so a wide block with nothing below
  it is where the time goes.
- **Side panel** — everything about the selected step: time total and self,
  loops, rows actual against estimated, rows thrown away by a filter, cost,
  buffers, spills, every property in the engine's own words, and the findings
  about it.
- **Insights** — the same findings as the TUI, most serious first, each with
  its fix and, where there is one, the SQL with a **Copy** button. Click a
  finding to jump to its step.

The page opens on the step behind the most serious finding.

**Using it**

| Action | Does |
| --- | --- |
| click a card | select it and show it in the side panel |
| double-click a card, or its chevron | fold / unfold the steps under it |
| hover a card | highlight its path up to the result |
| drag the background | pan |
| mouse wheel | zoom toward the pointer (− / + / **Fit** buttons do the same) |
| **Time · Cost · Rows · Shape ≈** | choose what colors, bars and flame widths measure |
| click a flame block | select it and, if it has steps under it, zoom into it; click the top block again, or a step in the breadcrumb above, to zoom back out |
| ◐ (top right) | switch between dark and light |

| Key | Does |
| --- | --- |
| `↑` / `↓` | parent / first child |
| `←` / `→` | previous / next sibling |
| `Enter` | fold / unfold the selected step |
| `1`–`4` | size by time, cost, rows, shape (as available) |
| `f` | fit the graph to the window |
| `g` | back to the graph tab |
| `Esc` | zoom the flame graph back out |

**Good to know**

- The page is one self-contained file: no server, no internet, nothing loaded
  from anywhere. Attach it to a ticket or a chat and it opens the same for
  whoever receives it.
- `Shape ≈` appears for SQLite and bytdb, which report no costs or timings;
  it sizes steps by a labeled rough estimate, not measured numbers.
- A very large plan (over 120 steps) opens folded below the fifth level, so
  the first view is a map; unfold what interests you.
- Add `#flame` to the file's URL to open straight on the flame view.
- It looks like dbc wherever it is opened, whatever your terminal's theme.

### Entity-relationship diagrams

dbc draws a connection's schema as an ERD: a box per table with its columns,
types and key badges (`PK`, `FK`, `UK`, and `PF` for a primary-key column
that is also a foreign key), and a line per foreign key, from the child's key
column to the parent's, with crow's-foot ends:

| End | Means | When |
| --- | --- | --- |
| `┼┼` at the parent | exactly one | the key's columns are `NOT NULL` |
| `┼o` at the parent | zero or one | one of them is nullable |
| `>o` at the child | zero or many | the usual foreign key |
| `┼o` at the child | zero or one | the key's columns are unique in the child (1:1) |

Keys to the same parent column with different ends (one `NOT NULL`, one
nullable) attach a little apart on its row, one point per kind of end; keys
with the same end share it.

Parents sit to the left of their children, a column per level. A table with
many children has them wrapped into a block. Lines to a far column run
through the gaps between the boxes in their way; a gap is widened when more
lines need it than it holds. Tables with no relationships
are packed together underneath. Views are left out unless asked for, since
they have no keys. Past 40 columns a table shows its key columns and then as
many others as fit.

| Form | For | From |
| --- | --- | --- |
| PNG / JPEG | a chat, a ticket, a slide, a design doc | web: **ERD** beside *Tables*, or a table's right-click ▸ **Diagram around it** (`e`), then ⤓ PNG / ⤓ JPEG · TUI: `e` on a table, or right-click ▸ **Diagram around it** / **Diagram all tables** (saved as a PNG and opened) · shell: `dbc erd -t png` / `-t jpeg` |
| Mermaid | a pull request, a README or a wiki (GitHub, GitLab, Notion and Obsidian draw ` ```mermaid ` blocks) | web: ⧉ Copy as Mermaid (`m`) or ⤓ Mermaid · TUI: right-click ▸ **Copy ERD as Mermaid** · shell: `dbc erd` (`-t markdown` adds the fence) |

"Around" a table means the table plus every table one foreign-key hop away,
in either direction: what it references and what references it. The web
dialog can widen that to 2 or 3 hops or to everything it connects to, and
can include views. `f` there switches between fitting the window and actual
size. The shell and the TUI draw dark unless `plan_theme = "light"` or
`dbc erd --theme light`. The web's pictures follow the page's light or dark.

The schema is read fresh each time, from the engine's own catalog. On
Postgres and bytdb that is `pg_constraint` and `pg_attribute`. On MySQL it
is `information_schema.key_column_usage`, and on SQLite its `pragma_*`
functions. A Postgres partitioned table is drawn once: its partitions, and
the copies of its foreign keys Postgres keeps on each of them, are left
out (the sidebar still lists them). It runs on the connection pool, never inside your open
transaction, and it doesn't wait for a running query. Mermaid needs a plain
word as an entity name, so a table like `public.cats` on a multi-schema
connection becomes `t1["public.cats"]`, an alias that needs Mermaid 10.5 or
later. A type such as `numeric(10,2)` becomes `numeric(10_2)`, with the
original kept as the attribute's comment.

### Copying results — including into Teams

Right-click a result (or `⧉ Copy ▾`) and pick **Table for Teams / Outlook /
Docs (HTML)**: dbc puts a real HTML table on the clipboard, with inline
styles, so it pastes into Teams, Outlook, Slack or Google Docs as a formatted
table rather than as markup. Where the table flavor cannot land — a target
that pastes only text, or a copy over SSH that goes through the terminal —
the paste is the same result as an aligned text table, never the HTML
source (that is `Export → HTML page`). Markdown, CSV, TSV and JSON are one
row down.
The scope is the selected range when there is one, or the whole result.

**Transposed.** `t` in the grid (or **Transpose** in its right-click menu;
`⇄ Transpose` on the results bar in `dbc web`) turns the result on its side,
as psql's `\x` does: each row becomes a column headed by its number, and
each column a line led by its name — the easy way to read, or share, one
wide record. Copies and exports follow what the grid shows, so **Table for
Teams** pastes the vertical table (names as row headers; a single row comes
out as `column | value`), `y`/`Y` copy the values in the grid's
orientation, and an export (`Ctrl+E`; `⤓ Export ▾ → HTML page` in `dbc
web`) writes it. Sorting, hiding and selecting work as they do upright; the
arrow keys follow the screen. Click a name to sort by that column, a row's
number to select it whole. Every record shares one width: drag any record's
border to widen them all (in the terminal `<` `>` step it and `=` fits it),
or the border beside `column` to widen the names; a double-click on either
fits it. The grid stays turned for the next result until `t` turns it back.

The rich copy needs the local clipboard (macOS, Windows, or Linux with an
X11 display or `wl-copy` on Wayland). Over SSH dbc falls back to the terminal's clipboard
protocol, which carries plain text only, and says so in the log.

### AI assistant

`✦ Ask` (or `Ctrl+A`) opens a chat about the query in the editor and the
result in the grid. It runs GitHub Copilot by default through its official
language server, speaking the Agent Client Protocol, so dbc holds no
credential: the language server keeps it, shared with every editor that uses
it (ced, VS Code, Neovim). Signed in from one of those, dbc just works. If you
are not, the pane says so and offers **⎆ sign in to Copilot**. It shows a
code to enter at github.com/login/device, copies it, and opens your browser
there. The question you asked goes out once GitHub confirms. It is GitHub's
device flow, so it works over SSH too: enter the code in a browser on any
machine. The transcript's right-click menu has **Sign in to Copilot…** at any
time, which also tells you which account is signed in.

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

Without `ai_rows` only the column names go. The rows the assistant gets are
the grid's view, as a copy's are: in the header sort's order (and it is told
the order is yours, not the query's), without the hidden columns. It is
still told the hidden columns' names, so it can point out that the answer
may be in a column you hid. The chip above the input says
what the next question will carry (click it to send the question alone), and
the transcript records what each one did carry.

**Sharing a result.** By default a result goes with a question only when
it is the result of the statement under the caret. To ask about a
particular one — an earlier result tab, a result next to the query you are
fixing — share it: **✦ Share with the assistant** on the result tab's or the
grid's right-click menu, or `S` in the results. Its tab is marked `✦`, and
its result goes with every question asked on that connection, framed as
"shared result 2" together with the statement it came from, until you stop
sharing it (the same item, or `S` again) or close the tab. One result per
connection is shared at a time; sharing another moves the share. A shared
tab keeps its result as a pinned tab does — the next run opens a new tab
rather than change what the assistant sees. Sharing is offered only on a
connection with `ai_rows = true`; elsewhere the item says which setting
would allow it. The data rule still holds: at most `ai_context_rows` rows go,
in that tab's sort order, without the columns hidden in that tab, and if
`ai_rows` is turned off after sharing only the column names go.

The assistant can answer but not act: dbc declines every request from the
agent to run a command or touch a file. SQL in an answer gets `⤓ insert`,
which puts it in the editor for you to read and run — there is deliberately
no run-it-for-me button. Click the title for the model picker (Copilot's
premium multiplier is shown beside each model) or to switch assistant:
`ai_agent = "claude"` uses Claude Code (`claude-code-acp`), `"gemini"` uses
the Gemini CLI.

**Conversations are kept.** Each one is saved to `~/.config/dbc/chats/` (one
JSON file per conversation, readable only by you, the last 30 kept) after
every answer, on `⟲ new`, and on quit. The empty pane lists the most recent
ones — click one to reopen it — and right-click the transcript for
**Recent conversations…** to see them all. Click the list's header to
fold it down to the header and its count; it stays folded (across reloads
in `dbc web`, across runs in the terminal) until you click it again. A reopened conversation is the
transcript, not the agent's memory: it starts a fresh session, so quote what
matters in a follow-up. To delete one, right-click its row (in the pane or
the list) and pick **Delete conversation**, or press `d` twice on it in the
list; to delete the one on screen, right-click the transcript and pick
**Delete this conversation** — the pane clears without saving it.

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

Hidden columns are a view of the result, and copies and exports take that
view (so does the assistant): hide the noisy columns, then copy the table
into Teams. The header
marks where columns are hidden with `║`, the bottom strip counts them, and
the copy's log line says how many were left out. Hidden columns and
hand-set widths survive re-running a query with the same columns; any other
result starts fresh. The sort survives only a rerun of the result tab (`r`).

`Ctrl+T` is the `\dt`: it runs the active driver's catalog query and drops
the tables and views into the results as `table_schema · table_name ·
table_type` — the same three columns on all four drivers.

The editor's consoles, the query history and the assistant's conversations
persist between sessions under `~/.config/dbc`.

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
picks up dbc's subcommands and flags as well. `catctl plugin update
rohanthewiz.dbc` fetches and rebuilds; add `--ref v0.2.0` to the install to pin
a release instead of tracking main.

The plugin tab opens in your current directory, so a project's own
`./dbc.toml` is used when there is one, otherwise
`~/.config/dbc/config.toml`, otherwise the demo connections. The integration
described above works the same either way. Its declared type is what keeps
Cats from offering the dbc pane as a place to drop an agent prompt.

The palette also has **dbc — web**, which runs `dbc web` (below) in a pane
of its own: the pane is the server's console, and closing it stops the
server.

### Query history

Every statement you run is recorded. `Ctrl+P` opens the history newest first;
type to filter it — over the SQL and the connection name — `↑`/`↓` to pick,
and `Enter` drops the statement into the editor at the cursor. It is *not* run:
a recalled query can be edited first, which is usually why you went looking
for it.

The history is one list, but it opens on **this database**: the statements
run on the database the editor is connected to — the same idea of "database"
the consoles use, so two connection names onto one database share it, and a
`<conn>/<database>` has its own. `Tab` (or the `● app · ○ all` chip beside
the filter) widens it to every database and back, keeping what you typed. A
database with no history yet opens on all of them. `dbc web`'s `Ctrl+P` does
the same for the query tab's database (the scope button in its footer, or
`Tab`). Statements recorded by a dbc from before this was kept belong to no
database, and show only under "all".

Each statement of a multi-statement run is recorded on its own, so any one of
them is recallable. A statement identical to the one before it on the same
database is not recorded twice, and the app's own catalog query (`Ctrl+T`)
never is.

History lives in `~/.config/dbc/history.jsonl` — one JSON object per line, so
multi-line SQL comes back exactly as it was written — capped at the last 500
entries and readable only by you. Delete the file to forget everything.

### Consoles and multi-statement buffers

The editor holds a **console**: a running SQL file of the database you are
connected to, not one buffer for everything. Consoles are kept per host, then
per database on it, and a database can have as many as you like — one per
line of work. Switch connections and, once the connect lands, the editor
saves what you were writing to the old database's console and opens one of
the new database's (the one you last had open there this session, else its
first); the log says which (`console: db.example.com:5432 · app · console`).
Two connections onto the same database (another name, another role) share
its consoles, and a derived `<conn>/<database>` has its own. A connect that
fails leaves the console where it was.

`⌥N` (`Alt+N`) opens a new console of the database, `⌥C` (`Alt+C`) moves to
its next one, and the editor's right-click menu lists them all; the Query
pane's title names the one you are in. The same menu renames the console in
the editor (**Rename console…**: a name of letters, digits, `.`, `-` and `_`,
not one the database already has; the text, caret and undo stay) and deletes
it (**Delete console…**, after asking; the editor moves to the database's
next console, or a fresh empty one if it was the last). A `dbc web` tab
showing that console finds out at its next save, which is refused because the
file moved on; the tab then shows the file as it is, and `Ctrl+Z` brings its
text back.

Consoles are plain `.sql` files, readable only by you, under
`~/.config/dbc/consoles/<host>/<database>/<name>.sql`:

```
consoles/
  localhost_5432/          host and port
    app/                   a database on it
      console.sql          its first console
      console-2.sql
    analytics/
      console.sql
  db.example.com_5432/
    app/                   another server's "app": consoles of its own
      console.sql
  local/                   SQLite and bytdb: the file, by name + path hash
    scratch.db-0b5e9f12/
      console.sql
```

A host or database name that is not already a safe file name (a socket path,
a database with a space in it, an embedded database's full path) gets a short
hash of the original added, so two names that fold together stay apart. A
console is saved on every switch and on quit, so each database's scratchpad is
still there tomorrow; one that never had anything typed into it gets no file.
On the first launch with consoles, the old single `~/.config/dbc/buffer.sql`
seeds the console you start on; it is left in place, no longer written.
`dbc web` edits the same files (see [the browser workbench](#the-browser-workbench-dbc-web)): the TUI writes a
console back only when you changed it, so one it merely had open does not
overwrite what a browser tab saved meanwhile.

A console you switch back to is as you left it: the caret, the selection, the
scroll and the undo history come back with it (for as long as dbc runs). If its
file changed while it was away, the new text comes in as one edit, so `Ctrl+Z`
brings back what you left.

Keep a whole scratchpad of SQL in the editor and run one statement at a time:
`Ctrl+R` executes only the statement the cursor sits in, and the log says
which one (`running statement 2/4 …`), then that it finished
(`statement 2/4 completed on pg in 12ms — 3 rows`). Select a region first and `Ctrl+R`
runs exactly that instead — a selection holding several statements runs them
in order, stopping at the first failure, with a result tab for each statement
that returns rows (see [Result tabs](#result-tabs-and-the-log-per-connection))
and the last of them on screen.
To run the whole buffer without selecting it, press `Ctrl+Shift+R` (or
`Alt+R` — a terminal without the kitty keyboard protocol sends Ctrl+Shift+R
as plain Ctrl+R, and on macOS `Alt+R` needs Option set to send Meta), or pick **▶ Run all** from the editor's right-click menu;
it takes the same path as a selection of every statement.

Statements are separated on semicolons, ignoring the ones inside strings,
quoted identifiers, comments, and PostgreSQL `$$` bodies — so a function
definition stays in one piece.

Editor runs share one pinned database session per connection, just like a
headless multi-statement buffer: `BEGIN` in one `Ctrl+R` and `COMMIT` in a
later one bracket a real transaction, and `SET`, `PRAGMA`, and temp tables
persist between runs. Switching connections releases the session at once —
along with any transaction it had open, which is rolled back, and a warning in
the log if it had one. If the server drops the session's connection (an idle
timeout, a restart), a session that ran `BEGIN`, `SET`, or other statements
fails the run with "session lost", so a `COMMIT` is never replayed on a fresh
connection outside the transaction it was meant to end. A session that has
only run queries is quietly replaced and the statement retried — when the
driver can tell it never reached the server; if it may have, its error is shown
as-is rather than risk running it twice. Either way the next run starts on a
new session.

Other than the editor's pinned session, pooled connections that sit idle for
`conn_idle_timeout` (default `"1h"`; `"0"` = never) are closed and reopened on
demand.

### Query tabs

As in `dbc web`, the editor can hold several **query tabs**, each its own
workspace: its own pinned session (a `BEGIN` in one does not leak into
another), its own console, result, plan and Tables list. `⌥T` (`Alt+T`) opens
a tab on the connection you are on, `⌥W` closes the tab (asking first when its
session may hold a transaction, since closing rolls it back; the last tab
stays), and `⌥1`…`⌥9` switch. The connections list, the log and the assistant
are shared; the assistant always talks about the tab on screen.

With two or more tabs, the editor's top border becomes a strip of them:

```
╭ Query 1 · console ● ─ wip • ─ Query 3 ◆ ─ + ──────────────╮
```

Click a tab to switch, double-click it to rename it, right-click it for
rename, close, a new tab and its database's consoles; `+` opens one. A tab
left running keeps running: `●` marks it while it runs and `•` once it has
finished (in the error color if it failed), and its result is drawn when you
come back to it. `◆` marks a tab whose session may hold a transaction, `SET`
values or temp tables. In the connections list, `●` is the tab's connection
and `○` one another tab is on; a connection a tab is on cannot be removed or
re-pointed until that tab moves off it.

#### Tab groups

Tabs can be gathered into **groups**, as in `dbc web`: a short coloured chip
in the strip with its tabs behind it, their titles underlined in the group's
colour, and foldable to the chip alone (`prod +2` — two tabs hidden; the tab
on screen is never folded away).

```
╭─ prod ─Query 1─Query 3─ wip +2 ─Query 4─ Query 2 ─[lite]─ + ─────╮
```

- A **connection group** is a rule: every tab on its connection (or on one of
  its other databases, `prod/analytics`) belongs, including a tab that
  switches onto it later. A tab can be taken out by hand.
- An **ad-hoc group** is a hand-picked set; a new tab opened from one of its
  tabs joins it.

Right-click a tab (or press `⌥G`) → *Add to group…* to start one, or to move
the tab into an existing one. Click a group's chip to fold or unfold it;
right-click it for its menu: a new tab in the group (on the group's
connection), rename, make ad-hoc, close the group's tabs (not one that may
hold a transaction), ungroup, and its tabs. A connection group with no open
tab stays as a hollow `[name]` chip after the tabs — click it to open a tab
on its connection. The `+` wears the colour of the group `⌥T` would put the
new tab in (hover it to read which), and right-click on `+` opens a tab in
any group. Names are up to 8 letters or digits. Groups are saved with the
tabs and follow a renamed connection.

Two tabs on one database get two consoles, never one file written by both: a
new tab takes a console no other tab shows (or a new one), `⌥C` skips the
ones other tabs show, and the console menu sends you to the tab that has one.

Tabs are kept between sessions with the pane sizes, in
`~/.config/dbc/tui-layout.json` — each tab's title, connection and console. On
the next start only the tab on screen connects; the others connect the first
time you look at them, so ten saved tabs do not dial ten connections.

### Result tabs and the log, per connection

Each query tab keeps a **result set per connection** it has been on. Switch
the tab to another connection and the results pane shows that connection's
results — nothing, the first time — and switching back shows the old
connection's again, as you left them: the same result tabs, each with its
sort, hidden columns, widths and scroll, and the same plan. The assistant
follows along: "the last result", "the last error" and the plan it is told
about are the connection's on screen. Each tab's sets are its own, as its
session is: two tabs on one connection keep separate results. Results are
kept in memory for as long as dbc (or the `dbc web` page's tab) runs.

A set holds **result tabs**, drawn on the results pane's bottom border in
the terminal and on the results bar in `dbc web`:

```
╰─ 1⚑ select * from orders · 2✦ columns cats · 3 select count(*) … ↻ ── { } switch · r rerun · P pin · S share · x close ─╯
```

A run **replaces the result in the tab it started from**, unless that tab
is **pinned** (`⚑`, `P` or the tab's right-click menu): then it opens a new
tab. So the edit-the-WHERE-and-run-again loop keeps one tab, and a result
worth keeping is one key from kept. The tab a run lands in is picked when it
starts, so you can look through the other tabs while a slow query runs
without its result landing on the one you are reading. A tab is named after
its statement (or `preview cats`, `columns cats`, `script x.go`). A script's
`s.Show` results all land in one tab, where "Result 1 · 2 · 3" steps through
them.

**Rerun** a tab (`r`, a click on the **↻** beside the tab on screen, or
**↻ Rerun its query** on its right-click menu) to bring its result up to
date: its statement runs again, on the same session,
and the fresh result goes back **into that same tab** — a pinned one
included, which stays pinned, in its place in the strip, with its name (a
shared tab stays shared). The pin keeps a result from being replaced by
*other* runs; rerunning the tab is how you ask for that result again. The
tab comes on screen with its hidden columns, its widths and its sort kept
(a statement edited and run into the tab starts unsorted). A rerun is a run:
one at a time, `Ctrl+K` stops it, and if it fails the tab keeps its old
result. A tab whose statement may write (an `INSERT`'s "n affected") asks
before running it again, and a script's tab has no statement to rerun — run
the script again instead. A rerun is not added to the history again.

A run of **several statements** (Run all, or a selection of several) opens
a tab per statement that returns rows, as DBeaver and DataGrip do; an
`INSERT`, `UPDATE` or DDL statement gets none, and when no statement
returns rows the last one's result lands alone. The log has a line for each
write instead (`statement 2/3: 4 affected — UPDATE …`, or `done` for DDL;
none for `BEGIN`, `SET` or `COMMIT`); past five, the rest fold into one
line with their total (`… 495 more writes, to statement 500: 495 affected
in all`), so a script of INSERTs does not flood it. The last
tab filled is the one on screen. Those tabs stay together, and running the
buffer again **refills them in order** instead of piling up more — a rerun
with fewer row-returning statements closes the tabs it has nothing for,
while a pinned (or shared) tab is left out and keeps its result. Running
one statement with one of those tabs on screen replaces just that tab. When
a statement fails, the results of the ones before it still land.

`result_tabs` (default 10, at most 50) caps the tabs of one connection's
set. When a run needs a new tab and the set is full, the oldest tab that is
neither pinned nor shared is dropped — another run's before one of this
run's own, so a run with more results than the cap keeps its last ones (the
log says how many did not fit). If every tab is pinned, the run is
refused before it starts, and the log says to unpin or close one. `{` / `}`
step through the tabs, `x` closes the one on screen (its right-hand
neighbour takes its place), and the strip's right-click menu also closes
every unpinned tab at once. A tab shared with the assistant (`✦`, see
[Sharing a result](#ai-assistant)) is kept like a pinned one.

The **log** is per connection too: the log pane shows the messages of the
connection the tab on screen is on — its runs, notices, connects and the
`s.Print` lines of scripts run on it — under the title `Log · <connection>`.
Two tabs on one connection share its log. A line goes to the log of the
connection on screen when it is written. A run or explain that finishes
after you switched away keeps its lines (and its result or plan) for its
own connection, and the log on screen gets one line saying so.
Lines written while the tab is on no connection (after a Disconnect, say)
are moved into the next connection's log when it connects, so none is left
where it can't be seen. `⧉ copy` on the log's title puts the connection's
log on the clipboard (one `HH:MM:SS message` line each), and `✕ clear`
empties it; in the terminal, `y` and `x` do the same with the log focused.
In `dbc web` a script tab, which is on no connection, has a log of its own.
It is the script's log, kept by its name: close the tab and open the script
again, and its earlier lines are there; rename the script and the log moves
with it. Logs are kept while the page is open in `dbc web` (a reload starts
them empty) and while dbc runs in the terminal; neither is saved.

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
r, err := s.Query("demo-sqlite", bigQuery)
if err != nil {
	if sdb.IsCanceled(err) {
		return nil // stopped by the user, not a failure
	}
	return err
}
```

## The browser workbench: `dbc web`

```sh
dbc web                      # serve on 127.0.0.1:8450 and open the browser
dbc web --no-open            # print the sign-in link instead
dbc web --listen 127.0.0.1:9000
```

`dbc web` is the TUI's workbench in a browser: connections and tables on
the left, a Monaco SQL editor, the results grid, the plan view, the log, and
the AI assistant. It runs statements through the same code the TUI does, so
the run slot, pinned sessions, cancel, history, explain and the assistant's
data rules behave the same in both. Beside query tabs it has
[script tabs](#script-tabs-in-dbc-web), which edit and run Go scripts, and
[pipeline tabs](#pipeline-tabs-in-dbc-web), which draw pipelines on a
canvas and preview and run them, and
[job tabs](#job-tabs-and-the-runs-view-in-dbc-web), which draw jobs — DAGs
of pipelines — beside the **Runs** view (`Alt+R`), every run drilled into.

**Layout.** A draggable bar separates every pair of neighbouring sections:
the sidebar and the work column, Connections and Tables, the editor and the
results, the results and the log, and the work column and the assistant.
Double-click a bar to go back to the default size. `‹` beside Connections
(or `Ctrl+B` / `⌘B`, or dragging the sidebar's edge shut) folds the sidebar
away; the `›` tab left on the window's edge brings it back. Sizes and the
fold are saved with the layout.

**Access.** It listens on loopback, and only a browser holding this launch's
secret can use it: dbc opens `…/login?s=<secret>` (and prints it), which
trades the secret for a session cookie. A restart signs every browser out;
use the new link. Scripts can send `Authorization: Bearer <secret>` (set it
with `--secret` or `DBC_WEB_SECRET`). A `--listen` address off loopback is
allowed, with a warning — it is a local tool, not a team server. The port is
8450, or a free one when 8450 is taken.

**Query tabs.** (The TUI has them too — see [Query tabs](#query-tabs).) Each tab is its own workspace with its own pinned session,
so a `BEGIN` in one does not leak into another. `Alt+T` opens a tab, `Alt+W`
closes it (asking first when its session may hold a transaction, since
closing rolls it back), `Alt+1`…`Alt+9` switch, and a double-click renames.
A tab keeps its result tabs, each with its grid view (sort, hidden columns,
widths), and its plan while another is on screen — per connection, as in
the terminal (see
[Result tabs and the log](#result-tabs-and-the-log-per-connection)); a run
left going in the background marks its tab (● running, • done) and names its
log lines.

**Tab groups** gather tabs behind a short coloured chip, as ced's do.
Right-click a tab → *Add to group…* to start one: an **ad-hoc** group
holds the tabs you put in it (a tab opened from one joins it), and a
**connection** group holds every tab on its connection or on one of its
other databases, including tabs that switch to it later. Click a chip to
fold its group down to the chip (`wip +2`), and right-click it to rename,
ungroup, close the group's tabs, or jump to one of them. Names are up to 8
letters or digits. Groups are saved with the layout and follow a
renamed connection.

A tab's text is one of its database's **consoles** — the TUI's files (see
[Consoles](#consoles-and-multi-statement-buffers)), so the terminal and the
browser share each database's running SQL. The tab names its console after
its title. When the tab's connection lands on another database, the tab
saves its console and shows one of the new database's that no other tab of
the window is showing, or a new one, so two tabs on one database get two
consoles. `Alt+N` gives the tab a new console of its database, `Alt+C` moves
to the next one, and right-clicking the tab lists the database's consoles
(and which tab shows each) with new, rename and delete. A rename or a delete
reaches every window; tabs showing a deleted console move to another. Saves
are checked against the file: if it changed since this tab loaded it (another
window, the TUI, an editor), it is not overwritten. The tab shows the file's
text instead and says so in the log, and `Ctrl+Z` brings your text back to
save over it. Another window's save shows up at once in a tab with no unsaved
edits to that console. A console keeps its caret, scroll and undo history in
the window for as long as the page is open, even while no tab shows it, so a
tab that comes back to it finds it as it was left. On the first start with consoles, each saved tab's
text moves into a console of its database (without overwriting one that
exists). The tab keeps its own copy too, so an older dbc still opens it.

Tabs, their connection and console, the pane sizes and light/dark are saved in
`~/.config/dbc/web.bytdb`; a reload keeps every tab's session. A saved tab
is shown by one browser tab of dbc web at a time: a second browser tab gets
the saved tabs the first is not showing, or a fresh one, with sessions of its
own. Closing a browser tab frees its tabs for the next one opened.

**Postgres in Docker…**, under **+** or in a connection's right-click menu,
starts a local PostgreSQL in a container and adds its connection; see
[Postgres in Docker](#postgres-in-docker).

**Dump database…** in a Postgres connection's right-click menu dumps it with
`pg_dump` from a dialog, as the TUI does; see
[Dumping a database](#dumping-a-database). The dump runs on the server in
the background, so the dialog closes once it starts. Its progress and outcome
reach every window's log over the event stream, and a reload doesn't lose
them.

**Adding connections.** The **+** beside *Connections* opens a menu:
**Add a connection…** and **Postgres in Docker…**. The first opens a form: name,
driver, the connection itself, [TLS](#tls), and whether the assistant may see
result rows (`ai_rows`). *Enter as* switches between **Fields** (host, port,
user, password, database and extra options, or a file for SQLite and bytdb)
and the whole **DSN** as text. From fields, dbc writes the DSN for you and
handles the quoting and escaping, so a password with `@`, `/` or spaces
needs nothing special. Options are `key=value` pairs separated by spaces
or `&` (for example `application_name=dbc` or `parseTime=true`). The password
may be a `${VAR}`. The TLS section shows for Postgres and MySQL, and its file
fields appear once a mode other than *disable* is picked. A relative path
there is relative to `~/.config/dbc`.
**Test connection** opens the DSN, pings it within `connect_timeout` and
closes it again, so nothing is saved until you choose to. It reports how long
the connect took, or the driver's error. For a SQLite or bytdb file that does
not exist yet, it says so rather than creating one. **Save** adds the
connection and switches the tab to it, whether or not it was tested. The
connection is kept in `~/.config/dbc/connections.toml`, not in your config
file, which dbc never rewrites. Every dbc reads it after the config file,
so the TUI, headless runs (`dbc -c name …`), `script`, `migrate` and
`explain` see these connections too. If the config file later defines the
same name, the config file's wins, with a warning. `connections.toml` uses
the same `[[connection]]` tables as the config file, so an entry can be
copied across. You may edit it by hand, but dbc rewrites it whole on the
next change from the browser, so comments are not kept. A running `dbc web`
reads it only at startup. A DSN is stored as typed: write `${PGPASS}` and the
password stays in the environment instead of the file. Each dbc expands it
from its own environment, so set the variable wherever you run the TUI too.
A DSN typed with the password inline is stored as typed, in a file only you
can read (`0600`). A DSN is never sent back to the browser. Connections that
an older `dbc web` kept in `web.bytdb` are moved to `connections.toml` the
first time it starts. Connections added this way show a muted `✎` before their
driver (the tab's connection is marked by the bar on its left; a dimmer bar
marks one another query tab, here or in another browser window, is on — hover
it to see which), and any
connection with TLS on says `· tls` beside its driver. To change one,
right-click it and pick *Edit…*: the same form, filled in. In fields, every
part of the saved DSN comes back except the password (a `${VAR}` password
does come back, since it names the secret rather than being it). Leave the
password empty to keep the saved one. If a DSN can't be split into fields
(several Postgres hosts, a MySQL unix socket), the form opens on the DSN
text instead and says why. As text, the DSN field starts empty. Leave it
empty to keep the saved DSN (for a test too), or type a new one; changing
the driver needs a new DSN. A rename keeps
the connection's place in the list and moves the saved query tabs that were
on it. To remove one, pick *Remove…*. *Disconnect*, on the row the tab is
on, takes the tab off the connection and keeps the connection in the list.
If the session may hold a transaction, it asks first, because disconnecting
rolls it back. The connection to the server closes when no other tab is
on it. A connection a tab is still on can't
be removed, renamed or given a new DSN until you switch that tab away;
whether the assistant may see its rows can be changed at any time. The
file's connections are changed in the file. The TUI has the same form —
`a` in its Connections pane (see
[Adding connections in the terminal](#adding-connections-in-the-terminal)).

**Keys.** The TUI's, bent where a browser keeps the chord for itself:

| Key | Does |
|---|---|
| `Ctrl+Enter` (`Ctrl+R`) | run the statement under the caret, or the selection |
| `Ctrl+Shift+Enter` | run every statement |
| `Ctrl+X` (nothing selected) · `Ctrl+Shift+X` | explain · explain analyze |
| `Ctrl+K` | stop the run (in the assistant: stop the answer) |
| `Ctrl+P` · `Ctrl+E` · `Ctrl+O` | history · export · scripts |
| `Ctrl+S` | in a script tab, save the script |
| `Ctrl+I` | the assistant, and back (`Ctrl+A` stays select-all) |
| `Ctrl+Space` | suggestions (they also open as you type — see [Completion](#completion)) |
| `F12` · `Shift+F12` · `F2` | on an alias, a CTE name or a column the query names: go to its declaration · list its uses · rename it (see [Go to definition, usages and rename](#go-to-definition-usages-and-rename)) |
| `Alt+T` · `Alt+W` · `Alt+1`…`9` | new tab · close tab · go to tab |
| `Alt+N` · `Alt+C` | new console · next console of the tab's database |
| `Alt+R` | every run of a job or a pipeline, drilled into (see [Job tabs and the Runs view](#job-tabs-and-the-runs-view-in-dbc-web)) |
| `{` · `}` · `P` · `r` · `S` · `x` | in the results: previous · next result tab · pin · rerun its query into it · share with the assistant · close |
| `F1` or `?` | every key |

On a Mac, `⌘` works wherever `Ctrl` is listed.

**What a browser adds.** Copy puts a real HTML table on the clipboard (the
browser writes `text/html` itself, so "copy for Teams" works over SSH too);
exports are downloads; the plan view is the interactive page's own, with
before/after comparison when you explain again; findings' SQL goes into the
editor with ⤓ Insert.

**The assistant** (`Ctrl+I`, or ✦ Assistant) is the TUI's: the same agent,
the same data rule — rows only on connections with `ai_rows = true`, in the
grid's sort order, without its hidden columns — and the same conversation
archive, so a conversation had in either is offered in both. "✦ Ask" in the
grid's menu, the value inspector, the editor's right-click menu and the plan
drafts a question about what you are looking at. Copilot sign-in runs in the
pane: dbc shows the device code, with buttons to copy it and open GitHub's
page.

**Scripts** (`Ctrl+O`, or ▷ Scripts) open the scripts browser: run, make,
rename or trash a script, or copy one of the built-in examples. Editing a
script opens a script tab — Go in Monaco, errors marked as you type, `sdb`
completion, `Ctrl+S` to save, and Run saves first. `s.Print` lines reach the
log and `s.Show` results the grid as they happen. See
[The scripts browser](#the-scripts-browser) and
[Script tabs in dbc web](#script-tabs-in-dbc-web).

`Ctrl+C` in the terminal stops the server; every tab's run is stopped and
its session released — anything left open is rolled back.

### As a macOS app

```sh
./mac-install.sh     # from a checkout: builds it, installs ~/Applications/dbc.app
curl -fsSL https://raw.githubusercontent.com/rohanthewiz/dbc/main/mac-install.sh | bash
```

`dbc.app` is `dbc web` in a window of its own rather than a browser tab: a
small Swift/WebKit shell ([`macapp/DbcApp.swift`](macapp/DbcApp.swift))
around a bundled dbc. It needs the Xcode Command Line Tools
(`xcode-select --install`) for `swiftc`; Go comes from your `PATH`, or the
script fetches the version `go.mod` asks for into `~/.local/go`. Run from a
checkout, the script builds that checkout as it is. Piped from curl, it
keeps a clone of `main` in `~/.dbc-src` and resets it on every run. The app
is built on your Mac and signed ad hoc, not downloaded, so there is nothing
to notarize. Re-run the script to update it. `DBC_APP_DIR` and
`DBC_APP_NAME` change where it goes and what it is called (the script's
header lists the rest).

Each launch starts its own `dbc web` on a free loopback port with a secret
of its own, waits for `/api/v1/health`, and signs its window in, so a
`dbc web` in a terminal keeps working beside it. The server gets your login
shell's environment (`$SHELL -il`), as a terminal would. That makes the
assistant's agents findable on `PATH`, and `${PGPASS}` in a DSN and
`ANTHROPIC_API_KEY` resolve as they do there. It starts in your home
directory, so `~/dbc.toml`, then `~/.config/dbc/config.toml`, is the config.
Everything else (connections, history, conversations, tabs and layout) is
the same `~/.config/dbc` every dbc uses. Only one `dbc web` can save tabs
at a time, though: whichever of the app or the terminal starts second warns
in its log and does not save them.

Downloads go to `~/Downloads`. The plan's standalone page opens in a second
window, and links off the workbench (GitHub's Copilot sign-in page, say)
open in your browser. **View ▸ Open in Browser** signs your browser in to
the same server. **View ▸ Show Log** opens `~/Library/Logs/dbc/dbc-web.log`,
the server's console (the previous launch's is `dbc-web.log.1`). ⌘Q stops
the server as `Ctrl+C` would. If the app crashes or is force-quit, the
server notices its stdin close and shuts down the same way. The app never
leaves a server behind.

## Scripting in Go

A script is a plain Go file defining one function:

```go
//go:build ignore

// Count the cats at each minimum age.
package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	for _, minAge := range []int{1, 3, 5} {
		r, err := s.Query("demo-sqlite",
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

Scripts live in `~/.config/dbc/scripts`, beside the consoles and the
history, unless the config sets `scripts_dir`. A relative `scripts_dir` is
relative to the config file that sets it, not to the directory dbc starts
in, so the TUI from any directory, `dbc web` and dbc.app all read the same
scripts. `~` and `${VAR}`s are expanded. The directory is made on the first
save. `dbc scripts` prints the directory and lists what is in it, and the
scripts browser names it in its title.

Scripts are interpreted at runtime (via yaegi) — no compile step, edit and
re-run. The full Go standard library is available. The `//go:build ignore`
line just keeps `go build` from compiling script files if they live inside a
Go module; dbc runs them regardless.

A script is an ordinary file, so it can be written anywhere: in dbc web's
script tabs, in your own editor from the TUI, or in vim and committed to a
repo of its own. Every save is revision-checked against the file, so a
change made elsewhere (another window, the TUI, an editor) is a conflict to
resolve, never silently overwritten.

### The scripts browser

`Ctrl+O` (or ƒ / ▷ Scripts on the toolbar) opens it, in the TUI and in dbc
web alike:

```
┌ Scripts · ~/.config/dbc/scripts ─────────────────────── [+ New] ┐
│ ⌕ filter                                                         │
│ copy_mytable.go   Copy myschema.mytable from ProdDr to dev   2h  │
│ nightly_report.go Export yesterday's orders as HTML          3d  │
│ ─ Examples ───────────────────────────────────────────────────── │
│ copy_table.go     Copy a Postgres table from one connection…     │
│ loop_params.go    …                                              │
│ ─ Trash (2) ▸ ────────────────────────────────────────────────── │
└──────────────────────────────────────────────────────────────────┘
```

- **Scripts** — yours, by name, each with its description (the comment
  above `package main`, to its first sentence) and age. With none yet,
  the section lists the **templates** instead: blank, query and show, a loop
  over parameters, copying a table, exporting a report. A template's
  connection names are filled in from your config, the current connection
  first.
- **Examples** — the samples in the repo's [`scripts/`](scripts/), built
  into the binary, so a fresh install has them with no checkout. They are
  read-only: Enter makes your own copy, which opens for editing. Headless,
  `dbc script loop_params` runs the built-in copy as it is.
- **Trash** — folded. Trashing asks nothing, because it moves the file into
  `<scripts_dir>/.trash/` (the newest 50 are kept), and Enter on a trashed
  script restores it, offering another name if the old one has been taken
  since.

Enter on a script runs it, as Ctrl+O always has. New, duplicate and
copying an example ask for a name, write the file, then open it for
editing. The rest:

| | TUI | dbc web |
|---|---|---|
| run · edit | `Enter` · `e` (or double-click) | `Enter` · `Shift+Enter` (or ▶ · ✎) |
| new from a template | `n` (or `+ New`) | `Alt+N` (or `+ New ▾`) |
| duplicate · rename | `d` · `r` / `F2` | ⋯ Duplicate… · `F2` |
| trash | `Del` / `x` | `Ctrl+Delete` (`⌘⌫`) |
| copy the path | `y` | ⋯ Copy path |
| filter | `/` (Esc or Tab back to the list) | type — the filter has the focus |
| unfold the trash | `t` (or click its heading) | click its heading |

In the TUI a right-click on a row has these as a menu; in the web, ⋯ on a
script's row does. The keys differ because the web's filter keeps the focus,
so bare letters would type into it, and a terminal does not reliably pass
`Shift+Enter` or `Ctrl+Delete` through.

### Script tabs in dbc web

Edit (or New) opens the script in a **script tab**: the editor in Go mode,
with the grid and the log beside it as for a query.

- **Run** (`Ctrl+Enter`, ▶ Run) saves first, then runs the file, so what
  runs is what is on disk and what `dbc script` would run. A save that does
  not land stops the run. `■ Stop` / `Ctrl+K` stops it as any run.
- **Save** is explicit: `Ctrl+S` or ⤓ Save. A script never autosaves, but
  unsaved edits are kept in the browser, so a reload does not lose them.
  Each browser tab keeps its own: two tabs editing one script never take
  each other's unsaved text, and a tab that was closed with unsaved edits
  hands them to the next tab that opens the script. The
  tab reads `▷ name.go ●` while it has unsaved changes, and closing it asks
  whether to save, discard or keep it open. If the file changed on disk
  meanwhile, the save is a conflict: the file's text loads as one undoable
  edit, and `Ctrl+Z` brings yours back to save over it.
- **Errors as you type.** 600 ms after you stop typing, the unsaved text is
  checked (as `dbc script --check` does) and the errors are marked on their
  lines; the header adds `· 1 error`. ✓ Check also lists them in the log.
- **Completion and hover** for the `sdb` API (`s.`, `sdb.`, the fields of a
  `Result`, `CopyOpts` or `WriteOpts`), with signatures and doc comments,
  and connection names inside a connection argument (`s.Query("…`).
- A script names its own connections, so a script tab is never connected:
  a click on a connection in the sidebar puts its name in at the caret, and
  Explain, Run all and the rows box are hidden.
- **Several results.** Each `s.Show` is kept (the newest 20 of a run) in
  the one result tab the run lands in (see
  [Result tabs](#result-tabs-and-the-log-per-connection)), and
  "Result 1 · 2 · 3" on the results bar switches between them. Copies,
  exports and the assistant follow the one shown.
- **The assistant** is asked about the script: it gets the whole file as
  Go, the configured connection names and drivers, the tables its SQL names,
  and the last run's error or shown result when that run was this script.
  The first script question of a conversation also sends a summary of the
  `sdb` API (signatures and one line each), so answers call the real
  methods. Go in answers gets ⤓ insert, into the script at the caret.

Double-click the tab to rename the script. One script has one tab; opening
it again goes to that tab.

### Editing scripts from the TUI

`e` in the browser suspends the TUI and opens the script in `$VISUAL`, else
`$EDITOR`, else `vi`. The value is split on spaces, so `EDITOR="code -w"`
works (the editor must wait until the file is closed). When it exits, the
TUI comes back, logs whether the file changed, checks it and logs each
problem as `~/…/name.go:6:9: undefined: x`. The browser then reopens on the
script, so Enter runs it. An editor that is not on `PATH` is said in the log,
and nothing is suspended. As in dbc web, a script that shows several
results keeps them (the newest 20): "Result 1 · 2 · 3" on the results
title switches between them, by a click or `[` / `]` in the grid.

After a script run the assistant is asked about that script, as a dbc web
script tab asks: the file's source as Go, the connection names and drivers,
the tables its SQL names, the run's error or shown result, and on the
conversation's first script question the `sdb` API summary. This lasts until
you are back in the editor — an edit or a caret move there, or a run of a
statement, makes the statement under the caret the subject again. The chip
above the assistant's input says which one goes.

### Checking without running

`dbc script --check NAME` checks a script without running any of it (not
`Run`, not `init()`): syntax, that it is `package main` with a
`func Run(s *sdb.S) error`, and the interpreter's compile pass (undefined
names, type mismatches, an import other than the standard library and
`sdb`). It also warns about the map-assignment quirk below. It is the same
check both UIs run after an edit. yaegi is not the Go compiler, though: only
the first compile error is reported, and an unused variable or import
passes. See [Scripts headless](#scripts-headless).

### The `sdb.S` API

| Method | Purpose |
| --- | --- |
| `s.Conns() []string` | Configured connection names |
| `s.Query(conn, sql, args...) (*sdb.Result, error)` | Run a query with params |
| `s.Exec(conn, sql, args...) (int64, error)` | Run a statement, get rows affected |
| `s.DB(conn) (*sql.DB, error)` | Raw `database/sql` handle — transactions, prepared stmts, anything |
| `s.Explain(conn, sql, analyze) (*sdb.Plan, error)` | The plan, as `Ctrl+X` sees it: `p.Text(sdb.PlanText{Insights: true})`, `p.Insights`, `p.Root` — see [`scripts/plan_check.go`](scripts/plan_check.go) |
| `s.Show(r)` | Push a result to the results table (stdout when headless) |
| `s.Print(format, args...)` | Log to the TUI log pane (stdout when headless) |
| `s.Export(r, format, path)` | Export a result; empty path → clipboard |
| `s.Canceled() bool` | True once the user has stopped this run |
| `s.Ctx() context.Context` | The run's context, for `select` on `Done()` |
| `sdb.IsCanceled(err) bool` | Tells a stop apart from a real query failure |

`sdb.Result` gives you `Columns []string`, `Rows [][]string`, `Raw [][]any`,
`Duration`, and `Affected`. Use the placeholder style of the target driver
(`$1` postgres/bytdb, `?` mysql/sqlite).

#### The DDL log

Every DDL statement a script runs is logged where `s.Print` lines go (the
connection's log in the TUI and dbc web; stdout or stderr headless) just
before it runs, verbatim. That means `CREATE`, `ALTER`, `DROP`, `TRUNCATE`,
`RENAME`, `COMMENT`, `GRANT` and `REVOKE`. It is always on, and a script
cannot turn it off:

```
DDL local: CREATE TABLE IF NOT EXISTS staging (id int PRIMARY KEY, body text)
DDL local: DROP TABLE nope
DDL local failed: SQL logic error: no such table: nope (1)
```

It covers `s.Query` and `s.Exec`, and each statement of a multi-statement
`Exec`. It also covers what `s.Copy` and `s.Writer` run to prepare a
destination: a `Create`'s `CREATE TABLE`, a `Truncate`, and a Writer's
`Setup`. A `Query` or `Exec` that fails adds a `failed` line, because a
script may carry on past the error. Logging before the statement rather than
after it means a long `CREATE INDEX`, or an `ALTER` waiting on a lock, shows
up while it is still running. It has limits:

- It works from each statement's first keyword, so DDL inside a function
  body or a Postgres `DO` block is not seen.
- Statements run through the raw `s.DB` handle are not logged.
- On SQLite and MySQL a `Truncate` runs as a `DELETE`, which is not DDL, so
  it adds no line.
- `dbc copy` is not a script and logs nothing.

Sample scripts live in [`scripts/`](scripts/): parameter loops, multi-host
sweeps, CSV/HTML report generation, and copying tables between connections.
They are the browser's Examples.

### ETL across connections

Scripts can move rows between connections, on the same engine or different
ones (Postgres, MySQL, SQLite, bytdb, in any pairing). Copying a table from
one Postgres to another is one call:

```go
st, err := s.Copy("prod-pg", "local-pg", "public.orders", sdb.CopyOpts{
	Create: true, Truncate: true, ProgressEvery: 250_000,
})
s.Print("%s", st) // copied 500000 rows prod-pg:public.orders → local-pg:public.orders in 886ms (direct COPY)
```

`"prod-pg"` and `"local-pg"` are connection **names**, not DSNs: the same
names as in the connections list, from the config file's `[[connection]]`
entries (see [Configure connections](#configure-connections)) or from
`~/.config/dbc/connections.toml` (connections added in the TUI or dbc web).
The DSN, its `${VAR}` expansion and the TLS settings all come from that
entry. A script has no way to define a connection of its own. The name
also gives the driver, which decides how the rows move (below). Each
connection's pool opens on first use, and a name dbc doesn't know fails with
`unknown connection`.

On Postgres and MySQL, `<conn>/<database>` names another database on the
same server, with the connection's credentials and TLS settings. For example,
`s.Copy("prod-pg/reporting", "local-pg", …)` reads from the `reporting`
database through `prod-pg`. A configured connection whose name matches
exactly always wins.

| Method | Purpose |
| --- | --- |
| `s.Copy(src, dst, table, sdb.CopyOpts{…}) (sdb.CopyStats, error)` | Copy a table (or query) from one connection to another |
| `s.Reader(conn, sql, args...) (*sdb.Reader, error)` | Stream a query's rows: `for rd.Next() { row := rd.Row() }`, then `rd.Err()` |
| `s.Writer(conn, table, cols, sdb.WriteOpts{…}) (*sdb.Writer, error)` | Load rows in one transaction: `w.Write(row)` …, `w.Close()` commits, `defer w.Abort()` |

`sdb.CopyOpts` fields: `To` (destination table, default the same name),
`Columns`, `Where` + `Args`, or a whole `Query` instead of a table; `Create`
(make the destination when missing), `Truncate` (empty it first);
`Transform func(row []any) ([]any, error)` to edit each row in Go (return
`nil` to skip it); `ProgressEvery` (print the running count) or `Progress`
(your own callback); `BatchSize`. `sdb.WriteOpts` has `Setup` (statements
run first, in the load's transaction), `Truncate` and `BatchSize`.

How it moves the rows:

- **Postgres → Postgres** with no `Transform` and no `Args` streams
  `COPY (SELECT …) TO STDOUT` straight into `COPY … FROM STDIN`. The rows are
  never decoded, so it is lossless for every type both servers know (arrays,
  jsonb, ranges, enums) and runs at the speed of the servers and the network.
  `COPY` cannot take bind parameters, so `Args` goes row by row.
- **Into Postgres** otherwise: `COPY … FROM STDIN` in text format, so the
  server parses each value as it would a literal — `"42"` from MySQL loads
  into an `integer`. Besides what a `Reader` yields, a `Transform` or a
  `Writer` may hand over a Go slice (an array: `[]string` into `text[]`,
  `[][]int` into `int[][]`), a map or struct (JSON, for `jsonb`), a
  `time.Duration` (an `interval`), a pointer (`nil` is NULL), or an
  `sql.NullString` or other `driver.Valuer`.
- **Into MySQL, SQLite or bytdb**: multi-row `INSERT` batches (500 rows,
  fewer for wide tables), the full-batch statement prepared once.

The destination loads in **one transaction**, together with `Create` and
`Truncate`. A failed, stopped (`Ctrl+K`) or panicking copy leaves it as it
was: the old rows still there, a table it was creating not created. Two
engines can't do DDL that way: on bytdb and MySQL the `CREATE TABLE` runs
just before the load, so a failure there can leave an empty new table, but
never a partial one. MySQL's `Truncate` is a `DELETE`, since its `TRUNCATE`
commits on the spot. So is Postgres's when source and destination are the
same database (one connection, or two names for it): `TRUNCATE`'s lock
would stall the copy's own read of the table, so a table copied onto itself
to rewrite it through a `Transform` works — at `DELETE`'s speed on a big
table.

A `Query` may be a write with `RETURNING`, which makes the copy a move:

```go
st, err := s.Copy("prod-pg", "archive-pg", "", sdb.CopyOpts{
	Query: "DELETE FROM jobs WHERE finished_at < now() - interval '90 days' RETURNING *",
	To:    "jobs_archive", Create: true,
})
```

From Postgres, the source's change commits only after the destination's load
has. A copy that fails, even at the load's final commit, leaves the rows
where they were. MySQL, SQLite and bytdb commit the write as it runs, before
the load. A write without `RETURNING` has nothing to copy and is refused
before it runs.

Reading from Postgres pins the settings that shape a value's text —
`DateStyle` to ISO, `IntervalStyle` to `postgres`, `extra_float_digits` to
exact — for that read only. A copy between two servers configured
differently (one with `DateStyle = 'SQL, DMY'`, say) then moves dates,
intervals and floats unchanged rather than swapping day and month. A
`Query` may end in `;` or a comment, but holds one statement.

`Create` copies a Postgres table to Postgres with its exact column types,
`NOT NULL`s and primary key. Between other engines each column gets a broad
type (integer, float, numeric, boolean, date, timestamp, bytes, or text) and
the source's primary key, where it can be read. A Postgres `Query` copied
to Postgres keeps each column's type name but not its length or precision
(`varchar(80)` becomes `varchar`), and a user type — an enum, a
composite — becomes `text`, since it may not exist on the destination.
Defaults, sequences, other indexes and constraints are not copied. bytdb requires a primary key, so to
copy a query into bytdb, create the table first. bytdb has no exact decimal
type, so a numeric column created there is `double precision`: values with
more than about 15 significant digits get rounded. To keep every digit,
create the table yourself with a `text` column. That works from Postgres
and MySQL, whose numerics arrive as text, but not from SQLite.

A `Reader` has no `max_rows` cap and keeps values typed: `int64`,
`float64`, `bool`, `string`, `time.Time`, `[]byte` for binary columns, `nil`
for NULL. Postgres `numeric`, `uuid`, intervals, arrays and ranges arrive
in their text form, the same from every server (ISO dates, exact floats). A
`Reader` or `Writer` a script leaves open is closed, or rolled back, when
`Run` returns. A Writer is never committed unless the script calls `Close`.
[`scripts/copy_table.go`](scripts/copy_table.go) shows all three levels, up
to a join across two connections. To copy a single table, with no script,
use [`dbc copy`](#copy-headless).

One interpreter quirk: yaegi silently drops a two-value assignment into a map
element. `m[k], _ = v.(string)`, `m[k], _ = other[k]` and `m[k], _ = f()`
store nothing, and `m[k], _ = <-ch` panics. Assign to a variable first, then
store it in the map. Present in yaegi v0.16.1 and on master as of 2026-10;
tracked upstream in
[traefik/yaegi#1655](https://github.com/traefik/yaegi/issues/1655).

### Pipelines

A pipeline is the ETL above as data: **fragments** in order, each a tree of
**nodes** — one source, transforms along the way, sinks at the leaves — or
a single rowless action. Inside a fragment rows move in **batches** (1000
by default): the source yields one, each transform reshapes it, each sink
loads it, and the sinks commit together at the fragment's end, so a
fragment is all-or-nothing per sink. Nothing passes between fragments but
what the sinks committed and a few named values, which is what makes a
fragment restartable on its own and a pipeline readable from any SQL
console. Pipelines live in `~/.config/dbc/pipelines/<name>.json` (or
`pipelines_dir` in the config, resolved like `scripts_dir`):

```json
{
  "name": "clean-and-load",
  "params": { "min_age": { "default": "2", "doc": "The youngest cat to take" } },
  "fragments": [
    { "name": "clean", "batch": 500,
      "nodes": [
        { "id": "src",  "plugin": "sql.read",     "cfg": { "conn": "demo-sqlite",
            "query": "SELECT id, name, breed, age FROM cats WHERE age >= ${min_age}" } },
        { "id": "tidy", "plugin": "go.transform", "cfg": { "code": "func Apply(b *sdb.Batch) (*sdb.Batch, error) { … }" } },
        { "id": "dst",  "plugin": "sql.write",    "cfg": { "conn": "demo-sqlite", "table": "cats_clean", "create": "true", "truncate": "true" } },
        { "id": "peek", "plugin": "preview",      "cfg": { "rows": "20" } }
      ],
      "edges": [ ["src", "tidy"], ["tidy", "dst"], ["tidy", "peek"] ] },
    { "name": "stamp",
      "nodes": [ { "id": "log", "plugin": "sql.exec", "cfg": { "conn": "demo-sqlite",
          "sql": "INSERT INTO pipeline_log VALUES ('clean', ${frag.clean.rows}, datetime('now'))" } } ] }
  ]
}
```

A node is a **plugin** with a config; every value is a string, and `${…}`
in one is a parameter (`${min_age}`) or a value an earlier fragment
published (`${frag.clean.rows}`; `sql.exec` publishes `affected`). The
plugins, and the fields each takes, are listed by `dbc plugins`:

| Plugin | Kind | Does |
| --- | --- | --- |
| `sql.read` | source | A query on a connection, streamed and uncapped; `args` bind its placeholders |
| `sql.table` | source | A table, with `columns`, `where` and `order` |
| `go.source` | source | `func Next(e *sdb.Env) (*sdb.Batch, error)` until it returns nil |
| `cols.select` | transform | `keep`, `drop`, `rename` (one `old=new` per line) |
| `rows.filter` | transform | Rules, one per line: `age >= 3`, `breed in Tabby,Siamese`, `name like %kit%`, `x notnull` |
| `rows.limit` | transform | The first N rows, then the source stops |
| `go.transform` | transform | `func Apply(b *sdb.Batch) (*sdb.Batch, error)`, per batch |
| `sql.write` | sink | A table, by COPY on Postgres and batched INSERT elsewhere, in one transaction; `create`, `truncate`, `key`, `setup` |
| `preview` | sink | The first N rows to the results view; nothing written |
| `sql.exec` | action | Statements on a connection; publishes `affected` |
| `go.action` | action | `func Run(s *sdb.S) error` — a script, inline |
| `script.run` | action | A saved script by name: every script written so far is a valid fragment |

A `go.*` node's code is interpreted by the same engine scripts are, with
the standard packages it uses by name imported for it, and called once
per **batch**: the row loop inside `Apply` runs at the interpreter's
speed (about 15× slower than compiled Go for a lower-case-and-trim of one
column — half a millisecond per thousand rows, or some two million rows a
second), but nothing crosses the interpreter boundary per row. A fragment
that is exactly `sql.read` or `sql.table` on Postgres into `sql.write` on
Postgres, nothing between, runs as `s.Copy`'s direct COPY and says so.

A script runs a pipeline by name, or builds one with its own funcs as
nodes — the one thing the JSON cannot say:

```go
st, err := s.RunPipelineNamed("clean-and-load", sdb.PipelineOpts{Params: sdb.Params{"min_age": "5"}})

p := sdb.NewPipeline("adhoc")
f := p.Fragment("cats").Batch(2000)
f.Node("sql.read", sdb.Cfg{"conn": "prod", "query": "SELECT id, email FROM users"})
f.ThenFunc(func(b *sdb.Batch) (*sdb.Batch, error) {
	c := b.Col("email")
	for i := range b.Rows {
		if e, ok := b.Rows[i][c].(string); ok {
			b.Rows[i][c] = strings.ToLower(e)
		}
	}
	return b, nil
})
f.Then("sql.write", sdb.Cfg{"conn": "local", "table": "users_copy", "create": "true", "truncate": "true"})
st, err = s.RunPipeline(p, sdb.PipelineOpts{})
s.Print("%s", st) // pipeline adhoc: 1 fragment, 48213 rows in 1.4s (succeeded)
```

`sdb.Batch` is `Cols []sdb.Col` and `Rows [][]any`, with `Col(name)`,
`Get`, `Set`, `AddCol`, `Drop`, `Keep`, `Rename`, `Filter` and `Clone`.
The stats name every fragment and node with rows in and out, batches and
time, and `dbc pipeline export NAME` writes any pipeline file as the
script above, to run or edit as one. Three examples are built in
(`dbc pipelines` lists them): `copy-cats`, `clean-and-load` and
`cats-report`, all on the demo connections, and a fourth, `breed-counts`,
for the example job. `dbc web` draws and edits pipelines on a canvas
([below](#pipeline-tabs-in-dbc-web)); [jobs](#jobs) put pipelines in a
dependency graph, on a schedule.

### Pipeline tabs in dbc web

`Ctrl+O` (▷ Scripts) lists your pipelines and the examples beside the
scripts. Copy an example, or **+ New ▾ → Pipeline**, and it opens in a
**pipeline tab**: a canvas in the editor's place, the grid and the log
below it as for a query.

```
┌ ⛓ clean-and-load.json ●   rows [50] ◎ Preview ⊡ Fit { } JSON ⇪ Go ⋯ ┐
├ palette ──┬ canvas ──────────────────────────────────────┬ inspector ┤
│ SOURCES   │ ✓ clean · batch 500 · 5 rows        ◎ ▶ ⋯   │ sql.write │
│ ⇥ sql.read│ ┌─────┐   ┌──────┐   ┌──────┐   ┌──────┐     │ dst       │
│ TRANSFORMS│ │ src ●──►● tidy ●──►● keep ●┬─►● dst  │     │ conn  […] │
│ ƒ go.trans│ └─────┘   └──────┘   └──────┘│  └──────┘     │ table […] │
│ SINKS     │                              └─►● peek │     │ create[x] │
│ ⇤ preview │ ✓ stamp                                     │           │
└───────────┴──────────────────────────────────────────────┴───────────┘
```

- **Build it by dragging.** Drag a plugin from the palette onto a lane to
  add a node there (onto the empty canvas: a new fragment holding it); a
  click adds it after the selected node, wired. Drag a card's output ● to
  another card to wire them — rows flow from one into the other; a node
  takes one input, so a new wire replaces the old one, and a loop is
  refused. Click a card, a wire or a lane to select it; `Delete` removes
  it, `Ctrl+D` duplicates a node. Drag the background (or use the wheel)
  to pan, `Ctrl`+wheel (or pinch) to zoom, `f` or ⊡ Fit to fit. A lane's
  ⋯ moves, adds or deletes fragments.
- **The inspector** is drawn from the plugin's own fields: a connection
  field offers the configured connections (a click on one in the sidebar
  sets it on the selected node), a columns field the columns a preview
  saw, SQL and Go fields are code boxes. With nothing selected it edits
  the pipeline's name, description and parameters.
- **Checked as you edit.** Half a second after a change the canvas is
  checked as `dbc pipeline check` would: what is wrong is marked ⚠ on the
  card, the lane or the header (`· 1 error`) and listed in the inspector;
  ✓ Check logs every finding.
- **◎ Preview** runs the canvas as it is — unsaved edits too — on real
  data, writing nothing: every sink becomes a preview, the source stops
  after `rows`, actions are skipped. Each branch's rows land in the grid
  ("Result 1 · 2" for two). A lane's ◎ previews that fragment alone.
- **▶ Run** (`Ctrl+Enter`) saves, then runs the file — what `dbc pipeline
  run` would run. Runs belong to dbc web, not to the tab: the tab is
  marked busy while one it started runs, and its ■ Stop rolls the
  fragment in flight back, but the tab is never blocked, and two
  pipelines run at once (one run of a pipeline at a time). Each card
  counts rows in → out as it goes, each lane shows its state (○ queued,
  ● running, ✓, ✗, ↷ skipped, ■ stopped), and the run's lines — fragments
  starting and ending, `e.S.Print` in a Go node, the DDL log — go to the
  pipeline's log. A parameter without a default is asked for; "Run with
  parameters…" (the tab's menu) asks for all of them.
- **Saving** is the script tab's: explicit (`Ctrl+S`, ⤓ Save, or Run),
  `⛓ name.json ●` while unsaved, the unsaved text kept in the browser
  across a reload, and a file changed on disk meanwhile is not written
  over — you choose to keep yours or load the file's. A canvas edit writes
  the file the way `dbc` does (two-space indent, keys sorted, edges on one
  line), so it diffs well in git.
- **{ } JSON** shows the same pipeline as JSON in the editor (with the
  check's findings marked on their lines), to edit by hand; ⊞ Canvas goes
  back once it parses. **⇪ Go** exports it as a dbc script in a script
  tab — the builder form `dbc pipeline export` writes.

Double-click the tab to rename the pipeline. One pipeline has one tab.

### Jobs

A **job** is pipelines in a dependency graph with one root: a pipeline
starts when every pipeline it waits for (`after`) has succeeded, so one
root fans out to several pipelines at once and several fan back in to
one. It runs by hand, on a **schedule** while `dbc web` is up, from a
**webhook**, from cron through `dbc job run`, or from a script. Jobs live
in `~/.config/dbc/jobs/<name>.json` (`jobs_dir`):

```json
{
  "name": "nightly",
  "root": "copy",
  "params": { "min_age": { "default": "2" } },
  "pipelines": [
    { "id": "copy",   "pipeline": "copy-cats" },
    { "id": "clean",  "pipeline": "clean-and-load", "after": ["copy"], "params": { "min_age": "${min_age}" } },
    { "id": "breeds", "pipeline": "breed-counts",   "after": ["copy"] },
    { "id": "report", "pipeline": "cats-report",    "after": ["clean", "breeds"] }
  ],
  "triggers": { "schedule": ["0 2 * * *"], "tz": "Europe/Paris", "catch_up": false, "webhook": true },
  "policy": { "on_failure": "finish_branches", "max_parallel": 2, "overlap": "skip", "timeout": "2h" }
}
```

```
        copy                 copy runs first; clean and breeds side by side
       ╱    ╲                (max_parallel 2); report when both succeeded
   clean    breeds
       ╲    ╱
       report
```

- **Steps** name a pipeline (`pipelines_dir`, then the examples; `.json`
  optional) and may set its params. A step's param values may use the
  job's own params (`${min_age}`) and the run's values: `${run.date}`
  (`YYYY-MM-DD`), `${run.time}`, `${run.started}`, `${run.id}`,
  `${run.trigger}`, `${run.job}` — which any pipeline node may use too.
- **policy.on_failure**: `finish_branches` (the default) skips everything
  downstream of a failed pipeline and lets the other branches finish; the
  job ends failed. `stop` cancels the run at once.
- **policy.max_parallel** (default 2): how many pipelines run at once. A
  running fragment holds a reader and a writer connection, so a fan-out
  wider than the pool waits rather than fails.
- **policy.overlap**: a start while the job is running is `skip`ped (the
  default; a scheduled one is logged as skipped) or `queue`d to run when
  that one ends (one waits at most). One run of a *pipeline* at a time
  holds everywhere: a job step whose pipeline is already running fails,
  and a pipeline tab's ▶ Run is refused while a job's step runs it.
- **policy.timeout** cancels the run after so long; the job ends failed.
- **triggers.schedule** is cron lines — `minute hour day-of-month month
  day-of-week`, with `*`, lists, ranges, `*/15`, `jan`–`dec`, `sun`–`sat`
  and `@daily`, `@hourly`, `@weekly`, `@monthly`, `@yearly` — in `tz` (the
  machine's zone when left out). When both day fields are set, either
  matches, as in crontab. On a daylight-saving day a time the clocks skip
  fires as they jump (`30 2 * * *` at 03:00) and a time they repeat fires
  once. Only a running `dbc web` fires schedules, and only for the jobs
  in `jobs_dir` (never an example); two of them on one machine (dbc.app
  and a terminal's) fire each time once between them. A fire it was down or asleep for is
  not run late unless `catch_up` is on; then it runs once, as soon as dbc
  web sees it.
- **triggers.webhook** opens `POST /api/v1/jobs/<name>.json/run` to a
  caller with the launch secret (`dbc web --secret …`):
  `curl -X POST -H "Authorization: Bearer $SECRET" -d '{"params":{"min_age":"3"}}' http://127.0.0.1:7777/api/v1/jobs/nightly.json/run`.
  A job that does not set it answers 403; the page's own Run needs nothing.
- **From a script**, `run, err := s.RunJob("nightly", sdb.Params{"min_age": "3"})`
  runs the job and waits for it; `run.Pipelines` has each step's stats. In
  `dbc web` it runs on the server's engine, beside its other runs.

**Run records.** Every run of a job or a pipeline (not a preview) leaves
`~/.config/dbc/runs/<job|pipeline>/<name>/<run id>.json` (`runs_dir`):
who started it, every step's, fragment's and node's state and counters,
and its log. It is rewritten every two seconds while the run goes, so a
crash leaves a record the next dbc reads as **interrupted**, and the
newest `runs_keep` (200) per name are kept. Any process reads them —
`dbc runs` from a shell sees what `dbc web`'s schedule ran, and the
other way round. In dbc web they are the [Runs view](#job-tabs-and-the-runs-view-in-dbc-web).

### Job tabs and the Runs view in dbc web

`Ctrl+O` lists your jobs (with when each fires next and how its last run
ended) and the examples beside the scripts and pipelines. Copy an
example, or **+ New ▾ → Job**, and it opens in a **job tab**: its DAG on a
canvas in the editor's place, a palette of your pipelines and the
examples on the left, an inspector on the right.

```
┌ ⧉ nightly.json ●                       ⊞ Design ◷ Runs ⊡ Fit { } JSON ⋯ ┐
├ pipelines ┬ canvas ─────────────────────────────────────────┬ inspector ─┤
│ YOURS     │ ┌ copy ◉ ───┐     ┌ clean ─────┐   ┌ report ──┐ │ job        │
│ orders    │ │⛓ copy-cats●┼──┬─►●⛓ clean-and…●┼─┬►●⛓ cats-re… │ │ schedule   │
│ EXAMPLES  │ └───────────┘  │  └────────────┘ │ └──────────┘ │ 0 2 * * *  │
│ copy-cats │                │  ┌ breeds ────┐ │              │ next: Fri… │
│ …         │                └─►●⛓ breed-co…●┼─┘              │ policy …   │
└───────────┴──────────────────────────────────────────────────┴────────────┘
```

- **The cards are placed for you**, left to right by how far downstream
  each step is, ordered to keep the dependencies from crossing — the same
  layout the ERD uses, worked out by dbc web. There is nothing to arrange:
  a step's place says what it waits for.
- **Build it by dragging.** Drag a pipeline from the palette onto a card
  to add a step that waits for that card's; onto the canvas, a step after
  the selected one (the first step is the job's root, `◉`); a click adds
  it after the selected step. Drag a card's output ● to another card to
  make that one wait for this one too — a loop is refused. Click a card or
  a dependency to select it; `Delete` removes it, `Ctrl+D` duplicates a
  step. The canvas pans, zooms and fits as a pipeline's does.
- **The inspector**, with nothing selected, edits the job: its name,
  description and params; its **schedule** — each cron line shows its next
  five fire times as you type, so you see what you wrote — its time zone,
  catch-up and **webhook** (with its URL and a ⧉ curl command); and its
  policy. A step's inspector sets its pipeline (↗ opens it), what it
  waits for, and the pipeline's params, each with its default and doc.
- **Checked as you edit**, as `dbc job check` would: every step's pipeline
  is found and checked too, and what is wrong is marked ⚠ on its card.
- **▶ Run** (`Ctrl+Enter`) saves, then runs the job in dbc web's engine.
  The tab turns to its **◷ Runs** face, on the new run's page, live; ■ Stop
  stops it. A preview sink in one of its pipelines lands its rows in the
  tab's grid. Back on **⊞ Design**, each card shows its step's state in
  the job's newest run — this tab's, a scheduled one, another window's.
- Saving, drafts, `{ } JSON` and rename are the pipeline tab's.

**The Runs view** — `Alt+R` or ◷ Runs on the top bar, for every run; a job
tab's ◷ Runs for that job's — lists the run records (any process's: a
cron's `dbc job run` too), newest first, filtered by job or pipeline,
status and age, with live rows for what runs now. A click opens a **run
page** that drills down:

```
‹ Runs  ✓ succeeded nightly · job · manual · 02:00:00 · 3m 12s · 1,204 rows   ⧉ id ↗
┌ the DAG: each step coloured by its state, its time and rows; the ──────┐
│ critical path (the chain that set when the run ended) drawn bold      │
├ ⛓ clean · clean-and-load ✓ — its fragments as bars on the run's ──────┤
│   time axis, so the wait before a step and its fragments' order show │
├ ▤ load — its nodes: rows in and out, batches, rows/s, time, the error ┤
│   pinned to the node that raised it                                   │
├ log · clean/load — the run's log, narrowed to what is picked ─────────┤
└───────────────────────────────────────────────────────────────────────┘
```

A running run's page moves with it (one another process runs is read
again every two seconds); ■ Stop stops it; ↗ opens its job or pipeline;
◎ Open preview goes to the tab whose grid holds a preview sink's rows.
`Backspace` goes back to the list.

## Headless mode

Everything works without the TUI, for cron jobs and shell pipelines:

```sh
./dbc "SELECT * FROM cats"                       # aligned text table
./dbc -c local-pg -t csv "SELECT * FROM users"   # any format to stdout
./dbc -t html -o report.html "SELECT ..."        # straight to a file
./dbc "INSERT ...; SELECT ..."                   # several statements, in order
./dbc -f report.sql                              # the SQL in a file
./dbc -c local-pg < report.sql                   # … or piped to stdin
./dbc script scripts/loop_params.go              # run a Go script
./dbc script copy_mytable                        # … one from scripts_dir, by name
./dbc script --check copy_mytable                # check it without running it
./dbc scripts                                    # list scripts_dir (and say where it is)
./dbc copy --from prod --to local --create orders # copy a table across connections
./dbc dump -c prod -t split -j 4 -o prod-sql     # dump a Postgres database, a file per table
```

| Flag | |
| --- | --- |
| `-f`, `--file FILE` | read the SQL from a file; `-f -` reads stdin |
| `-t`, `--format FORMAT` | `text` (default), `csv`, `tsv`, `markdown`, `html`, `json` |
| `-o`, `--out FILE` | write the output to a file instead of stdout |
| `-c`, `--conn NAME` | which connection to run on |
| `--tx` | run the statements in one transaction: all commit, or none do |
| `-k`, `--keep-going` | go on past a failed statement instead of stopping there |
| `--config FILE` | config file (default `./dbc.toml`, then `~/.config/dbc/config.toml`) |
| `--demo bytdb\|sqlite` | which demo starts active when there is no config (also `$DBC_DEMO`) |
| `--driver NAME --dsn STRING` | a one-off connection that is in no config file |
| `--tls MODE` (`--tls-ca`, `--tls-cert`, `--tls-key FILE`, `--tls-key-password '${VAR}'`) | [TLS](#tls) for the `--dsn` connection, overriding the DSN's own |

`dbc --help` lists them all. Long names take one dash or two (`-dsn` works),
and flags may go before or after the SQL. With no SQL argument and no `-f`,
dbc reads the SQL from stdin when stdin is a pipe or a file, and otherwise
opens the TUI. Give the SQL as one quoted argument: more than one argument is
an error rather than a silently shortened query. Use `--` before SQL that
starts with a comment, so the flag parser leaves it alone. `Ctrl+C` cancels a
running query or script and exits 130; bad usage exits 2.

`-f` was the output format before it was the SQL file; `dbc -f csv …` now
says to use `-t csv` rather than looking for a file named `csv`.

### Copy headless

`dbc copy` is `s.Copy` without the script, for a cron job or a shell
pipeline:

```sh
./dbc copy --from prod --to local public.orders                  # into an existing table
./dbc copy --from prod --to local --create public.orders         # … created when missing
./dbc copy --from prod --to local --truncate public.orders       # … emptied first
./dbc copy --from prod --to local public.orders archive.orders   # … under another name
./dbc copy --from prod --to local --truncate \
    --where "created_at >= now() - interval '1 day'" orders      # only some rows
```

| Flag | |
| --- | --- |
| `--from NAME`, `--to NAME` | source and destination connections (both required; may be the same one, copying to a different table). A `<conn>/<database>` name works as it does in `s.Copy` |
| `--create` | create the destination table when it does not exist (see `Create` under [ETL](#etl-across-connections)) |
| `--truncate` | empty the destination first, in the load's transaction |
| `--where CONDITION` | copy only the source rows that match, written in the source's SQL |

A second table name is the destination table; without it, the destination
has the source's name. The copy is the same as `s.Copy` in a script:
Postgres to Postgres streams `COPY` to `COPY`, and the destination loads in
one transaction, so a failed or interrupted (`Ctrl+C`, exit 130) copy leaves
it as it was. On success it prints one line to stdout
(`copied 179982 rows pg1:public.orders → pg2:public.orders in 405ms (direct COPY)`).
When stderr is a terminal, it also prints the row count there every
100,000 rows. A cron job gets only the summary line. Exit status: 0
copied, 1 failed, 2 bad usage, including an unknown connection name or a
table copied onto itself. `-c`, `-f`, `--tx`, `-k`, `-t` and `-o` are
refused, because a copy has no use for them.

### Explain headless

```sh
./dbc explain "SELECT * FROM orders WHERE user_id = 42"   # tree + findings
./dbc explain -a -f slow.sql                              # analyze
./dbc -c pg explain -t json "SELECT …" | jq .insights     # for tooling
./dbc explain -t html -o plan.html "SELECT …"             # the interactive page
./dbc explain --open "SELECT …"                           # … straight into the browser
./dbc explain -t pdf -o plan.pdf "SELECT …"               # graph + findings, to send
./dbc explain -t jpeg -o plan.jpg "SELECT …"              # … as a picture (png too)
./dbc explain -t pdf --theme light -o plan.pdf "SELECT …"  # … on paper, to print
./dbc explain -t mermaid "SELECT …" | pbcopy              # a chart for a PR or a wiki
```

`--fail-on warn` (or `crit`) exits **3** when the plan has a finding that
severe, which turns the findings into a CI gate: keep the queries that matter
in a folder and fail the build when one of them starts scanning a big table.

```sh
for q in queries/*.sql; do ./dbc -c staging explain --fail-on warn -f "$q" || exit 1; done
```

`explain` takes one statement. `-t` is `text` (colored on a terminal, unless
`NO_COLOR` is set), `markdown`, `json`, `html`, `pdf`, `jpeg`, `png`, or
`mermaid`. The PDF and pictures are bytes: write them with `-o`, or pipe
them — dbc refuses to print them on a terminal. They are dark unless
`--theme light` (or `plan_theme = "light"` in the config) asks for paper.

### Diagrams headless

```sh
./dbc erd                                       # Mermaid erDiagram source for the whole connection
./dbc erd -t markdown >> docs/schema.md         # … in a ```mermaid fence, which GitHub draws
./dbc -c pg erd -t png -o schema.png            # the diagram as a picture (jpeg too)
./dbc erd --open                                # … straight into your image viewer
./dbc erd --table orders -t png -o orders.png   # orders and the tables one key away
./dbc erd --table orders --depth 2 …            # … two hops out (-1: all it connects to)
./dbc erd --views …                             # views too
./dbc erd -t png --theme light -o schema.png    # on paper rather than slate
```

`-t` is `mermaid` (the default), `markdown`, `png` or `jpeg`. The pictures
are bytes: write them with `-o`, or pipe them. dbc refuses to print them on
a terminal. `--table` takes a name as the sidebar shows it (`schema.name`
on a connection with several schemas), and can be repeated or
comma-separated. A table that isn't there is a usage error (exit 2).

### Dumps headless (Postgres)

`dbc dump` runs PostgreSQL's own `pg_dump` against a configured connection,
so you don't have to restate its host, credentials and TLS settings for
`pg_dump`. The TUI and `dbc web` do the same from a form
([Dumping a database](#dumping-a-database)):

```sh
./dbc dump -c prod -o prod.sql                          # plain SQL, for psql
./dbc dump -c prod | gzip > prod.sql.gz                 # … to stdout
./dbc dump -c prod -t custom -o prod.dump               # one archive, for pg_restore
./dbc dump -c prod -t directory -j 4 -o prod.d          # a file per table, 4 tables at once
./dbc dump -c prod -t split -j 4 -o prod-sql            # plain SQL, a file per table
./dbc dump -c prod/analytics --schema-only -o s.sql     # another database on that server
./dbc dump -c prod --table 'sales.*' --exclude-table-data audit_log -o sales.sql
./dbc dump -c prod -o x.sql -- --no-comments --lock-wait-timeout=10s   # any other pg_dump option
./dbc dump -c prod -t split -o out --dry-run            # print the commands, run nothing
```

`-t` picks the format:

| `-t` | writes | restore with |
| --- | --- | --- |
| `plain` (default) | one SQL file, or stdout without `-o` | `psql -X -d TARGET -f prod.sql` |
| `custom` | one compressed archive (binary: `-o` or a pipe) | `pg_restore -d TARGET prod.dump` |
| `tar` | one tar archive (binary: `-o` or a pipe) | `pg_restore -d TARGET prod.tar` |
| `directory` | a directory: a file per table plus `toc.dat` | `pg_restore -j 4 -d TARGET prod.d` |
| `split` | a directory of plain SQL, a file per table | `psql -X -d TARGET -f prod-sql/restore.sql` |

`directory` and `split` write several files and dump several tables at once
with `-j N`. They need `-o` naming a directory that doesn't exist yet or is
empty. The `split` layout is dbc's own:

```
prod-sql/
  restore.sql          \ir's the rest in order; psql runs it from any directory
  1-pre-data.sql       schemas, types, tables, functions, sequences
  2-data/
    public.cats.sql    one table's rows each
    sales.orders.sql
  2-data-other.sql     sequence positions, large objects (when there are any)
  3-post-data.sql      indexes, constraints, foreign keys, triggers, matview refresh
  archive/             only with --keep-archive: the directory dump it was made from
```

It is made from **one** `pg_dump -Fd` run, which `pg_restore` then writes
out as SQL a part at a time. So every table comes from the same snapshot, as
in any single pg_dump. Separate `pg_dump -t` runs per table would each see
the data at a different moment. Table names that would collide as file names
(`Cats` and `cats` on a case-insensitive disk) get the entry's id appended. A
failed or canceled split removes what it wrote.

| Flag | pg_dump's | |
| --- | --- | --- |
| `-j`, `--jobs N` | `--jobs` | tables at once (`directory`, `split`), sharing one snapshot |
| `--schema-only` / `--data-only` | same | definitions only / rows only |
| `--schema`, `--exclude-schema PATTERN` | `-n`, `-N` | repeatable; pg_dump's patterns (`'sales*'`) |
| `--table`, `--exclude-table PATTERN` | `-t`, `-T` | repeatable (`-t` is dbc's format flag, hence the long names). As in pg_dump, `--table` dumps just the tables, not their schema: restore into a database that has it |
| `--exclude-table-data PATTERN` | same | keep the table, not its rows |
| `--clean`, `--if-exists`, `--create` | same | `plain`; `split` only with `--create` (drop and recreate the database) |
| `--no-owner`, `--no-privileges` | same | no `ALTER … OWNER`; no `GRANT`/`REVOKE` |
| `--inserts`, `--column-inserts` | same | rows as `INSERT`s instead of `COPY` |
| `--compress L` | same | `6`, or `zstd:3`/`lz4`/`gzip:9` with pg_dump 16+ |
| `--encoding ENC` | same | |
| `--keep-archive` | | `split`: keep `archive/` too |
| `--pg-bin DIR` | | where `pg_dump` and `pg_restore` are (also `$DBC_PG_BIN`) |
| `--dry-run` | | print the command(s); connects to nothing |

dbc refuses flags `pg_dump` would quietly ignore. For example, `--clean` or
`--no-owner` with an archive format: those are `pg_restore` options for that
format. Anything else goes to `pg_dump` after `--`, except the options dbc
sets itself (`--file`, `--format`, `--dbname`, `--host`, `--jobs`, …).

**Which `pg_dump`.** `pg_dump` refuses a server newer than itself. dbc asks
the server for its version and uses the first `pg_dump` that is new enough. It
tries `--pg-bin`, `$DBC_PG_BIN` or the config's `pg_bin` if you set one, then `PATH`, then the
usual install directories: Homebrew's keg-only `libpq` and `postgresql@N`,
Postgres.app, Debian's `/usr/lib/postgresql/N/bin` and RHEL's
`/usr/pgsql-N/bin`. With none installed, `brew install libpq` (macOS) or
`apt install postgresql-client` provides them.

**The connection.** dbc passes the connection to `pg_dump` the way dbc would
open it: the DSN (URL or `key=value`), the connection's [TLS](#tls) keys, and a
`NAME/otherdb` database. These go in a libpq service file only you can read,
and the password goes in `PGPASSWORD`. Neither appears on `pg_dump`'s command
line, where `ps` would show it. A `tls_key_password` becomes libpq's
`sslpassword`. `verify-full` with no CA named uses the system trust store
(`sslrootcert=system`, libpq 16+), as dbc does. DSN keys libpq doesn't take
(session settings like `search_path`) are left out with a note. pgx's pool
settings are left out silently.

`pg_dump`'s messages go to stderr as they come. With `-o`, a summary line
saying how to restore goes to stdout. `Ctrl+C` interrupts `pg_dump` and exits
130. A connection that isn't Postgres is a usage error (exit 2).

### Scripts headless

`-t` and `-o` apply to scripts too (`-f` does not, since a script is its own
input; neither do `--tx` and `-k`, since a script runs its statements itself):

```sh
./dbc -t json script scripts/loop_params.go        # one JSON array on stdout
./dbc -t csv -o report.csv script scripts/loop.go  # straight to a file
./dbc script nightly_report                        # by name, from scripts_dir
./dbc script loop_params                           # a built-in example, by name
./dbc scripts -t json                              # the scripts and examples, as JSON
./dbc script --check nightly_report                # check, don't run (exit 1 on an error)
./dbc script --check -t json ~/scripts/*.go        # several, diagnostics as JSON
```

The argument is a file path, as it always was. If no such file exists and
the argument has no `/`, it is looked up in `scripts_dir`, with `.go`
optional, and then among the built-in examples (`./dbc script loop_params`
works in a fresh install). A file in the current directory wins over a
script of the same name, and your own script wins over an example of the
same name. Running an example says so on stderr. `dbc scripts` lists name,
modified time, size, description (the comment above `package main`, to its
first sentence) and kind: your scripts (`script`), then the examples no
script of yours shadows (`example`, with no modified time; `null` in JSON).
The directory and the counts go to stderr, so `-t json` stays one document.
`--check` finds names the same way, and names an example
`example:loop_params.go` in its output.

`--check` takes one or more scripts (names or paths) and runs none of them.
It prints one line per problem on stdout, as a compiler does
(`file:line:col: message`, with `warning:` before a warning), and nothing for
a clean script. It exits 1 when any script has an error and 0 when there are
only warnings, so it can gate a scripts repo in CI. `-t json` prints one JSON
array of `{file, line, col, severity, msg}` instead (`[]` when clean). Only
the config is read; nothing is connected.

On stdout, in a block format (`text`, `markdown`, `csv`, `tsv`), each result a
script pushes with `s.Show` is written the moment it is shown, so it lands in
order among the `s.Print` lines around it. A script can show any number of
results, so their banners count up without a total — `#1`, `#2` — where a
multi-statement query's read `2/5`; a script that shows one result still
gets its `#1`. With `-o`, or in `json` or `html`, the results are collected
and rendered together when the script finishes — so `-t json` yields one
array rather than a run of separate documents. A block format written with
`-o` holds exactly what the stream would have written.

`s.Print` output is progress, not data, and streams as it happens. It shares
stdout with a `text` table, but moves to stderr when a machine-readable format
has stdout to itself, so `./dbc -t json script … | jq` parses.

### Pipelines headless

```sh
./dbc pipelines                                   # pipelines_dir and the examples
./dbc pipeline run clean-and-load -p min_age=5    # a path, a name, or an example; -p sets a param
./dbc pipeline run clean-and-load --preview 20    # the first 20 source rows through, shown, nothing written
./dbc pipeline run nightly --fragment merge       # one fragment only
./dbc pipeline run nightly -t json | jq .status   # the run's stats as the document
./dbc pipeline check nightly.json other.json      # diagnostics; exit 1 on an error
./dbc pipeline export nightly -o nightly.go       # the pipeline as a dbc script
./dbc plugins                                     # the node kinds and their fields
```

The log (fragments starting and ending, DDL, a node's own lines) streams
as a script's `s.Print` does; what a `preview` sink shows goes out as a
script's `s.Show` results do; in `text`, one line per fragment follows,
with every node's rows in and out. Names resolve as scripts do: a file
here, then `pipelines_dir`, then an example. Exit 0, 1 on failure, 130
when interrupted — a stop during a load rolls it back first. The run
leaves a record in `runs_dir`, as every run does ([Jobs](#jobs)).

### Jobs headless

```sh
./dbc jobs                                        # jobs_dir and the examples: schedule, next fire, last run
./dbc job run nightly -p min_age=3                # run it here and wait; a path, a name, or an example
./dbc job run nightly -t json | jq .status        # the whole run record as the document
./dbc job check nightly                           # the job and every pipeline it runs; exit 1 on an error
./dbc runs                                        # the run records, newest first (any process's)
./dbc runs --job nightly --status failed --since 7d
./dbc run show 20261009-020000-7f3a               # one run as a tree: job → pipelines → fragments → nodes
./dbc runs --sql "SELECT step, avg(seconds) FROM pipelines GROUP BY step ORDER BY 2 DESC"
./dbc run cancel 20261009-020000-7f3a --secret S  # stop a run of a running dbc web (its schedule's, the browser's)
```

`dbc job run` runs the job in its own process: lines stream with their
step (`[clean] fragment clean done: 7 rows in 3ms`), preview sinks' rows
go out as a script's do, and the run's tree follows. Exit 0 when the job
succeeded, 1 when not, 130 when interrupted (the run is stopped and every
load in flight rolled back first). It is how a crontab drives a job
without `dbc web`: `0 2 * * * /usr/local/bin/dbc job run nightly`.
`--sql` loads the records as four tables — `runs`, `pipelines`
(`run_id`, `step`, …), `fragments` and `nodes`, each with its status,
times, `seconds` and rows — into a throwaway bytdb and runs the query in
any `-t` format.

`dbc run cancel` asks a running `dbc web` to stop a run, through its API:
`--url` (default `http://127.0.0.1:8450`, or `$DBC_WEB_URL`) and the
secret dbc web was started with (`--secret`, or `$DBC_WEB_SECRET` for
both — dbc web keeps a fresh one in memory only otherwise). It waits for
the run to roll back and says how it ended. A run a `dbc job run` is
running belongs to that process: dbc web refuses it, and Ctrl+C there
stops it.

### Multi-statement runs

The SQL argument may hold several statements, split the same way the editor
splits them — semicolons inside strings, comments, and `$$` bodies don't count.
They run in order on one connection, and every result is rendered:

```sh
./dbc "INSERT INTO cats (id, name, breed, age) VALUES (9, 'Zed', 'Tabby', 4);
       SELECT breed, count(*) AS n FROM cats GROUP BY breed ORDER BY n DESC, breed"
```

```
-- 1/2 │ demo-bytdb │ 1 rows affected in 3.73ms
-- INSERT INTO cats (id, name, breed, age) VALUES (9, 'Zed', 'Tabby', 4)
rows_affected
-------------
1

-- 2/2 │ demo-bytdb │ 5 rows in 80µs
-- SELECT breed, count(*) AS n FROM cats GROUP BY breed ORDER BY n DESC, b…
breed       n
----------  -
Tabby       3
Maine Coon  2
Siamese     2
Bengal      1
Sphynx      1
```

In `text`, `markdown`, `csv` and `tsv` on stdout, each result is written as
soon as its statement finishes, so a long run shows its progress. `html` and
`json` are one document around every result, and `-o` writes one file, so
those wait for the run to end. Either way the output is the same.

A run stops at the first statement that fails — later statements are skipped —
but the results before it are still written, and the error names the one that
broke (`statement=2/3`). Exit status is 1 for a failure, 130 for a Ctrl+C.

With `-k` (`--keep-going`) a failed statement is reported on stderr as it
happens and the run goes on to the next. Each result keeps its statement's
number (`-- 3/3` after a failed `2/3`), and the run exits 1 at the end if any
statement failed. A Ctrl+C or a lost connection still stops it.

The statements share one pinned database session, so session-scoped SQL means
what it says across them: nothing is committed until you say so in

```sh
./dbc "BEGIN; UPDATE cats SET age = age + 1; SELECT * FROM cats; COMMIT"
```

and `SET`, `PRAGMA`, and temp tables set up by one statement are still there
for the next. Nothing is wrapped in a transaction for you — without a `BEGIN`
each statement commits on its own — unless you ask with `--tx`:

```sh
./dbc --tx -f backfill.sql
```

dbc then runs `BEGIN` first and `COMMIT` after the last statement, and on the
first failure (or a Ctrl+C) it runs `ROLLBACK` instead and says so on stderr. The
results that ran are still shown, but none of their changes are kept. `--tx`
refuses a buffer that manages its own transaction (`BEGIN`, `COMMIT`,
`ROLLBACK`, `START TRANSACTION`, …) and does not mix with `-k`. On MySQL, DDL
(`CREATE`, `ALTER`, `DROP`, …) commits implicitly, so keep it out of a `--tx`
run there.

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

Postgres server notices (`RAISE NOTICE`, `RAISE WARNING`, "table … does not
exist, skipping") go to stderr as each statement returns, one line each
(`NOTICE: …`), as psql prints them. A notice from a failing statement comes
before its error, and a deferred trigger's notice comes when `--tx` commits.

## Migrations

`dbc migrate` applies versioned SQL migrations in the goose file format, with
goose's `goose_db_version` table — so a database goose has been migrating
carries straight over, and the goose binary stops being a thing to install.

```sh
./dbc -c local-pg migrate status                  # each file, applied or pending
./dbc -c local-pg migrate up                      # apply everything pending
./dbc -c local-pg migrate down                    # roll back the latest
./dbc -c local-pg migrate create add_users_table  # new timestamped file
./dbc -c local-pg -t json migrate version         # for scripts
```

Verbs: `status`, `version`, `up`, `up-by-one`, `up-to VERSION`, `down`,
`down-to VERSION`, `redo`, `create NAME`.

The migrations directory is `--dir`, else the connection's `migrations` setting
in the config, else the working directory:

```toml
[[connection]]
name       = "church-dev"
driver     = "postgres"
dsn        = "postgres://devuser:secret@localhost:5432/church_development?sslmode=disable"
migrations = "db/migrate"
```

With no config file at all, `--driver` and `--dsn` name the database on the
command line — goose's whole invocation, one flag longer:

```sh
./dbc --driver postgres --dsn "$DATABASE_URL" --dir db/migrate migrate up
```

A migration file is `NNN_name.sql` with `-- +goose Up` and `-- +goose Down`
sections. Statements are split by the same scanner the editor uses, so a
`$$`-quoted function body is one statement without help; the goose
`StatementBegin`/`StatementEnd` block and `NO TRANSACTION` annotations are
honored too. Each migration runs in a transaction with its version row, so a
failure leaves neither behind. `up` refuses a pending migration older than the
current version — the merge-of-two-branches case — unless `--allow-missing` is
given, exactly as goose does.

Works on all four engines. bytdb runs DDL outside transactions, so there each
statement commits on its own.

## Export formats

`csv`, `tsv`, `markdown`, `html` (styled standalone page), `json`
(array of objects), `text` (aligned table) — from the `Ctrl+E` dialog
(clipboard or file), from scripts via `s.Export`, or headless via `-t`.

To the **clipboard**, `html` is not the page but a self-styled `<table>`
offered as the clipboard's HTML flavor, so it pastes into Teams, Outlook and
Docs as a table (see *Copying results* above). That holds for a script's
`s.Export(r, "html", "")` too.

The HTML page wears the same muted green as the TUI, surface for surface.
Both read the palette from [`theme/`](theme/theme.go), so a change to those
constants reaches the app and its exports together.

## License

MIT. See [LICENSE](LICENSE).
