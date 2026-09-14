# dbc migrate replaces goose

Session: https://claude.ai/code/session_011aQE2KJb9PYEC4vHeeSAja
Date: 2026-09-13

## Why

The church framework (`~/projs/go/church/church`) ran its Postgres schema
migrations with the external `goose` binary: fourteen files in
`db/migrate/NNN_name.sql`, a `goose_db_version` table, and README steps to
install goose first. The user called it a loose end and asked to move the
functionality into dbc, which already opens every engine the church project
uses.

## What was built

### `migrate` package (`migrate/parse.go`, `migrate/run.go`)

- **File format is goose's, unchanged.** `NNN_name.sql` (the name part is
  optional) with `-- +goose Up` / `-- +goose Down` sections. The annotation
  parser also honors `StatementBegin`/`StatementEnd` verbatim blocks and
  `NO TRANSACTION`; any other annotation (`ENVSUB` etc.) is an error rather
  than being silently ignored, since it would change the file's semantics.
- **Statements are split by `sqlsplit`**, the same scanner the editor uses,
  so `$$`-quoted function bodies are one statement without needing a
  StatementBegin block. Non-verbatim runs of lines are split; verbatim blocks
  are one statement each; the two interleave in file order.
- **`Load(dir)`** skips subdirectories and non-matching files (church keeps a
  `queries_stash/` and `user_and_db/` beside its migrations) and rejects two
  files sharing a version.
- **Version table is goose's `goose_db_version`** with goose's columns and
  the version-0 seed row, so a database goose migrated carries over as-is and
  goose can still read what dbc writes. Existence is probed with a SELECT
  rather than `CREATE TABLE IF NOT EXISTS`, which not every engine spells the
  same. `tstamp` is written explicitly from Go rather than relying on a
  `now()` default. Applied state is the newest row per version, which also
  reads the old goose style of recording a rollback as `is_applied=false`.
- **`Dialect`** per engine: DDL for the version table, placeholder style
  (`$n` for postgres/bytdb, `?` for mysql/sqlite), and a `noTx` flag. bytdb
  runs DDL outside transaction blocks, so its migrations run statement by
  statement.
- **`Migrator`**: `Statuses`, `Version`, `Up`, `UpTo`, `UpByOne`, `Down`,
  `DownTo`, `Redo`. A migration's statements and its version row go in one
  transaction, so a failure leaves neither. `Up` refuses a pending migration
  older than the current version (the two-branches-merged case) unless
  `AllowMissing` is set, matching goose. Rolling back a version the directory
  no longer has a file for is an error. `Log` is an optional per-migration
  line callback.
- **`Create(dir, name)`** writes a UTC-timestamped empty file, name sanitized.

### CLI (`migrate.go`, `main.go`)

```
dbc [-c conn | -driver d -dsn s] [-dir path] [-allow-missing] migrate <cmd>
  status | version | up | up-by-one | up-to V | down | down-to V | redo | create NAME
```

- `status` and `version` are emitted as `model.Result`s through the ordinary
  exporters, so `-f json migrate version` works for deploy scripts.
- Migrations dir precedence: `-dir`, then the connection's new `migrations`
  config field, then `.` (goose's default).
- **New `-driver` / `-dsn` flags** register an ad-hoc connection named `dsn`
  so a CI job with no config file can run
  `dbc -driver postgres -dsn "$DATABASE_URL" -dir db/migrate migrate up`.
  It becomes the default connection for that run, and works for ordinary
  headless queries too. Connection resolution was lifted out of
  `runQueryHeadless` into `pickConn` so both paths share it.
- `db.Driver` exports the alias→canonical driver resolution.
- README gained a "Migrations" section; `dbc.example.toml` shows the
  `migrations` field.

### Church side (separate repo, committed there)

- New `church/dbc.toml`: connection `church-dev` with
  `migrations = "db/migrate"`, so `dbc migrate up` from the church directory
  needs no flags.
- README, CEMA_LOCAL_SERVER.md (git-excluded), the parent `CLAUDE.md`, cema
  and ccswm READMEs, and two `test_scripts` comments now say dbc instead of
  goose. Remaining "goose" mentions name the file format only.

## Verification

- `migrate` tests: parser cases and error cases; `Load` ordering and decoys;
  the full up-to / up / no-op / down / redo / down-to-0 cycle on **both
  SQLite and bytdb**; missing-migration refusal and `-allow-missing`;
  a failing migration rolled back with the earlier one kept and the error
  naming the statement position (via `serr.StringFromErr`, since fields are
  not in `Error()`); reading a goose-written history including an old-style
  rollback row; `Create`.
- Full dbc suite, `go vet`, `gofmt` clean.
- **Real data:** `dbc migrate status` on `church_development` matched goose's
  own `status` output line for line. On a throwaway Postgres database all
  fourteen church migrations went up (in two steps with `up-to`), `up` again
  was a no-op, `redo` worked, `down-to 0` returned to version 0, and goose
  read that table as version 0. The scratch database was dropped.
- `~/bin/dbc` was rebuilt from this tree so the command works locally now.

## Next

- Push dbc so `go install github.com/rohanthewiz/dbc@latest` (what the church
  docs now say) actually delivers `migrate`. Consider tagging a release.
- Church's `db/migrate` is Postgres-specific (`OWNER TO`, `BIGSERIAL`), and
  its bytdb backend boots from `db/bytdb_schema.go` instead. Running the same
  files on bytdb through `dbc migrate` is therefore not a goal; noted so it is
  not attempted by accident.
- After `down-to 0` on the scratch database, `information_schema.tables` still
  counted 2 public tables (`goose_db_version` plus one more). Likely a church
  migration whose Down does not drop everything its Up creates; not
  investigated. A church concern, not a dbc one.
- Carried: `scripts/export_report.go` and `testdata/show_two.go` hard-code the
  connection name `demo`, which is why the SQLite demo keeps that name.
- Carried: every headless invocation seeds both demos when there is no config;
  lazy seeding on first use is the fix if that ever matters.
- Carried: `~/Library/Caches/dbc/demo.bytdb` is real; deleting it is safe.
