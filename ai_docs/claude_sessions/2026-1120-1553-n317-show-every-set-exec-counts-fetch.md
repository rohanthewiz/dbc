# N-317: a script's s.Show shows every result set; s.Exec of a FETCH counts again

Session: `d38127f4-1586-44b7-b8c8-d8ff34133ea5`

## Ask

"Next list item N-317": since N-289 a statement can return several result
sets (`model.Result.More`), but a script's `s.Show(r)` showed the first
alone, and nothing in the examples or the README ranged over `r.Sets()`.
Two ways out were offered: `Show` shows every set, or it stays and the docs
carry the loop. Same neighbourhood: `s.Exec` of a statement that returned
rows handed back 0, where on Postgres a `FETCH` used to give its row count.
Then `/sw`.

## The premise, checked first

- `sdb.Show` called the host's `show` once with `r`; the three hosts (the
  workspace for the TUI and dbc web, headless `runScriptHeadless`, the jobs
  engine's Go-node session) each read a shown result's first set only.
  None of them looks at `More`, so the sets past the first were silently
  dropped.
- `s.Exec` is `run` → `RunNotices` → `Manager.run`, and returns
  `r.Affected`. A `FETCH` is a kindMaybe since N-289: run as a Query, and
  when columns come back `runMaybe` returned the rows with `Affected` 0,
  although `pgTagTracer` had just stored the tag's "FETCH n". Before N-289
  a FETCH ran as an Exec and returned n. Same for an `EXECUTE` of a
  prepared SELECT. A `CALL` with an INOUT parameter has the tag "CALL"
  (no count) either way; MySQL never had a count after a result set.
- A script has no session: each `s.Exec` takes a pooled connection, so a
  FETCH through it only finds its cursor when the pool hands back the same
  connection (a sequential script usually gets it). Rare, which is why the
  item is "value low".

## What changed

**`s.Show(r)` ranges the sets (`sdb/sdb.go`)**

```
s.Show(r) ──► for set in r.Sets():  host.show(evenRows(set))
              r (More, MoreCut kept) · More[0] · More[1] …
```

- Each set reaches the host as a result of its own, evened on its own:
  "Result 1 · 2 · 3" in the TUI and dbc web, `#1`…`#3` headless (a JSON
  element each), one kept result each for a pipeline's Go node. No host
  changed.
- The first set handed on is `r` itself, `More` and `MoreCut` and all, not
  a stripped copy: headless's `noteTruncated` words the dropped-sets note
  from `len(r.More)`. No host ranges a shown result's sets, so nothing
  doubles.
- `s.Show(r.More[i])` shows one set (it has no `More`); the doc gives the
  copy for showing `r`'s first set alone.
- `s.Export` is unchanged — it writes the one set it is given (a CSV holds
  one table); its doc, the README and the skill show the loop for exports.

**Postgres keeps the tag's count beside rows (`db/stmtkind.go`)**

- `runMaybe`: when columns came back, `res.Affected = tag.Load()`. So
  `s.Exec` of `FETCH 3` gives 3, of an `EXECUTE` of a prepared SELECT its
  rows. A kindMaybe is never cut (`query`'s `cut` is false), so a FETCH
  past `max_rows` is drained and its tag still says every row fetched.
- Every UI and export reads `Affected` only when `IsExec` (checked: tui
  `resultsTitle`, `ResultStatus`, the run summary, `writeLog.see`, export
  `summary`/`jsonAll`, web grid.js), so nothing on screen changed.
- `model.Result.Affected` got a doc comment saying when a rows result
  carries a count.

**Docs**: README (the `sdb.S` table; a result-sets paragraph with the
export loop; what `s.Exec` returns; scripts headless `#1`–`#3`), the dbc
skill's `scripting.md` (its N-289 loop replaced by a "do not write" and
the export use), `sdb/sdbapi/api.json` regenerated.

## Decisions made without the user

| Decision | Chosen | Cost / how to flip |
| --- | --- | --- |
| Show every set, or keep Show and document the loop | Show every set (the item's first option; "next list item N-317" named neither) | the loop `scripting.md` gave since N-289 now shows sets 2…n twice; flip = `sdb.Show` back to one `s.show(evenRows(r))` and restore the doc loop |
| `s.Exec` of a row-returning statement | Postgres tag count where pgx hands one over (kindMaybe only); else 0 as before | a SELECT through `s.Exec` stays 0 (it always was); MySQL stays 0 |

Recorded in N-317's closed line rather than as a new item: both are
two-line flips.

## Verification

- New tests, each failing on the old code (checked with `git stash` of the
  product files) and passing now:
  - `sdb.TestShowEverySet` — three sets handed on in order, the first `r`
    itself with `MoreCut`, the second evened on its own, a set from `More`
    alone, the doc's copy of the first set alone.
  - live `sdb.TestLiveShowEverySetMySQL` — a two-SELECT `CALL` through
    `s.Query` + `s.Show`.
  - live `sdb.TestLiveExecCountsFetch` — a one-connection pool and a
    `WITH HOLD` cursor: FETCH 3 → 3, MOVE 2 → 2, FETCH 10 with five left →
    5, EXECUTE → 4, SELECT 1 → 0; Query of the EXECUTE keeps rows and
    count.
  - live `db.TestLiveFetchCountPastMaxRows` — `max_rows` 2, `FETCH ALL`
    of five: two rows, counted 5.
  - `db.TestLiveStmtKindPostgres` now checks the counts (`kindShape`
    appends ` · counted n`), and the MySQL test that none come.
- `go test -race -count=1 -p 1 ./...` with both live DSNs (postgres:17,
  mysql:8.4 in Docker): exit 0, 34 packages ok.
- staticcheck 2026.2.2 (CI's version, built into the scratchpad with Go
  1.26.1; the one in `~/go/bin` is built with 1.24 and cannot load the
  module): clean on `sdb`, `db`, `model`.
- Real binary in a scratch HOME against both servers: a script that runs a
  three-SELECT `CALL`, `s.Show`s it, exports each set and `s.Exec`s a
  FETCH — text `#1`–`#3`, `-t json` three envelopes, three CSV files,
  `FETCH 4 → 4`; `script --check` clean.
- The sdbapi summary had 6 bytes under its 14 KB budget. Show's new first
  sentence and Affected's doc grew it; shortening `Export`'s first
  sentence ("renders … and writes it … the system clipboard") brought it
  to 14,328 (8 free).
- Not done: the TUI and dbc web on screen — added to N-316.

## Notes

- Port 55432 (the live-test recipe's Postgres port) was held by another
  project's container (`bytdb-numeric-pg`); the live containers ran on
  55442 and 53316 instead, and were stopped at the end.
- The first full-suite background task reported "completed" while its log
  held 10 of 34 packages; the same task id was reported completed again
  later, with the run's own `exit 0` line. Read a test log's own exit line,
  not the notification.
- zsh does not word-split `$PKGS` (in memory already): one rerun passed
  the whole package list as one malformed import path. `${=PKGS}` works.
- `go/doc`'s synopsis runs to the first period: a first sentence broken
  over two lines with a comma still counts whole against the summary
  budget.
- Commit `b20e263` holds the item and the list; this doc stamps the list's
  closed line with its stem.

## Next

Closed: N-317. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: N-316 (a script's `s.Show` of a
multi-set `CALL`, in either UI, added to its hand checks). Full list:
`ai_docs/todo/next-list.md`.
