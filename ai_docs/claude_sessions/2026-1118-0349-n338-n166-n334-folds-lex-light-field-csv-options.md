# N-338, N-166, N-334: cheaper Folds and Lex, the narrow light field, CSV options; CI's Linux staticcheck

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the last batch of the run. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-338: Folds and Lex (`88fed13`)

- **Folds** takes its opaque spans from `lexEach`, the lexer's scan with
  a callback, and keeps only strings, quoted names and comments. It no
  longer builds all 349k tokens. One bracket stack is reused across
  statements.
- **Lex** allocated once per capitalized word (`strings.ToLower` in the
  keyword lookup). `isKeyword` lowers into a stack buffer instead.

| Benchmark | Before | After |
|---|---|---|
| Lex, 3.5 MB | 19.5 ms, 109k allocs | 14.2 ms, 34 allocs |
| Folds, 3.5 MB | 47 ms, 78 MB | 40 ms, 32 MB |

The Lex and Folds budgets in `TestScanCostsWhatItHolds` are tightened
(Lex 0.01 a token, Folds 0.8 a statement), and `TestKeywordsFitTheLookup`
is added.

## N-166: closed (`e46d37d`)

A temporary probe on the pipeline inspector's Go field covered both
themes:

- The field is 259 px wide.
- The error note reads at 5.28:1 dark and 4.79:1 light. The light warn
  colour is about 4.9:1 since N-300.
- A 200-letter name gives a note cut at 160 characters plus "…".
- The field scrolls horizontally, and the screenshots show the note's
  tail.

No defect found.

## N-334: CSV options (`fedd3f0`)

**The keys.** `csv_bom` and `csv_formula_guard`, both off by default.
They reach every CSV or TSV export to a file or stdout:

- the CLI's three paths: streamed rows, a run collected into `-o`, and
  blocks streamed to stdout;
- the TUI's export dialog;
- dbc web's download;
- `s.Export`, through a host-only `S.WithCSV`, so the script API's budget
  is untouched.

A clipboard copy never takes them.

**The BOM** is written once per document.

**The guard** quotes text starting with `=`, `+`, `-`, `@`, a tab or a
CR, unless the cell is a number. MySQL's text protocol sends an INT's -5
as a string, so a raw-type check alone would have guarded it. The header
row is never guarded.

**Tests:** across export, the CLI (stdout equals `-o`), config and web.
**Docs:** README, `dbc.example.toml`, the dbc skill.

## CI: Staticcheck failing on Linux (`a628295`)

**The failure.** Every Linux CI run since `2479d03` failed in the
Staticcheck step with S1016 at `clip/x11owner.go:105`. That was the
first completed run with the step; the runs before it were cancelled by
later pushes.

**Why it was missed.** The file is Linux-only, and my staticcheck runs on
macOS never analysed it. `GOOS=linux go run …staticcheck` can't be used:
it cross-builds the tool itself and gets "exec format error". The way
that works is a host-built binary run with `GOOS=linux`.

**The fix.** `x11Payload(c)`, a conversion instead of the struct
literal. It is clean as Linux and as macOS.

I saved this as a memory (`staticcheck-linux`).

## N-308

Updated: neither open flake showed up in this day's full runs.

## Next

Closed: N-338, N-166, N-334. Declined: None. Raised: None. Deferred:
None. Promoted: None. Moved: None. Updated: N-308. Full list:
`ai_docs/todo/next-list.md`.
