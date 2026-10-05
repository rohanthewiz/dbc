# dbc — a TUI database client, scriptable in Go

`dbc` is a terminal database client for Postgres, MySQL, SQLite, and
[bytdb](https://github.com/rohanthewiz/bytdb) with a twist: instead of a
bespoke macro language, you script it in **Go**. Drop a
`.go` file in the scripts directory, loop over parameters, run queries against
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

`go test ./...` runs the unit tests. Three opt-in suites need more than Go:

- **Live databases** (`db/live_*`): set `DBC_LIVE_PG_DSN` and/or
  `DBC_LIVE_MYSQL_DSN` to a throwaway Postgres or MySQL.
- **dbc web in a browser** (`web/e2e`, a module of its own so go-rod never
  enters dbc's dependencies): `cd web/e2e && DBC_E2E=1 go test -count=1 -v .`
  It builds dbc, serves `dbc web` from a temporary HOME on two SQLite files,
  and drives a headless Chrome through it: sign-in, a run, the Tables list,
  Show columns, copies, switching, Disconnect, the connection form and tabs
  across a reload. A JavaScript error on the page fails it. `DBC_E2E_CHROME`
  names the browser, `DBC_E2E_HEADFUL=1` shows it, and `DBC_LIVE_PG_DSN`
  adds the Postgres schema picker.
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
scripts_dir        = "scripts"
max_rows           = 1000   # rows fetched from the server
max_display_rows   = 2000   # rows the results table draws (0 = all)
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

The tables list shows each table's row count once it has been counted —
`cats (8)` in `dbc web`, where hovering says "cats with 8 rows", and a
right-aligned `8` in the terminal. The counts load after the list, never
holding it up, and are cached per connection for two minutes. A run that
may have changed rows (an `INSERT`, `DELETE`, `TRUNCATE`, DDL, a `COMMIT`, a
script) recounts the list straight away, past the cache; a `SELECT`, `SET`
or `BEGIN` does not. In `dbc web` every query tab on that connection, in
any window, is recounted, not just the one that ran it; writes made by
other clients show at the next connect. A write inside an open transaction
shows once it is committed, since the counts are read outside your session. On Postgres
and MySQL a table the database's statistics put at a million rows or more
shows that estimate, marked `~` (`~1.2M`), instead of being counted; the
rest, and every table on SQLite and bytdb, are counted exactly. A count that
takes over three seconds falls back to the estimate, or to no number. Views
are not counted.

When a connection's tables span more than one schema, `dbc web` puts a
schema filter under the Tables heading, so a big catalog can be narrowed to
the one you are working in. Click it (or tab to it) for the list of schemas,
each with its table count; type to narrow the list — names starting with
what you typed come first — then `Enter` or a click picks one, and `Enter`
again moves to its tables. `Esc` drops the typing; emptying the box and
picking "all schemas" (or its `×`) shows everything again. The heading then
reads `· 12 / 340`. The pick is remembered per connection, across reloads
and windows. The heading and the filter stay put while the tables scroll.

On Postgres the sidebar is loaded a level at a time, so connecting to a big
server costs what the sidebar shows, not what the server holds:

- **db**: the server's databases (those you may `CONNECT` to; templates
  left out). Picking one switches the tab to it, using the same host,
  credentials and TLS. A Postgres connection is bound to one database, so
  this opens a connection named `<conn>/<database>` (e.g.
  `ProdDr/analytics`). It shows in the header, and tabs, history and the
  schema pick remember it like any connection. Moving off it closes its
  connection to the server once no tab is on it, so browsing a server's
  databases does not leave one open per database visited.
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
| connections | right-click (or the toolbar's `● conn ▾`) | **Disconnect** the active connection, then the list to connect to |
| tables | click `⛁ db ▾` / `◫ schema ▾` | *(Postgres; MySQL has `⛁ db` only)* pick another of the server's databases / another schema, from a list you can type into |
| tables | click / double-click / right-click | select / preview the first 100 rows / show columns, diagram it, diagram all tables, copy the ERD as Mermaid, insert name, copy name |
| tables | `c` on the selected table | show its columns: its `information_schema.columns` rows (name, type, nullable, default, length) in the grid, ready to copy |
| tables | `e` on the selected table | diagram it and its neighbours (an ERD, below), saved as a PNG and opened |
| editor | click, drag, double-, triple-click | caret, selection, word, line |
| editor | right-click | run, copy, cut, select all, undo, history, ask the assistant |
| results | click, drag, shift-click | cell, rectangular range, extend |
| results | row number click | selects the row |
| results | header click | sorts by that column (again: descending; again: result order) |
| results | header border drag / double-click | resizes the column / fits it to its content (past the 40-cell auto-size cap) |
| results | header right-click | hide that column (or the selected range's); hidden ones are listed there to show again |
| results | double-click | inspects the value in full (JSON is pretty-printed) |
| results | right-click | copy cell / row / range / whole result as **HTML table**, Markdown, CSV, TSV, JSON; sort; hide, fit, show columns; export; ask the assistant |
| any pane | wheel, shift+wheel | scrolls what is under the pointer, vertically / sideways |
| scrollbars | click, drag | jump, drag |
| pane borders | drag | resize the sidebar, the editor/results split, the log, the assistant |
| assistant | `⤓ insert` on a code block | puts that SQL in the editor at the caret |
| results title | click `Results` / `◈ Plan` | switches the results pane between the grid and the plan |
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
| `Ctrl+O` | Pick and run a Go script from `scripts_dir` |
| `Ctrl+P` | Query history — filter, then `Enter` inserts (never runs) |
| `Alt+N` · `Alt+C` | A new console of the database · the database's next console (see [Consoles](#consoles-and-multi-statement-buffers)) |
| `Ctrl+Space` | *(editor)* Suggestions at the caret — they also open by themselves after `.`, `::` and two letters of a word (see below) |
| `Ctrl+T` | List the tables and views on the active connection |
| `Ctrl+L` | Jump to the connections list |
| `x` | *(connections)* Disconnect without picking another connection. If the session may hold a transaction, dbc asks first, because disconnecting rolls it back |
| `d` / `s` | *(tables, Postgres; `d` on MySQL too)* Pick a database / a schema |
| `Ctrl+G` | *(inside Cats)* Hand the statement to an agent in another pane |
| `y` / `Y` / `c` | *(results)* Copy the cell or range / the row / open the copy menu |
| `<` / `>` / `=` | *(results)* Narrow / widen the column / fit it to its content |
| `-` / `+` | *(results)* Hide the column (or the range's columns) / show every hidden column |
| `Enter` | *(results)* Inspect the value under the cursor |
| `p` | *(results)* Switch between the result grid and the plan |
| `z` | *(results)* Give the results pane the whole column / give it back |
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

### Completion

The editor suggests what comes next from the connection's schema — its
tables, columns, types and foreign keys, read once per connection — and
from the dialect's vocabulary:

| Where | Suggested |
| --- | --- |
| `FROM ▮` · `UPDATE ▮` · `INTO ▮` | tables and views (the browsed schema's first), schemas, the statement's CTEs |
| `JOIN ▮` | first the tables a foreign key links to the ones already named, as whole clauses: `customers c ON c.id = o.customer_id` |
| `ON ▮` | the foreign key's condition between the table just joined and the others |
| `o.▮` | the columns of the table or CTE alias `o` stands for (a `SELECT *` CTE included); `public.▮` lists a schema's tables |
| `SELECT ▮` · `WHERE ▮` · `= ▮` … | the columns of every table the statement names — written after the caret too, so `SELECT ▮ FROM orders` works — qualified when two share a name; then functions and keywords |
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

In the terminal the list opens by itself after `.` or `::` and on the second
letter of a word, and follows your typing; `Ctrl+Space` asks anywhere. `↑`/`↓`
choose, `Tab` picks, `Enter` picks only when that changes the text (at the
end of a word already typed in full it is a new line), `Esc` closes. In dbc
web it is Monaco's suggest list, fed by the same rules. Running DDL
(`CREATE`, `ALTER`, `DROP` …) or a script makes the next suggestion read the
schema again.

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
| JPEG / PNG | a chat, a ticket, a slide | web: ⤓ Save ▸ JPEG / PNG · TUI: right-click ▸ **Save as JPEG** · shell: `-t jpeg` / `-t png` |
| Mermaid | a pull request or wiki (GitHub, GitLab and Notion draw ` ```mermaid ` blocks) | web: `m` or ⤓ Save ▸ Copy as Mermaid · TUI: right-click ▸ **Copy as Mermaid chart** · shell: `-t mermaid` |

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
table rather than as markup. Markdown, CSV, TSV and JSON are one row down.
The scope is the selected range when there is one, or the whole result.

**Transposed.** In `dbc web`, `t` in the grid (or `⇄ Transpose` on the
results bar) turns the result on its side, as psql's `\x` does: each row
becomes a column headed by its number, and each column a line led by its
name — the easy way to read, or share, one wide record. Copies and exports
follow what the grid shows, so **Table for Teams** pastes the vertical table
(names as row headers; a single row comes out as `column | value`), and
`⤓ Export ▾ → HTML page` downloads it. Sorting, hiding and selecting work as
they do upright; the arrow keys follow the screen. Every record shares one
width: drag any record's border to widen them all, or the border beside
`column` to widen the names; a double-click on either fits it.

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
**Recent conversations…** to see them all. A reopened conversation is the
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
result starts fresh.

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

Each statement of a multi-statement run is recorded on its own, so any one of
them is recallable. A statement identical to the one before it is not recorded
twice, and the app's own catalog query (`Ctrl+T`) never is.

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
pane's title names the one you are in.

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
which one (`running statement 2/4 …`). Select a region first and `Ctrl+R`
runs exactly that instead — a selection holding several statements runs them
in order, stopping at the first failure, with the last result shown in the
table.
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
data rules behave the same in both.

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

**Query tabs.** Each tab is its own workspace with its own pinned session,
so a `BEGIN` in one does not leak into another. `Alt+T` opens a tab, `Alt+W`
closes it (asking first when its session may hold a transaction, since
closing rolls it back), `Alt+1`…`Alt+9` switch, and a double-click renames.
A tab keeps its result, its grid view (sort, hidden columns, widths) and its
plan while another is on screen; a run left going in the background marks
its tab (● running, • done) and names its log lines.

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

**Adding connections.** The **+** beside *Connections* opens a form: name,
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
file's connections are changed in the file.

**Keys.** The TUI's, bent where a browser keeps the chord for itself:

| Key | Does |
|---|---|
| `Ctrl+Enter` (`Ctrl+R`) | run the statement under the caret, or the selection |
| `Ctrl+Shift+Enter` | run every statement |
| `Ctrl+X` (nothing selected) · `Ctrl+Shift+X` | explain · explain analyze |
| `Ctrl+K` | stop the run (in the assistant: stop the answer) |
| `Ctrl+P` · `Ctrl+E` · `Ctrl+O` | history · export · scripts |
| `Ctrl+I` | the assistant, and back (`Ctrl+A` stays select-all) |
| `Ctrl+Space` | suggestions (they also open as you type — see [Completion](#completion)) |
| `Alt+T` · `Alt+W` · `Alt+1`…`9` | new tab · close tab · go to tab |
| `Alt+N` · `Alt+C` | new console · next console of the tab's database |
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

**Scripts** (`Ctrl+O`, or ▷ Scripts) run from `scripts_dir`; `s.Print`
lines reach the log and `s.Show` results the grid as they happen.

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

Sample scripts live in [`scripts/`](scripts/): parameter loops, multi-host
sweeps, and CSV/HTML report generation.

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

### Scripts headless

`-t` and `-o` apply to scripts too (`-f` does not, since a script is its own
input; neither do `--tx` and `-k`, since a script runs its statements itself):

```sh
./dbc -t json script scripts/loop_params.go        # one JSON array on stdout
./dbc -t csv -o report.csv script scripts/loop.go  # straight to a file
```

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
