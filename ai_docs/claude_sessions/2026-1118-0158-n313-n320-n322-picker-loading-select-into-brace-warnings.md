# N-313, N-320, N-322: pickers while loading, SELECT … INTO relists, stray ${ warned

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the seventh batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-313: the TUI's pickers while the catalog loads (`e724248`)

`pickWhy` gained a `CatalogLoading` case: "still reading <conn>'s
catalog — its databases are on their way" (or schemas).

- It sits after the driver's rule, since SQLite and MySQL really have no
  schemas to pick, and before the unread case.
- The first edit's string replace matched twice. It also put the case
  into the Tables pane's empty-text switch; that copy was removed.

Tests:

- New `TestLivePickersWhileTheCatalogLoads` (postgres:17). It lands a
  connect with its catalog read held back, as the running program has it
  in flight. A SQLite unit check was not possible: its driver rule
  answers first, correctly.
- `TestLiveNavigatorPostgres` and the SQLite mid-load test still pass.

## N-320: a Postgres SELECT … INTO relists the catalog (`015a755`)

**New method.** `sqlsplit.Dialect.ChangesCatalog` is the verb rule, plus
a SELECT whose main statement (past the CTE list) holds INTO.

- That counts on Postgres, bytdb and Generic.
- On MySQL and SQLite a SELECT … INTO makes no table, so the verb decides
  alone and `INTO @v` costs no relist.
- The package-level `ChangesCatalog` stays for callers without a
  dialect.

**Callers that now pass the dialect:**

- The workspace's run, through `dialectLocked`: `runEffects.d` for the
  relist and `writeLog.d` for `countless` (a SELECT INTO's line reads
  "done", as CREATE TABLE … AS does).
- `sdb.S.logDDL`.

Tests:

- `TestChangesCatalogSelectInto`.
- Live `TestLiveSelectIntoRelists`: the run ends with a Relist that lists
  the new table. The old code fails it with "want a Relist".

## N-322: a stray ${ in the config is warned about (`27fc834`)

`config.braceWarnings` scans a value as os.Expand does, skipping each
`$$` pair first. It warns about what os.Expand drops without a lookup:
an unterminated `${` (one warning) and an empty `${}`. Each warning names
the `$${` escape.

`ExpandDSN`, `ExpandTLS` and `resolvePath` add these beside
`unsetWarning`'s warnings. In the real binary:

```text
warning: scripts_dir has a "${" with no "}" — it is dropped and the text after it kept; write $${ for a literal ${
warning: connection "pgx": DSN has a "${" with no "}" — …
```

Test: `TestBraceWarnings`. One of its cases was wrong at first: `$$$${`
is two literal dollars, so no warning.

**A tooling note.** Two shell commands with heredocs full of `$` were
refused by Claude Code's removal-safety check: it could not parse them,
though nothing was deleted. The edits were made with the Edit tool, and
the commit message was written to a file for `git commit -F`.

## N-308 updated

Its "pipeline slow … was stopped" `tui` failure was seen once more, in a
full `go test ./...` after N-320. The failing test's name was cut by a
`| tail` again. It did not come back in `./tui` alone (three runs) or in
three full runs with the whole log kept. N-308 now says to keep the whole
log.

N-320's commit went in after that red full run, because the command
didn't gate the commit on the tests. The later full runs were green and
the flake predates the session; still, a commit should follow a green
run, not a red one.

## CI

The run for `ba58f74` was the first full one with the new "Fuzz (brief)"
step. It passed on Linux and macOS, every test included. The pushes held
back while it ran went out after it, ending at `27fc834`.

## Next

Closed: N-313, N-320, N-322. Declined: None. Raised: None. Deferred:
None. Promoted: None. Moved: None. Updated: N-308. Full list:
`ai_docs/todo/next-list.md`.
