# A review of all of dbc: the reliability, performance and usability plan

Session: `007484b3-76ba-4046-a205-6d4e0f8e42f5`

## Ask

"Take a deep look at dbc and come up with a plan to improve reliability,
performance, and usability." Then, once the plan had been shown in chat:
"save these to a plan under ai_docs/plans and raise the items on the next
list."

No product code changed in this session. It produced a plan and 59 list
items.

## What was done

The last whole-repo review (`ai_docs/fable_recommends.md`, 2026-07-31) was
of a 3.2k-line codebase and is all done. dbc is now about 111k non-test
lines of Go and 17k of hand-written JavaScript, so this was a fresh pass
against `main` at `2dece91` (v0.13.2).

- **Run in the session:** `go build ./...`, `go vet ./...` and
  `go test -race -count=1 ./...` with `CATS_*` stripped. All pass on the 33
  packages that have tests; `tui` and `erd` take about 46 s each. Warm
  startup of `dbc --version` is about 20 ms.
- **The headless CLI in a scratch HOME:** a SQL error, an unknown
  connection and a bad `--format` all print as a raw logrus line and exit 1;
  a 2M-row `-t csv` export stops at 1,000 rows with a stderr note and
  exit 0, and there is no flag to lift the cap.
- **Six area audits in parallel**, each read-only with file-and-line
  findings checked against the next list first: query core (`db`,
  `workspace`); `tui`; `web` (Go and JS); `etl`, `pipeline`, `jobs`,
  `script`, `sdb`, `migrate`, `pgdump`; `config`, `userdata`, `export`,
  `ai` and the CLI; `sqlsplit`, `sqlcomplete`, `explain`, `designer`,
  `erd`. The SQL audit ran its findings (repro programs, timings, about
  1.4 million fuzzed calls); the other five traced code and driver source
  and ran nothing.
- **Spot checks of the audits' main claims** against the code, and three
  reproduced here with a short program:
  - `sqlsplit.MySQL.Split("DELETE FROM t # old; cleanup\nWHERE id = 1;")`
    gives `DELETE FROM t # old` and `cleanup\nWHERE id = 1`.
  - A SQLite `CREATE TRIGGER … BEGIN … END;` splits at its inner `;`, the
    last piece a bare `END`.
  - `sqlcomplete.Complete` on the buffer `.` panics with
    `slice bounds out of range [:-1]`.

## What it found

In short: the parts hardened on purpose held up (the connection manager,
the retry rule, `etl.Copy`, run records, the scheduler, saved connections),
and the defects are at the edges those passes did not reach.

- **Data loss and crashes** (phase 1, N-247 … N-264): the splitter follows
  Postgres's comment rules on every engine and splits compound bodies; the
  completion panic; a pipeline `sql.read` that commits before its sink has
  loaded, with Preview committing a writing source; TUI editor text saved
  only on a clean quit, with no SIGHUP handler; consoles and history written
  in place; history that stops loading after one statement over 1 MiB; quit
  with an open transaction unasked; a formatter that turns `- -1` into
  `--1`; the web idle reaper cancelling running statements.
- **Reliability hardening** (phase 2, N-265 … N-275): silent web saves,
  dropped stream events with no resync, no `--max-rows`, no backstop for a
  node that ignores cancel, per-process overlap rules, no migration lock,
  Stop on Postgres always destroying the session, unknown TOML keys
  ignored.
- **Performance** (phase 3, N-276 … N-285): a result cut at `max_rows`
  still streams the whole result; cell cost follows value length in both
  grids; the TUI re-splits, re-wraps and re-lexes buffer-sized data per
  frame and per keystroke; completion builds every doc before filtering;
  the formatter is O(tokens × depth).
- **Usability** (phase 4, N-286 … N-301): headless errors as raw log
  lines, values losing detail on export, `CALL`/`FETCH` showing
  "0 affected", a failed query's message only in the log, no find in the
  TUI, no command palette, completion and quoting gaps.
- **Guardrails** (phase 5, N-302 … N-305): no fuzz targets and three
  benchmarks in the repo, no linter in CI, live database suites opt-in
  only.

## What was saved

- `ai_docs/plans/reliability-performance-usability.md`: the order and the
  reasons for it, a table per phase mapping each N-number to its place and
  its evidence, how the review was checked and what it did not check, what
  was looked at and found sound, a few things noted but not raised, and
  which suites to run for which items.
- `ai_docs/todo/next-list.md`: N-247 … N-301 at the end of Open, N-302 …
  N-305 at the end of Validate, Next ID now N-306. Each entry has where,
  the scenario, the fix direction and its evidence (*reproduced*, *read*,
  *audit, run*, *audit, traced* or *suspected*), and ends with
  "(RPU plan, phase N)".

## Choices made, for the author to overrule

- **59 entries from the 38 shown in chat.** The chat items were split into
  one-commit pieces, and audit findings left out of the chat summary were
  added (`explain.Detect` taking an ordinary result for a plan, the Origin
  check on GET, completion requests filling the browser's connections,
  among others).
- **Placement.** Everything that needs building went to Open, none to
  Roadmap; the four guardrail items went to Validate as tests with no
  product change. Sorting later phases into Roadmap is the author's call.
- **Values** follow the list's scale literally: `medium` for a visible
  defect nobody routes around yet, `low` for a gap nobody has met. None is
  `high`, the data-loss items included.

## Not checked

- Nothing ran against a live Postgres or MySQL. Claims about driver
  behaviour (a `rows.Close()` that drains, a cancel that closes the
  connection) are from the drivers' source. N-276 says to measure the drain
  on the live suites before fixing it.
- dbc web was not rendered in a browser; N-300's topbar overflow is from
  the CSS alone.
- No static analysis past `go vet`: the local `staticcheck` was built with
  Go 1.24 and refuses the Go 1.26 module, and running a current one was
  not permitted in the session. Raised as N-304.

## Next

Closed: None. Declined: None. Raised: N-247 … N-305 (59 items: N-247 to
N-301 in Open, N-302 to N-305 in Validate). Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
