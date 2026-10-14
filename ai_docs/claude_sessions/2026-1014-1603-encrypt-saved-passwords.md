# Saved connections' DSNs encrypted at rest (AES-256-GCM, key beside the file)

Session: `2d1c6bee-22cc-47b6-867d-b55aaa0f2010`

## Ask

"Give me a simple encryption mechanism for now, for dbc passwords saved in
plain text on my machine." Then `/sw`.

## Findings

- **The passwords were only in `~/.config/dbc/connections.toml`.** It is
  where the TUI's connection form and dbc web save a connection
  (`config.SavedStore`), and it kept each DSN as typed, password inline.
  There is no `~/.config/dbc/config.toml` on this machine. The DSNs were
  counted, not read: `web.bytdb` (whose old conns table was emptied by
  `moveStoreConns`), `history.jsonl`, `consoles/` and `chats/` hold no
  `postgres://`, `password=` or `@tcp(` strings.
- **All the file I/O goes through two functions**, `ReadSaved` and
  `writeSaved` (the latter only from `SavedStore.edit`, under the
  `connections.toml.lock` writers' lock). The DSN never goes back to the
  browser (`web/conns.go`), and the TUI form gets it through
  `SavedStore.Get`. So encrypting at those two points leaves every caller
  plain in memory and unchanged.
- **The password is embedded in the DSN, in three syntaxes** (PG URL, libpq
  keyword/value, MySQL). Encrypting only the password would mean parsing,
  unescaping and re-escaping each one. `dsnexpand.go` does the escaping half
  for `${VAR}`s, but not the extracting half. Encrypting the whole `dsn`
  value avoids all of that.
- **No test reads the user's real `~/.config/dbc`.** Every test that runs
  `setup()` or the binary sets its own `HOME`. That mattered here, since
  `LoadSaved` now rewrites the file.
- **The tutorial test failures are N-211**, not this change.
  `tutorial/workspace/dbc.toml` is gitignored by the root's bare `dbc.toml`
  rule (`git check-ignore -v` confirms it).

## Changes

### `config/secret.go` (new)

- **Cipher.** AES-256-GCM via `cipher.NewGCMWithRandomNonce` (Go 1.24+), so
  the nonce is drawn and prepended by the AEAD itself. Stored as
  `dsn = "enc:v1:<base64(nonce ‖ ciphertext ‖ tag)>"`, with a fresh nonce
  on every write. GCM is authenticated, so a wrong key or an edited value
  fails with an error instead of decrypting to garbage that would be sent
  to a server as a DSN.
- **Key.** `connections.key` beside the file (`keyFile`:
  `connections.toml` → `connections.key`, derived from the path so tests in
  temp dirs get their own). 64 hex chars from `crypto/rand`, `0600`.
  - Made by `loadOrCreateKey` only when a write first needs it, and only
    under the writers' lock. Two processes therefore cannot each make one,
    the second replacing a key data was already encrypted with.
  - Written atomically (`writeAtomic`), then the directory is fsynced, best
    effort, so the key is on disk before any DSN encrypted with it.
  - A malformed key is an error, never replaced.
- **Which DSNs** (`sealsDSN`): every driver but sqlite/sqlite3/bytdb (file
  paths, left readable). An unknown or empty driver counts as a server, so
  a typo cannot leave a password plain. `${VAR}` DSNs are encrypted too:
  telling them from inline ones needs per-syntax parsing, and a wrong call
  there would leave a password in plain text.
- **Read side** (`openSaved`): decrypts envelopes in place, reading the key
  only once one is found. It reports whether any server DSN was plain. One
  DSN that does not open fails the whole read, like a TOML parse error
  does. The error names the connection, the key path and what to do (when
  the key is missing: restore it, or put the plain DSN back).
- **Write side** (`sealSaved`): encrypts a copy, so `edit`'s list (and a
  memory-only store's) stays plain.

### `config/saved.go`

- `ReadSaved` now wraps `readSaved`, which also returns `plain`.
- `writeSaved` calls `sealSaved`. Its temp → fsync → rename body moved into
  `writeAtomic`, shared with the key file.
- **Migration.** `LoadSaved`, which every dbc command runs at startup, does
  an identity `edit` when the file held a plain server DSN, so an existing
  file is encrypted on the next start instead of at the next add or edit. A
  failure is a warning, and the next start retries.
- Without the key, `edit` fails at its read, so the file is never rewritten
  (which would lose its encrypted DSNs) and no new key is made.
- The file's header comment and the PASSWORDS paragraph of the package doc
  now describe the encryption and the key.

### Tests

- `config/saved_test.go`: `TestSavedRoundTrip` now expects the postgres DSN
  as an envelope (no `PGPASS`, no `@stg/app` in the text), the sqlite one
  as typed, a `0600` key, and three entries in the directory (file, lock,
  key).
- `config/secret_test.go` (new):
  - `TestSealOpenDSN`: round trip, distinct envelopes for one DSN, wrong key,
    a flipped ciphertext bit, bad base64.
  - `TestLoadSavedEncryptsPlainFile`: a plain file is encrypted by
    `LoadSaved`, the merged config is plain, the key is stable across later
    writes, and a second `LoadSaved` writes nothing.
  - `TestNoKeyWithoutServerDSN`.
  - `TestMissingKey`: read fails naming the key, the add fails, the file is
    unchanged, no new key, `LoadSaved` warns.
  - `TestMalformedKey`: never replaced.
  - `TestKeyFile`.

### `README.md`

The saved-connections paragraph: how the DSN is stored (AES-256-GCM, the
key file, `0600`), what that protects (the file leaking alone) and what it
does not (anything running as the user). Copy both files to move them; lose
the key and the DSNs must be retyped. Copying an entry into `config.toml`
now means writing its `dsn` out again.

## Verification

- `gofmt -l config` clean, `go vet ./config` clean.
- `go test ./...`: all pass except `tutorial` (N-211, pre-existing).
- **The real binary, against a throwaway `HOME`.**
  - Setup: a hand-written plain `connections.toml` with a postgres entry
    (password `hunter2`, port 1) and a sqlite entry.
  - `dbc -c lite "select 1"` ran, and the file then held `enc:v1:…` for pg,
    the sqlite path unchanged, and `connections.key` `0600`.
  - `dbc -c pg "select 1"` failed dialing `127.0.0.1:1` as
    `user=app database=app`, so the decrypted DSN reached the driver.
  - `grep -r hunter2` over that HOME found nothing.
- The user's real `connections.toml` was not touched. It is encrypted by
  the first start of a rebuilt dbc (N-215).

## Notes for the user

- Rebuild every dbc in use (the repo's stale `./dbc`, `dbc.app` via
  `mac-install.sh`). An older binary reads `enc:v1:…` as the DSN itself and
  fails to connect. It does not damage the file: it writes the envelopes
  back as they are.
- Back up `connections.key` along with `connections.toml`.

## Next

Closed: None. Declined: None. Raised: N-214, N-215, N-216.
Deferred: N-214. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
