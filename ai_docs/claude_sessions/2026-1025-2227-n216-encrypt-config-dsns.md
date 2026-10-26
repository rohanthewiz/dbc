# N-216 — `dbc encrypt`, and encrypted DSNs in config files and `--dsn`

Session: `4a1c10ce-df78-45f6-a90b-57142e9f899e`

## Ask

From the cats-todo backlog, next-list item N-216: only `connections.toml`
encrypted its DSNs (`config/secret.go`). A `config.toml` / `dbc.toml` DSN with
an inline password stayed plain — `Load` did not open `enc:v1:` values and no
command made one. Add `dbc encrypt` (prints the envelope for a DSN typed or
piped in, made with `connections.key`) and have `Load` open envelopes through
the same `openDSN`. Follow-up ask: let the ad-hoc `--dsn` accept an envelope
too.

## What was there

- `config/secret.go`: AES-256-GCM envelopes (`enc:v1:<base64(nonce‖ct‖tag)>`),
  key in `~/.config/dbc/connections.key` beside `connections.toml`, made by
  the first saved-connections write under the writers' lock (`lockSaved`).
- `Load` decoded the config, then `ExpandDSN`'d each DSN; nothing knew of
  envelopes there. dbc never writes the config file.

## Design

- **One key.** Config envelopes use `connections.key` too, not a key beside
  each config file: a `dbc.toml` often sits in a git checkout, the last
  place a key should be made. Cost: a config with envelopes opens only where
  that key is — same as `connections.toml`.
- **`config.SealDSN(savedPath, dsn)`** takes the saved-connections writers'
  lock and `loadOrCreateKey`s, so a concurrent `dbc web` write cannot make a
  second key that strands one side's envelopes. Refuses an envelope as input.
- **`configKey`** (Load, and `--dsn` via exported `config.OpenDSN`) reads the
  key lazily at the first envelope and never makes one: a config with no
  envelope needs no key; a missing/wrong key fails with an error naming the
  source (`connection "pg" in dbc.toml` / `--dsn`) and the key path, rather
  than dialing the envelope as a DSN.
- **Order in Load:** open, then `ExpandDSN`, so an envelope may hold
  `${VAR}`s. `--dsn` is not expanded after opening, matching every other
  `--dsn` (the shell expands those).
- **`dbc encrypt`** (`encryptcmd.go`): no-echo prompt on a terminal
  (`charmbracelet/x/term.ReadPassword`), else stdin. Never an argument (shell
  history, `ps`). Input trimmed (a pipe's trailing newline would otherwise
  land in the last DSN value); empty or multi-line input is usage (exit 2).
  Envelope alone on stdout; a how-to on stderr only when it is a terminal.
  Reads no config, opens nothing.

## Changes

- `config/secret.go`: `KeyFile`, `SealDSN`, `configKey`, `OpenDSN`, and the
  doc comment on the config-file flow.
- `config/config.go`: `LoadDemo` opens envelopes before `ExpandDSN`.
- `encryptcmd.go` (new) + registered in `main.go`'s `newCLI`.
- `main.go` `addAdHocConn`: `--dsn` through `config.OpenDSN`, after the flag
  checks.
- `cats-plugin.toml`: `encrypt` in the completions' subcommands
  (`TestCatsManifest_CompletionsCoverCLI` requires it).
- README (Configure connections paragraph, saved-connections note, headless
  `--driver/--dsn` row), `dbc.example.toml`, `.claude/skills/dbc/SKILL.md`.

## Tests

- `config`: `TestLoadOpensConfigEnvelope` (also: a later saved connection
  reuses the key `SealDSN` made), `TestLoadConfigEnvelopeFails` (wrong key,
  missing key, no key made), `TestLoadPlainConfigNeedsNoKey`.
- `main`: `TestReadDSN`, `TestAdHocDSNEnvelope`.
- Real binary (temp `HOME`, `CATS_*` stripped): a sqlite envelope in
  `dbc.toml` and in `--dsn` runs a query; with the key moved both fail with
  the named error, exit 1; argument / empty / two-line input exit 2; the
  prompt under a pty (`script -q`) does not echo.
- `go test ./...` passes.

## Next

Closed: N-216. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
