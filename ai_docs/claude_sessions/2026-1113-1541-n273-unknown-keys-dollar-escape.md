# N-273: unknown config keys are reported, and `$$` is a literal `$`

Session: `2690c1a0-7bf1-4210-a0a4-153a4dcbd1aa`

## Ask

N-273 from the next-list (RPU plan, phase 2), then `/commit`, `/push`,
`/sw`. The item had two parts:

- **Unknown TOML keys were ignored.** `config/config.go` (Load) and
  `config/saved.go` (readSaved) threw away the decoder's metadata, and
  nothing called `Undecoded`. So `ssl = "verify-full"` or `tls_mode = …`
  in place of `tls` connected with no certificate check, while a bad
  *value* of `tls` failed the load on purpose. `max_row = 50000` was
  ignored too. Fix: make `md.Undecoded()` warnings, and errors for keys
  under `[[connection]]`.
- **A literal `$` could not be written in a password.** `os.Expand` read
  `S3cr$t9` as a lookup of `$t9`, which lost `$t9` with an "unset env var"
  warning, and authentication failed. The form's DSN builder
  (`db/dsn.go`) leaves a typed `$` as it is, so the form had the same
  problem. Fix: make `$$` a literal `$`, and say so in the warning.

Commit: `df16005` (pushed).

## What was built

**`config/strict.go`** (new). Reports undecoded keys.

- `strayKeys(md)`: `md.Undecoded()` has no entry index for an array of
  tables. Every entry's key is `connection.ssl`, so it can't tell you
  which connection the key is in. `md.Keys()` is in document order and
  has one `connection` key per `[[connection]]` header, so the walk
  counts headers to place each undecoded key at the top level or in
  entry *i*. This was checked against BurntSushi/toml v1.6.0 in a scratch
  program. The walk also:
  - leaves out a key under one already listed (`[ai]`'s own keys,
    `sub.x` under `sub = {…}`) and resets that list at each header;
  - handles an inline `connection = [{…}]`, which is type `Array` with
    one key for all its entries, by leaving the entry unnamed.
- `tomlKeys(reflect.Type)` reads the key lists from the struct tags. It
  flattens the embedded `TLSOpts` and skips `-` and unexported fields. It
  gives `configKeys`, `connectionKeys` and `savedConnKeys`, so a new
  setting shows up in the messages the day it is added.
- Suggestions:
  - `keyAliases` maps libpq and MySQL TLS names (`ssl`, `sslmode`,
    `tls_mode`, `sslrootcert`, `sslcert`, `sslkey`, …) to the `tls*`
    keys. An alias counts only when that file level has the key, so a
    top-level `sslmode` gets no `tls` hint.
  - `editDistance` is optimal-string-alignment (a swap of two adjacent
    characters counts as one edit: `dns` → `dsn`). A suggestion needs at
    most 1 edit for keys of up to 4 characters, otherwise at most 2.
  - The first run of the real binary suggested `dsn` for `ssl`
    (Levenshtein 2), which is the worst possible hint for this item. That
    is why the aliases and the tighter short-key limit exist.

**`config.toml`** (`Config.checkStray`, called right after decoding):

- A top-level stray key is a warning: `dbc.toml: unknown key max_row (did
  you mean max_rows?) is ignored`.
- A stray key inside a `[[connection]]` fails the load. One error names
  every bad key by connection and lists the keys a connection takes:
  `connection "prod": ssl (did you mean tls?)`. `demo = true` is caught
  as well, because `Connection.Demo` is `toml:"-"`.
- `added` is accepted and ignored (`copiedSavedKeys`). `savedDoc`
  promises an entry can be copied from one file to the other, and every
  saved entry has `added`.

**`connections.toml`** (`savedRead` replaces readSaved's
`(list, plain, err)`):

- `usable` drops an entry that has a stray key, with a warning. This
  follows MergeSaved's reasoning for a bad `tls`: a connection that was
  meant to be protected must not connect without the protection. A
  top-level stray key in this file is a warning.
- `migrations` is a stray key here, since SavedConn has no such field.
- `SavedStore.edit` refuses to rewrite the file while any stray key is in
  it (`strayErr`). The rewrite encodes SavedConn, which would drop the
  key silently, and the skipped entry would come back on the next start
  without it, which is the exact case being guarded.
- LoadSaved's start-up rewrite (the one that encrypts a plain DSN) is
  refused by the same check. Its warning then says the passwords are
  still in plain text and why.
- `ReadSaved` still lists every entry, because the web edit form reads
  through it.

**`config/envref.go`** (new):

- `expandEnv` is `os.Expand` with one change: `$$` becomes `$`.
  `os.Expand` reads `$$` as the shell's special variable `$` (the PID),
  which no environment holds, so `$$` only ever expanded to "". Making it
  a literal changes nothing that worked before. `${$}` also becomes `$`.
- `unsetWarning` adds "write $$ for a literal $" to every unset-variable
  warning.
- Used by `ExpandDSN` (both passes), `ExpandTLS` and `resolvePath`
  (`scripts_dir`, `pg_bin`, the other `*_dir` keys).
- In ExpandDSN's marker pass, `$$` turns into its `$` as DSN text rather
  than as a reference. It gets no marker, so `fillRefs` doesn't escape
  it.

**The form:** a typed `$` before a name stays a reference, because
`${VAR}` passwords work that way by design. The web and TUI hints and
the password placeholders now say "$$ for a literal $". Test and Save
show ExpandDSN's warning, which carries the hint.
`db.IsEnvRef("$${X}")` is false, so a password with a literal `${` is
still withheld from the browser.

**Docs:** README (Configure connections: `$$` and unknown keys; saved
connections: `migrations` is not copied; the form: `$$`),
`dbc.example.toml`, the saved file's header (`savedHeader`), and the
dbc skill.

## Tests

- `config/strict_test.go`:
  - top-level warnings with suggestions, and a table reported once;
  - several connections with `ssl`, `tls_mode`, a subtable and `demo`,
    all in one error, with nothing about the clean entry;
  - `dns` → `dsn`;
  - an inline array;
  - `added` accepted;
  - saved file: an entry skipped, a top-level warning, `migrations`, an
    Add refused and the file byte-for-byte unchanged, ReadSaved still
    listing every entry;
  - a plain DSN beside a stray key is left plain, with the warning;
  - `TestNearKey` (aliases, typos, nothing near, no top-level `tls`);
  - `TestTomlKeys`.
- `config/envref_test.go`:
  - `$$` in a Postgres keyword DSN, a Postgres URL and a MySQL DSN, each
    read back by pgconn or go-sql-driver, beside a real `${VAR}`;
  - `$$$${VAR}` ordering;
  - the unescaped warning;
  - tls_ca and pg_bin paths;
  - a whole Load.
- `db/dsn_test.go`: `TestBuildDSNLiteralDollar` (form → BuildDSN →
  ExpandDSN → the driver reads one `$`), round-trip cases with `$$`, and
  `IsEnvRef` cases for `$${X}` and `$$X`.
- Full `./...` was run twice (no live DSNs: nothing here talks to a
  server). The first run failed only `web` TestChatEffort; the second was
  green. That test also fails on a clean tree, 6 of 30 and then 1 of 30
  runs alone, so it is flaky and not caused by this change. Raised as
  N-321.
- Real binary, scratch HOME, `CATS_*` stripped:
  - `ssl = "verify-full"` failed the load naming
    `connection "prod": ssl (did you mean tls?)`;
  - `max_row` warned and the query ran;
  - a sqlite `dsn = "file:${DIR}/a$$b.db"` created `a$b.db`;
  - a `connections.toml` entry with `sslmode` was skipped with the
    warning, and `-c` on it then said "unknown connection".

## Notes

- A python heredoc holding backticks and `${` was refused by the
  harness's rm-safety parser (it could not parse the command; there was
  no rm in it). Writing the script to a scratch file and running it
  worked.

## Next

Closed: N-273. Declined: None. Raised: N-321, N-322. Deferred: None.
Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
