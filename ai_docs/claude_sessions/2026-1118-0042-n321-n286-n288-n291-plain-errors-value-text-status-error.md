# N-286, N-288, N-291: plain errors, values keep their detail, the error in the status line (and N-321's record)

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`: do every Open and Validate item in
`ai_docs/todo/next-list.md` that does not need the user, committing and
pushing after each and wrapping every two or three. This doc covers the
first batch.

The triage printed at the start:

- **Doable here.** Medium value first: N-286, N-288, N-291, N-292, N-243,
  N-302. Then the lows: N-283–N-285, N-287, N-290, N-293–N-301, N-307,
  N-309, N-310, N-313, N-319, N-320, N-322–N-324, N-327, N-328, N-332,
  N-333. Then the Validate items that can run here: N-166 (a headless
  light-theme render), N-209, N-220, N-225, N-245, N-308 (flaky or failing
  tests), and N-303–N-305 (budgets, a linter, the live suites in CI).
- **Skipped, they need the user.**
  - N-188 waits on upstream: yaegi #1736 was still open on 2026-11-18.
  - N-221 is contingent: it only matters if the commit subject is wanted
    on releases.
  - N-314, N-317 and N-318 are the user's call.
  - Every hand check in a GUI, terminal or on real data: N-064, N-087,
    N-104, N-106, N-115, N-162, N-163, N-168, N-169, N-212, N-213, N-215,
    N-217, N-223, N-224, N-227, N-234, N-246, N-312, N-315, N-316.

**N-321's record.** The previous session closed N-321 with commit
`2d839c7` but never wrapped. Its Closed entry and N-333's `raised` still
held `@SESS@` / `@N-321@` placeholders. This doc fills them, so N-321 and
N-333 point here.

## N-286: a headless failure is one plain line (`db870f0`)

Before, `dbc "selec 1"` printed logrus's line through `logger.LogErr`:
`time=… level=error msg=… conn=… error=… fields.msg=… function=…
location=…`.

`errline.go` is new:

- **The plain line.** `errLine(err, msg)` writes
  `dbc: <msg>: <wraps>: <error> [k=v …]`:
  - The serr wrap messages are stored innermost first under `msg`, and
    printed outermost first.
  - The fields are serr's `UserFields()`, so no `location`/`function`
    frames. A repeated pair is said once, and a value with a space, quote,
    bracket or `=` is Go-quoted.
  - The fields go in brackets at the end, because an error's text often
    ends in its own parenthesis (`(1)`, `(SQLSTATE …)`).
- **The structured line, on request.** `reportErr` prints the plain line,
  or `logger.LogErr` when `DBC_LOG_ERRORS` is set. `0`, `false`, `no` and
  `off` count as unset. `fail`, `reportFailed` (`-k`) and the rollback
  note all go through it.
- **An unknown connection is a usage error.** `unknownConn(cfg, name)`
  says `no connection named "nope" — configured: lite, other (or
  --driver/--dsn for an ad-hoc one, named "dsn")`.
  - `pickConn` now checks the name before anything opens and calls
    `usage` (exit 2), for every headless command that uses it.
  - `checkCopyConns` uses the same message.
- **A stray bare word is refused.** `checkBareWord` runs in `rootAction`
  when there is exactly one argument.
  - It refuses a lone bare word (ASCII letters, digits, `_`, `-`) that is
    not a one-word statement (`begin commit rollback end abort vacuum
    analyze analyse checkpoint reindex`).
  - It suggests the nearest command or alias by `config.EditDistance`
    (newly exported; the optimal-string-alignment distance the config's
    unknown-key hint already used). The limit is 2 edits, or 1 for a word
    of four letters or fewer. Hidden commands are skipped.
  - The message ends with the escape hatch: a trailing `;` runs the word
    as SQL.
- **The tutorial reads the new line.** `tutorial.lastLine` drops
  `dbc: ` and a final ` […]` group. It still reads the structured line too.
- **Docs.** The README's headless section has a paragraph and a sample.
  The dbc skill's error line is updated, and so is the ETL tutorial's
  sample copy failure.

What the real binary printed, on a scratch SQLite config with two
connections:

```text
$ dbc "selec 1"
dbc: query failed: SQL logic error: near "selec": syntax error (1) [conn=lite]      (exit 1)
$ dbc -c nope "select 1"
no connection named "nope" — configured: lite, other (or --driver/--dsn …)          (exit 2)
$ dbc wbe
"wbe" is not a dbc command — did you mean "dbc web"? To run it as SQL, end it with a semicolon: dbc 'wbe;'
$ dbc "select 1; selec 2; select 3" -k
dbc: statement failed: … [conn=lite statement=2/3]
$ dbc copy --from lite --to other t      # duplicate key
dbc: copy failed: constraint failed: UNIQUE constraint failed: t.id (1555) [op=insert batch_rows=1 conn=other table=t op=commit from=lite:t to=other:t]
```

Tests: `TestErrLine` (wraps, duplicates, quoting, an serr inside a `%w`
chain, nil), `TestStructuredErrorsEnv`, `TestCheckBareWord` (`wbe`,
`scirpt`, `migrtae`, `pipelien`, `foo`; and `select 1`, `vacuum`, `wbe;`,
`"users"`, `now()` let through) and `TestUnknownConn`. `TestLastLine` is
extended.

Usage messages still print bare, with no `dbc: ` prefix, as every other
`usage()` call does.

## N-288: values keep their detail (`965b705`)

`db/cell.go`:

- **Column kinds.** `columnKinds(rows)` reads `ColumnTypes()` once per
  result set. `colKindOf` gives `kindDate` for `DATE`, `kindFloat32` for
  `FLOAT4` and `FLOAT`, and plain for everything else. `REAL` stays plain:
  SQLite's is 8 bytes. `cell` carries its column's kind, and `readSet` sets
  it.
- **`cellValueAs(v, kind)` writes each type like this:**
  - A time is `RFC3339Nano`. A DATE column's midnight is `2006-01-02`.
  - A float goes through `FloatText(f, bits)`: plain `'f'` digits when
    `0 == |f|` or `1e-7 ≤ |f| < 1e21`, and `'g'` outside that, which is
    JavaScript's and encoding/json's rule. NaN, Infinity and -Infinity
    take Postgres's spelling.
  - A float4 that pgx widened is narrowed back to float32 when it fits
    exactly. Its raw value is float32 too, so JSON writes `0.1`.
  - Bytes that are not UTF-8 become `\x` + hex, as psql shows a bytea.
    UTF-8 bytes stay text, which keeps MySQL's text protocol unchanged.
- **Export:**
  - `DelimitedRecord(dst, disp, raw)` turns a NULL into an empty CSV/TSV
    field. `delimited` and the headless `rowStream.Row` both use it.
  - `jsonValue` writes a NaN or ±Inf (float64 or float32) as its string,
    in `jsonArr` and `rowMaps` (the run envelopes). `Raw` is not changed.
- **Not done, raised as N-334:** the CSV formula guard and the BOM option.
  Both change bytes, so they want options. The item's "Fix:" did not name
  them.

Checked in the real binary:

- **Postgres 17** (Docker, port 55517):

  ```text
  1234567,0.1,NaN,-Infinity,2026-11-10,2026-11-10T12:00:00.25Z,\xff00,,NULL
  ```

  In JSON the same row has `"f4": 0.1`, `"nan": "NaN"`, `"n": null` and
  `"s": "NULL"`.
- **MySQL 8.4** (port 53384): FLOAT 0.1; a DATE with and without
  `parseTime`; `DATETIME(3)` with `parseTime` gave `…T12:00:00.25Z`; a
  BLOB of `unhex('ff00')` gave `\xff00`; NULL gave an empty field.
- **SQLite:** DATE gave `2026-11-10`, REAL 1234567 gave `1234567`,
  `x'ff00'` gave `\xff00`, and `1e300` gave `1e+300`.

Tests:

- `TestCellValueText` covers each rule and `colKindOf`.
- `checkFloatText` checks the round trip at its precision, the exponent
  rule and the non-finite words without calling `FloatText`. It now runs
  over `TestCellValueMatchesSprint`'s 2,000 floats. That test still holds
  every other type to fmt.Sprint, and times to RFC3339Nano.
- `TestWriteJSONNonFinite` replaces `TestWriteJSONRefusesNaN`.
- `TestDelimitedNull` is new.
- `TestWriteMatchesRender`'s reference `oldDelimited` now carries the
  empty NULL, and so does web `TestGridCopyRangeAndHidden` (a CSV copy).

The README's "Export formats" section says how values are written.

## N-291: the error in the status line (`fbc420c`)

`workspace.failedLocked` writes the status that both UIs show:
`error after 12ms: <statusErr(err)> — ✦ ask the assistant why`.

- `statusErr` takes the first line of `err.Error()`, with no serr fields.
  Text after a newline becomes ` …`; that is a Postgres error's
  DETAIL/CONTEXT. It is then clipped to `statusErrMax` (160 runes).
- **Why the status bar and not a banner:** the bar is drawn at every
  terminal size and under `z` zoom, and it needs no new pane state.
- The `error after` prefix is kept, so the TUI's colour rule and the
  existing tests still read it.

Tests: `workspace.TestStatusErr`, and `tui.TestShortTerminalShowsTheError`
(a 120×14 frame with the log hidden; the bottom line holds the error and
no fields).

**The trade:** on an 80-column terminal a long error pushes the hint off
the bar's right edge. That is deliberate; the error is what the user needs
there. This was not run in `tui/e2e` or a real terminal.

## Checks run

- `go build ./...` and `go vet` on the touched packages.
- The full `go test ./...` after N-288. Its one failure was the web copy
  test that expected the old `NULL`, which was then updated. Afterwards
  `./db ./export . ./web ./workspace ./tui ./tutorial ./config` passed.
- `gofmt` drift in `designer/import_test.go`, `sqlsplit/folds.go` and
  `tui/app_test.go` predates this session and was left alone.

Two throwaway containers are running for the items to come: `dbc-n-pg17`
(postgres:17 on 55517) and `dbc-n-my84` (mysql:8.4 on
53384). They are this run's own and are removed at its end. The other
container on this machine, `bytdb-numeric-pg`, is not this run's and was
left alone.

## Next

Closed: N-321 (recorded; the earlier session's commit `2d839c7`), N-286,
N-288, N-291. Declined: None. Raised: N-333 (recorded, from N-321's
session), N-334. Deferred: None. Promoted: None. Moved: None. Updated:
None. Full list: `ai_docs/todo/next-list.md`.
