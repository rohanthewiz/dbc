# CLI on urfave/cli v3: `-f` reads SQL from a file or stdin, `-t` is the format

Session: 1b57c31e-8c22-4437-aa52-a6507a352f65
Date: 2026-09-24

Loaded `2026-0924-1744-conn-cancel-dead-sessions-retire-tview-ui` and the
next-list.

## Ask

1. N-005: headless SQL came only from the argument. "Let's retake `-f` for
   file input and use `-t` for output format. What do you think?"
2. After my recommendation: "Yes do the cli change, however let's no longer use
   Go's flag package, rather github.com/urfave/cli, which would make the
   `-format` option `--format`".

## Recommendation (agreed)

- `-f FILE` matches `psql -f` and `usql -f`. `-t` also means "tuples only" in
  psql, but it's easy to remember as "type".
- Breaking `-f` was cheap. No release has been tagged (N-011), and nothing in
  `~/projs` outside dbc runs `dbc -f <format>`.
- `-f -` reads stdin. Piped stdin with no SQL argument also runs headless,
  because the TUI needs a terminal on stdin, so a pipe there was never a TUI
  launch.
- `-f` together with a SQL argument, `script` or `migrate` is an error rather
  than being silently ignored.

## What changed

### `main.go`

- **Go's `flag` was replaced by `github.com/urfave/cli/v3` v3.13.0.**
  - `newCLI()` builds the tree. The root command is the TUI or a headless
    query, and `script` and `migrate` are subcommands.
  - Each flag writes into a plain package variable through `Destination`
    (`flagConn`, `flagFile`, `flagFormat`, …). The existing helpers and tests
    read the values without passing `*cli.Command` around.
- Flags:

  | short | long | notes |
  | --- | --- | --- |
  | `-f` | `--file` | SQL from a file, `-` = stdin |
  | `-t` | `--format` | default `text` |
  | `-o` | `--out` | |
  | `-c` | `--conn` | |
  | | `--config`, `--demo` (`$DBC_DEMO` via `cli.EnvVars`) | |
  | | `--driver`, `--dsn` | help category "ad-hoc connection" |
  | | `--dir`, `--allow-missing` | help category "migrate" |

- **All flags stay on the root and are persistent** (the cli default).
  - They work after the SQL and after a subcommand
    (`dbc migrate version -t json`).
  - `--dir db/migrate migrate up` (goose's word order) still works.
- **Old spellings still parse.** urfave's own parser looks names up
  regardless of the dash count, so `-dsn` and `-demo` work. `--` still ends
  flags, for SQL that starts with a comment.
- **`sqlInput(args, file, stdin, stdinPiped)`** is the headless-or-TUI
  decision, with a table in its comment:
  - argument, `--file`, `-` or piped stdin → headless
  - nothing → TUI
  - argument and `--file` → error
  - more than one argument → error. Unquoted SQL used to run only its first
    word.
  - a format name given to `-f` that isn't an existing file → "-f now reads
    SQL from a file — for the output format use -t csv"
- `stdinHasInput` counts a named pipe or a regular file only. A terminal,
  `/dev/null` (a char device) or a closed stdin still opens the TUI.
- `refuseFile` rejects `--file` on `script` and `migrate`.
- **`setup()`** (load config, add the ad-hoc connection, seed the demos) runs
  inside the actions. So `--help` and usage errors never touch a database.
- **`usage(msg)`** prints and exits 2. It replaces the hand-rolled
  `Fprintln` + `os.Exit(2)` pairs. `main` also exits 2 on a cli parse error,
  which urfave has already printed along with the help text.
- **Gotcha:** `serr.Wrap(nil, …)` prints "SErr: Not wrapping a nil error" to
  stdout, which would corrupt headless output. The stdin branch checks the
  error before wrapping.
- **Gotcha:** a word in backticks inside a flag's `Usage` becomes the value
  placeholder, even on a `BoolFlag`. `--allow-missing` rendered as
  `--allow-missing migrate up` until the backticks were removed; there's a
  comment on it now.

### Elsewhere

- `migrate.go`: `flagDir`/`flagMissing` are plain values now. The usage text
  and comments use the `--` spellings.
- `README.md`, "Headless mode":
  - examples use `-t` and `-f`
  - a flag table
  - the stdin rules and the one-argument rule
  - a note on the `-f` → `-t` move
  - the migrate and script sections use the `--` spellings
- `cats-plugin.toml`: static completion flags updated to the new short and
  long names.
- `dbc.example.toml`: comments use `--demo` and `--dir`.

## Verified

- `go test -race ./...` passes.
  - `TestSQLInput` covers every row of the decision table.
  - `TestCLIParsing` swaps the actions for recorders and checks:
    - short and long spellings
    - flags after the SQL
    - single-dash long names
    - `-f -` not ending flag parsing
    - goose word order
    - flags after a subcommand
    - `--` before a comment
- Ran the built binary against the demos (scratch `HOME`, `CATS_*` unset):
  - argument, `-f`, `-f -`, piped and redirected stdin
  - every refusal, `migrate help`
  - `-demo sqlite -c demo`, and an empty file ("nothing to run", exit 2)
- A bare `dbc` under a Python `pty` enters the alternate screen and draws.
  macOS `script` couldn't be used because it wants a terminal on its own
  stdin.

## Next

Closed: N-005. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: N-003 (line ref; now also covers `-f`/stdin input), N-010 (line ref,
seeding moved into `setup`). Full list: `ai_docs/todo/next-list.md`.
