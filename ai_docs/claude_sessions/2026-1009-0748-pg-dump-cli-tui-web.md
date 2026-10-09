# Postgres dumps: `dbc dump`, and "Dump database…" in the TUI and dbc web

Session: `de8961be-29b3-4b85-8daa-05ee68b316b7`

## Ask

1. From the cats-todo backlog: "For Postgres DBs add the ability to do a DB
   dump with options for multiple files and other pg_dump options."
2. "Add a Dump database action to the web UI and TUI."
3. `/sess-wrap`.

## What was built

### `dbc dump` (CLI, `dumpcmd.go`)

`dbc dump` runs PostgreSQL's own `pg_dump` against a configured connection,
including its TLS keys and a `NAME/otherdb` database. The global `-t` picks
the format:

- `plain` (default; `-o` or stdout), `custom`, `tar`, `directory` (`-j N`):
  pg_dump's own formats.
- `split`: dbc's own, plain SQL with one file per table. The layout is
  `1-pre-data.sql`, `2-data/<schema>.<table>.sql`, `2-data-other.sql`
  (setval, large objects), `3-post-data.sql` and a `restore.sql` that
  `\ir`s them in order. `--keep-archive` also keeps `archive/`.

It has first-class flags for `--schema-only`/`--data-only`, schema and table
include/exclude, `--exclude-table-data`, `--clean`/`--if-exists`/`--create`,
`--no-owner`, `--no-privileges`, `--inserts`/`--column-inserts`, `--compress`,
`--encoding` and `--dry-run`. Anything else goes after `--`, except the
options dbc manages itself (`--file`, `--format`, `--dbname`, `--host`,
`--jobs`, …).

### Shared core

- **`db/libpq.go`** (`LibpqFor`) restates a dbc connection in libpq's terms,
  because pg_dump speaks libpq, not pgx:
  - it parses the URL form (multi-host, IPv6, `%2F` socket dirs, `ssl=true`)
    and the keyword form itself, since pgx.ParseConfig discards the raw
    settings;
  - the tls keys override the DSN, and `tls_key_password` becomes
    `sslpassword`;
  - `verify-full` with no CA gets `sslrootcert=system`;
  - pgx-only keys are dropped silently, runtime params with a note, and a
    `service=` DSN is refused.

  All settings go in a 0600 libpq service file in a private temp dir, the
  password goes in `PGPASSWORD`, and pg_dump gets only
  `--dbname=service=dbc`. Nothing secret appears in `ps`.
- **`pgdump/`** is UI-free, like pgdocker:
  - `Options.Check` refuses flags pg_dump would silently ignore (`--clean`
    etc. on archives; `split --clean` without `--create`).
  - `Locate` takes `--pg-bin`, `$DBC_PG_BIN` or config `pg_bin` if set, else
    PATH, else Homebrew's keg-only libpq/postgresql@N, Postgres.app, Debian
    and RHEL directories. It asks the server its version and uses the first
    pg_dump that is new enough.
  - `Run.Do`: single-file and directory dumps write to `OUT.partial`, which
    is renamed to OUT on success and removed on failure or cancel. Cancel
    sends SIGINT.
  - `split.go` runs one `pg_dump -Fd` (one snapshot, parallel), then
    `pg_restore --list` and a `pg_restore` per part (sections, plus
    `--use-list` per TABLE DATA entry). Up to `--jobs` run at once
    (errgroup). Colliding file names (`Cats`/`cats`) get the TOC id
    appended. A failed run removes what it wrote.
  - `prepare.go` is shared by the three front ends: `Prepare` (Check, Target,
    ServerMajor, Locate), `Form` → `Options` (the dialogs' fields; archive
    formats drop the hidden restore-time boxes), `DefaultOut`
    (`~/Downloads/<conn>-YYYYMMDD-HHMM.sql`), `SwapSuffix`, `RestoreHint`,
    `Report` (Do with every step, tool line and outcome passed to
    `say(level, text)`), and `LineWriter`.
- **Config `pg_bin`** (`config.PGBin`) is resolved like `scripts_dir` through
  the new shared `resolvePath`. It is documented in `dbc.example.toml`.

### TUI (`tui/dump.go`)

The connections right-click menu has **⇩ Dump database…**. It is dimmed with
the reason on non-Postgres connections and while a dump runs, when
**■ Stop dump of X** is offered instead.

The form has:
- format chips with a one-line description;
- To file / To directory, its suffix following the format;
- Jobs (directory and split only);
- What (everything / schema only / data only);
- Schemas, Tables, Skip tables, Skip data of;
- checkboxes, with owner/create/clean hidden for archive formats;
- More options.

Dump checks the form, then `Prepare` runs off the UI goroutine
(seq-numbered, so stale answers are dropped). Errors stay in the form. On
success the form closes and `Run.Report` streams lines to the on-screen log
via `m.send`. One dump runs at a time. Quitting the TUI cancels a running
dump and waits up to 15s for its cleanup.

### dbc web (`web/dump.go`, `conns.js`)

- `GET /api/v1/dump?conn=` returns the suggested path and the running dump.
- `POST /api/v1/dump` refuses with `ok:false` for the dialog, or starts the
  dump in the background and answers at once.
- `POST /api/v1/dump/stop` cancels it.
- Lines and running state are broadcast as `"dump"` events to every window
  (`app.js` routes them to `dbc.conns.onDump`), so a reload loses nothing.

One dump runs at a time across windows, and `Server.Shutdown` stops it. The
dialog mirrors the TUI form, and the menu rows match the TUI's.

## Verification

- **Unit tests.**
  - `db/libpq_test.go`: keyword and URL forms, TLS, derived databases,
    verify-full, refusals.
  - `pgdump`: Check, args, Locate version picking with fake binaries, TOC
    parsing and split planning, split runs and cleanup with fake
    `pg_dump`/`pg_restore`, and the plain-run mismatch hint.
  - `tui/dump_test.go`: the menu row, form fields and suffix, refusals, an
    unreachable server, a run to the log, stale answers.
  - `web/dump_test.go`: info and refusals, a background run, one at a time,
    Stop leaving no `.partial`.

  The full suite passes. The dump tests also pass under `-race`.
- **Real Postgres, in containers** (a linux dbc inside `postgres:17-alpine`
  against a seeded PG17). All formats were written. The split, custom and
  `--create --clean` (derived DB) dumps restored with identical counts,
  sequence, FK, indexes and matview. Also checked: the version-mismatch
  message with pg_dump 16, and every refusal.
- **Host.** Homebrew's keg-only `postgresql@16` pg_dump was found by the
  probe (`which` had missed it). Plain, directory into an existing empty dir
  (the rename path) and split all ran against PG16.
- **Web e2e** (go-rod): a new "dump database dialog" step checks the dimmed
  row on SQLite and, with `DBC_LIVE_PG_DSN`, the suggested name, the
  format-following suffix, Jobs and owner visibility, then Dump. Against
  PG17 it showed the "pg_dump 16 is too old" refusal in the dialog. Against
  PG16 it ran to "dumped pg to …" in the log, and the file exists.
- **Docs.** README has "Dumps headless (Postgres)", "Dumping a database"
  (TUI and web) and a web workbench paragraph. Also updated: the
  connections-menu row in the TUI table, the help line, `cats-plugin.toml`
  completions (enforced by `manifest_test.go`) and the dbc skill table.

## Notes

- `--table` doesn't dump the table's schema (pg_dump's rule). Restoring
  `--table 'sales.*'` into an empty DB fails until `sales` exists. This is
  noted in the README flag table.
- One stray SQL probe was run against the throwaway test container during
  testing. It touched only that container, which was removed.
- Test containers and networks (`dbcdump-*`, `dbce2e-pg`) were removed.

## Next

Closed: None. Declined: None. Raised: N-169, N-170, N-171, N-172.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
