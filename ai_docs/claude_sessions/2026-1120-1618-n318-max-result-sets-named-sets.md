# N-318: `max_result_sets`, and headless output names a statement's sets

Session: `ed453f08-ac18-4a0c-8e6a-8473da3dff88`

## Ask

"Next list item N-318": four calls N-289 made without the user's word,
each easy to flip:

1. a statement's result sets capped at 51 (`db.maxMoreSets`), not a config key;
2. headless, a lone statement with several sets renders as a multi-result
   document — banners `1/1`, `1/1`, and in `-t json` envelopes where one set
   gives a bare array;
3. a verb in neither `queryVerbs` nor `execVerbs` runs as a Query (a round
   trip when it returns no rows) rather than an Exec (a silent "0 affected");
4. a different statement run over one of a `CALL`'s set tabs replaces that
   tab alone.

Then `/sw`.

## The premise, checked first

All four still stood as written: `db/stmtkind.go` (`maxMoreSets = 50`,
`kindOf`), `main.go` `runStatements` flattening `res.Sets()` at the
statement's position, `export.WriteRunWith` (lone statement + one result
→ bare; otherwise envelopes, banners `Pos(at, total)`), and
`workspace.placeRunLocked`'s sibling rule. N-332 had since added JSON row
streaming for a `kindRows` statement only (`db.OneRowSet`), so a `CALL`
still goes through the collected path.

## The user's decisions

They were asked, one question per call (each with its cost):

| Call | Answer |
| --- | --- |
| 1. set cap | **config key** |
| 2. lone statement's sets | **keep the shapes, name the set** (recommended) |
| 3. unknown verb | **Query**, as built (recommended) |
| 4. set tabs | **replace that tab only**, as built (recommended) |

## What changed

**`max_result_sets` (`config/config.go`, `db/stmtkind.go`)**

- `Config.MaxResultSets` (`toml:"max_result_sets"`), default
  `config.DefaultResultSets` = 51, the first set included. Unset, 0 or less
  is the default (as `max_rows`' is); 1 keeps the first set alone; no
  ceiling, since each set is already cut at `max_rows`.
- `Config.ResultSetLimit()` reads it with the default for a Config built in
  code (nil included), like `ResultTabLimit`.
- `readSets`: `maxMore := m.cfg.ResultSetLimit() - 1`; `maxMoreSets` is gone.
- The headless note and the workspace's log warning now end with
  "max_result_sets in config for more".

**Named sets in headless output (`export/export.go`, `main.go`)**

```
at        1     2        2        2        4      (statement 3 failed)
SetPos    -     1/3      2/3      3/3      -
banner   1/5   2/5 · set 1/3  …  2/5 · set 3/3   4/5
```

- `export.SetPositions(at)` reads a run of equal positions as one
  statement's sets — no two statements share a position — so no caller of
  `RenderRun`/`WriteRun` changed. `SetPos{K, N}`, `String()` "set 2/3",
  `PosSet(i, n, s)`.
- Text and markdown banners `1/1 · set 2/3`; HTML headings
  `Query Result · set 2 of 3` / `Statement 2 of 5 · set 1 of 3`; a JSON
  envelope of one of several sets gets `"set"` and `"sets"` (present only
  then, like `"truncated"`). A statement with one set is never named, so
  every other document is byte-identical.
- The block stream (several statements to stdout) is handed the set by
  `runStatements`' `onResult`, now `(i, r, export.SetPos)`, and writes it
  with the new `export.WriteSetBlockWith` (`WriteBlockWith` calls it with
  the zero SetPos). A script's shows stay `#i`: `blockStream.show` passes
  no set.
- Notes: a cut set is named (`statement 2/2 · set 2/2: result truncated…`,
  `set 2/2: …` for a lone statement, streamed CSV included via
  `rowStream.end`); the dropped-sets note names the statement, since only the
  first set carries `MoreCut`. `noteSetTruncatedAt` holds both;
  `noteTruncated`/`noteTruncatedAt` are kept as wrappers.

**Docs**: README (config block, result-sets paragraph, headless formats
table and the "one exception" paragraph), `dbc.example.toml`, the dbc
skill's `SKILL.md`, `model.Result.MoreCut`'s comment, `sdb/sdbapi/api.json`
regenerated (budget test passes; only a non-first sentence changed).

Calls 3 and 4 needed no code: recorded in N-318's closed line as kept by the
user's word.

## Verification

- New and changed tests: `config.TestLoadMaxResultSets` (absent, 5, 1,
  100000, 0, −3, zero and nil Config, demo); `db.TestReadSets` (the
  default from config, then caps 3 and 1); `export.TestRenderRunSetsOfOneStatement`
  (text, markdown, JSON, HTML, CSV unchanged), `TestRenderRunSetsAmidStatements`,
  `TestSetPositions`; main `TestBlockStreamNamesSets` (streamed equals
  collected; both notes) and a lone-statement case in `TestWarnTruncated`.
- `go test -race -count=1 -p 1 ./...` with both live DSNs (postgres:17 on
  55442, mysql:8.4 on 53316): exit 0, 34 packages ok.
- staticcheck 2026.2.2 (CI's, built into the scratchpad with Go 1.26.1):
  clean on `.`, `export`, `config`, `db`, `workspace`, `model`.
- Real binary in a scratch HOME against mysql:8.4, a three-SELECT procedure:
  text and markdown banners `1/1 · set 1/3`…`3/3`, JSON `"set"`/`"sets"`,
  CSV unchanged, HTML headings `Query Result · set k of 3`; `SELECT 0; CALL
  three()` streamed as `1/2`, `2/2 · set 1/3`…; `--max-rows 2` over a
  procedure whose second set has three rows: `note: set 2/2: result
  truncated at 2 rows` (lone CSV, streamed) and `statement 2/2 · set 2/2`
  (two statements); `max_result_sets = 2`: two sets kept, `set 1/2`, `set
  2/2`, and the dropped-sets note naming the key.
- Not done: the TUI and dbc web on screen (their warning text changed only).

## Notes

- Port 55432 is still held by another project's container
  (`bytdb-numeric-pg`); the live containers ran on 55442 / 53316 again.
- Under `max_result_sets = N` with more sets sent, the banners count the
  kept sets (`set 2/2`), and the note says more were dropped.
- `gofmt -l` lists `designer/import_test.go`, `sqlsplit/folds.go`,
  `tui/app_test.go`: untouched here, left alone.

## Next

Closed: N-318. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
