# N-305, N-304, N-332: live suites in CI, staticcheck, a streaming JSON export

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the tenth batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-305: the live Postgres and MySQL suites in CI (`cdf2a1d`)

**The job.** `test.yml` gained a `live` job on ubuntu-24.04.

- It runs postgres:17 and mysql:8.4 service containers, both with
  password `pw`, database `dbc` and a health check.
- It sets the two DSNs and runs `go test -p 1 -count=1 -run Live ./...`.
  All 112 live tests are named `TestLive*`.

**Run here first, in the same shape** (Docker, `-p 1`). Everything
passed once the MySQL database was named `dbc`.

- With a database named `t`, `tui.TestLiveNavigatorMySQL` failed.
- Its picker types the base database's name and takes the first match,
  and `t` matched `dbc_tui_nav_other` first.

**CI.** Run 44266597786 passed the `live` job on its first try, its "Live
tests" step taking about 2 minutes, and both test jobs passed too. This
was the first green CI run of the session with every job, fuzz included.

## N-304: staticcheck in CI (`e576f44`)

staticcheck 2026.2.2, built with Go 1.26, made 14 findings.

**Fixed:**

- SA4006: the first row and col in `grid.dragTo` and `flipDragTo`, and a
  test's first y, were never used.
- S1003: a `strings.Index < 0` became `!strings.Contains`.
- ST1018: a literal zero-width joiner became `‍`.
- ST1008: a test helper's error now comes last.

**Removed:** unexported dead code (U1000): `Model.copyResult`,
`grid.sub` and `Model.gridView` (all left over from N-282), the
`FragmentBuilder.seq` field, and the `levelValues` test helper. The
linter proves nothing references them.

**Kept, with `//lint:ignore SA1019 <reason>`:** the legacy PEM decryption
in `db/tlskey.go`, and `driver.Conn.Begin` in `db/mysqlkill.go`.

**CI.** The test job runs a "Staticcheck" step after Vet, on Linux,
pinned to that version.

**Also:** gofmt on five files this session had edited without
reformatting (`main.go`, `copycmd.go`, `tui/app.go`, `tui/transpose.go`,
`db/mysqlkill.go`).

## N-332: a lone JSON query streams (`12fc134`)

This takes the item's second route; there is no new `jsonl` format.

- New `db.OneRowSet(stmt, driver)` is true for kindRows: a SELECT, WITH
  or VALUES. Not for a CALL, nor for a kindMaybe statement.
- `jsonStreams` sends such a statement through `rowStream`, as CSV
  already goes.
- New `export.JSONRows` writes the array one row at a time. `jsonArr` is
  built on it, so the streamed and collected bytes cannot differ.
- When the statement fails partway, the array on stdout is closed before
  exit 1.

Test: `TestRowStreamMatchesCollected` now covers JSON too.

Real binary: 1M rows × 6 columns, SQLite, `-t json --max-rows 0`:

| Build | Peak RSS | Time |
| --- | --- | --- |
| before | 688 MB | 2.43 s |
| after | 43 MB | 1.78 s |

The output was byte-identical (110 MB). The README's export paragraph is
updated.

## Next

Closed: N-305, N-304, N-332. Declined: None. Raised: None. Deferred:
None. Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
