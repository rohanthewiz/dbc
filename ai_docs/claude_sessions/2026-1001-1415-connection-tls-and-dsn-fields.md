# TLS for connections, and a field-by-field connection form

Session: `5650388d-4c60-466b-9cfd-ea2a1dd32128`

## Ask

1. "I need tls support in the connection"
2. (mid-turn) "And please give me individual fields to enter a new
   connection. It's hard to construct the full DSN in one go for a human."

## What was built

### TLS settings per connection

Four new keys on a `[[connection]]`, one spelling for both server engines:

```toml
tls      = "verify-full"   # disable | prefer | require | verify-ca | verify-full
tls_ca   = "certs/ca.pem"  # blank: the system's trusted CAs
tls_cert = "~/certs/me.pem"
tls_key  = "~/certs/me.key"
```

- **Modes** are libpq's `sslmode` names, with libpq's meaning on both
  engines. `require` with a `tls_ca` checks the chain, as `verify-ca` does.
  Only `verify-full` checks the host name.
- **Unset `tls`** leaves TLS to the DSN, exactly as before. **Set**, it wins
  over the DSN's own `sslmode=` / `tls=`.
- **Fail closed.** `TLSOpts.Check` (`config/tls.go`) refuses an unknown mode,
  files with no mode, files beside `disable`, and a cert without its key.
  From the config file that fails the load. From `connections.toml` the
  entry is skipped with a warning. Neither falls back to "no TLS".
- **Paths** (`ExpandTLS`) take `${VAR}` and `~/`. A relative path is relative
  to the file that names it: the config file, or `~/.config/dbc` for saved
  connections. The `--tls-*` flags resolve against the working directory.
- **Wiring:** `config.TLSOpts` is embedded untagged in `Connection` and
  `SavedConn`, so the keys sit flat beside `dsn` in both files. It also
  carries JSON tags for dbc web.

How each driver gets it (`db/tls.go`, called from `openPool`):

- **Postgres:** `pgTLSDSN` writes the settings into the DSN as `sslmode`,
  `sslrootcert`, `sslcert` and `sslkey`. A URL gets query params. The
  keyword form gets quoted pairs appended, and pgx's map makes the later key
  win. pgx then builds libpq's exact behavior, including multi-host,
  `prefer`'s plaintext fallback and `verify-ca`.
- **MySQL:** go-sql-driver cannot name a CA or client cert in a DSN.
  `mysqlConnector` therefore parses the DSN, sets a `*tls.Config` from
  `mysqlTLS` on `mysql.Config.TLS`, and opens through `NewConnector` +
  `sql.OpenDB`. `RegisterTLSConfig` was avoided because its process-wide
  name registry would need name lifetime management across edits.
  `verify-ca` uses `InsecureSkipVerify` plus a `verifyChain` check of the
  chain only, as pgx does.
- **SQLite/bytdb:** TLS settings are refused, since a local file has no
  connection.
- Errors name the file (`tls_ca: open …`, `tls_cert … / tls_key …: …`).
  serr's fields are not shown to users, so the path goes in the message.

**CLI:** `--tls`, `--tls-ca`, `--tls-cert`, `--tls-key` apply to the ad-hoc
`--dsn` connection. Given without `--dsn`, they are an error rather than
silently ignored. They were added to `cats-plugin.toml`'s completions,
because `manifest_test` checks every flag is there.

### Field-by-field DSN entry (`db/dsn.go`)

- `DSNParts{Host, Port, User, Password, Database, File, Options}`.
- `BuildDSN(driver, parts)` writes one shape per engine:
  - **Postgres:** libpq **keyword/value** form. The password is always
    quoted, and other values are quoted when needed. A URL would need the
    password percent-encoded *before* `${VAR}` expansion, so an
    env password with `@`/`/`/`#` would break it.
  - **MySQL:** `user:pass@tcp(host:port)/db?opts`. The password is raw (the
    driver splits at the last `@`), and an IPv6 host is bracketed.
  - **SQLite:** the path, or `file:path?opts` when there are options.
  - **bytdb:** the path only.
  - **Options** are `key=value` pairs, separated by spaces or `&`, with
    `'quoted'` values allowed. A Postgres option may not set a field's
    keyword (`host`, `password`, …).
- `SplitDSN(driver, dsn)` is the inverse, for the edit form. It reads both
  Postgres forms and MySQL's short forms. `${VAR}`s are masked with
  stand-ins while parsing, because `url.Parse` rejects `{}` in userinfo, and
  restored afterwards. A DSN that would lose something in fields (several
  hosts, a MySQL unix socket) returns `ErrNotFields`.
- `IsEnvRef` identifies a password that is only a `${VAR}`, which may be
  shown in the form.

### dbc web

- **API:**
  - `connForm` takes `parts` (and `keep_password`) as an alternative to
    `dsn`. `formDSN` / `partsDSN` turn them into a DSN before `check`, so
    everything downstream is unchanged.
  - New `GET /api/v1/conns/:name/parts` returns the saved DSN split into
    fields. The password is withheld (`has_password`) unless it is a
    `${VAR}`. The response also carries the TLS settings as typed.
  - `connInfo` and the sidebar `data-tls*` attributes carry the resolved
    TLS settings.
  - An edit that changes TLS counts as a reconnecting change.
- **Form** (`conns.js`):
  - An *Enter as* Fields/DSN toggle, with Fields the default for adding.
  - Per-driver rows: host+port, user, password (masked), database, options;
    or file (+ options for SQLite). Port and options placeholders differ per
    driver.
  - A TLS select with descriptive labels. Its CA/cert/key rows show only for
    a mode other than ""/disable.
- **Editing in fields:**
  - Untouched fields send `dsn: ""` ("keep"), not parts. Rebuilding would
    re-spell the DSN, the server would count that as a reconnecting change,
    and it would be refused while a tab is on the connection, even for an
    `ai_rows`-only edit.
  - An empty password over a withheld one sends `keep_password`, and the
    server takes the password from the stored DSN.
- **Sidebar** shows `· tls` beside the driver when a mode other than
  `disable` is set.
- **CSS:**
  - `.connform [hidden]` uses `!important`, because flex rows would
    otherwise override `hidden`.
  - The form body scrolls inside the modal (`.connbody`).

## Verification

- **Unit tests:**
  - `config/tls_test.go`: Check, ExpandTLS, Load and saved round trip.
  - `db/dsn_test.go`: build checked through `pgx.ParseConfig` /
    `mysql.ParseDSN`, round trips, hand-written DSNs, ErrNotFields.
  - `db/tls_test.go`: real TLS handshakes against a loopback listener with
    a test CA. It covers each mode's accept/refuse for MySQL and for pgx's
    config from the rewritten DSN, client-cert-required servers, and bad
    files.
  - `web/conns_test.go` `TestConnFieldsAndTLS`.
- **Live** (`db/live_tls_test.go`, new):
  - Postgres 17 with `ssl=on` (snakeoil cert): `pg_stat_ssl` confirms
    `require` and `prefer` encrypt despite `sslmode=disable` in the DSN, and
    that `disable` does not. `verify-full` is refused.
  - MySQL 8.4: `Ssl_cipher` is set under `require`/`prefer`. `verify-ca`
    with the server's own `ca.pem` connects, and `verify-full` with it is
    refused on the auto-generated certificate's name.
  - All 19 live tests pass. The Postgres session/idle/cut suite also passes
    over a TLS connection, which exercises sockpeek's `*tls.Conn` path.
- **CLI end to end:**
  - `--tls require|disable|verify-full` against the TLS Postgres.
  - `--tls verify-ca --tls-ca` against MySQL.
  - `--tls` without `--dsn` is refused.
- **Browser:** a throwaway go-rod harness (scratchpad, not committed; see
  N-051) ran 23 checks: field visibility per driver and mode, Test with a
  missing CA, sqlite-from-fields connecting and saving, the saved
  `connections.toml` text, the sidebar `· tls` mark, the edit prefill with
  the password withheld, an edit that keeps the password, and DSN mode.
  Screenshots looked right.
- `dbc.example.toml` (with the new TLS lines) still loads. `go vet` and the
  full `go test ./...` are clean.

## Docs

- README: a new `### TLS` section under *Configure connections* (keys, mode
  table, precedence, paths, fail-closed). The `dbc web` *Adding connections*
  paragraph was rewritten for Fields/DSN, TLS and the edit behavior. The
  flags table gained `--tls*`.
- `dbc.example.toml`: TLS lines on the MySQL example.

## Next

Closed: None. Declined: None. Raised: N-074, N-075.
Deferred: None. Promoted: None.
Updated: N-051. Full list: `ai_docs/todo/next-list.md`.
