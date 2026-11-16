# N-281: formatting costs what the input holds

Session: `05d26188-5eb8-4de4-a95c-d3bdd366781c` (the same session as N-280,
`2026-1116-1446-n280-completion-costs-what-it-keeps`)

## Ask

"continue with N-281" (RPU plan, phase 3), then `/sw`.

N-281 said the formatter is O(tokens × nesting depth):
`sqlsplit/format.go`'s `try` laid out a group's whole subtree on one line
to measure it, at every level, with no early exit past column 80. A
left-nested `((a=0 OR a=1) OR …)` took 1.4 s at 5,000 deep, 27 s at
20,000, and 100k bare parens took 2 m 43 s. Format runs on the TUI's event
loop (`tui/format.go` `formatQuery`), so a generated query pasted and
formatted froze the editor. The suggested fix was to stop the flat
measurement once it is over the width, or to memoise flat widths per node.

Commits:

- `3eb44d3`: N-281
- `4c7ae14`: N-329
- `e36291f`: N-330
- `a82ff06`: N-331

## Measured first

New `BenchmarkFormatDeep` in `sqlsplit/formatcost_test.go`, over four
generators:

- `leftOr(n)`: an ORM's `((a = 0 OR a = 1) OR a = 2) …`
- `rightOr(n)`: `a = 0 OR (a = 1 OR (…))`
- `parens(n)`: `SELECT ((((1))))`
- `calls(n)`: `SELECT f(0, f(1, …))`, which breaks into blocks

"Before" was HEAD, at `-benchtime=1x`:

| case | before | after |
|---|---|---|
| left-or 1,000 | 58 ms, 31 MB | 1.6 ms, 4.6 MB |
| left-or 5,000 | 1.7 s, 774 MB | 10 ms, 24 MB |
| left-or 20,000 | 27 s (item) | 44 ms, 117 MB |
| right-or 5,000 | 1.4 s, 794 MB | 10 ms, 24 MB |
| parens 10,000 | 1.7 s, 451 MB | 7.4 ms, 15 MB |
| parens 100,000 | 2 m 43 s (item) | 77–93 ms |
| calls 1,000 | 47 ms, 42 MB | 4.3 ms, 22.7 MB |

The cost is now linear, about 2 µs a level.

## What was built

### The stop (N-281, `3eb44d3`)

`fmtWriter` gained `limit` (in flat mode, the width left on the line `try`
measures for) and `done()`:

```go
func (w *fmtWriter) done() bool { return w.flat && (w.broken || w.col > w.limit) }
```

Stopping cannot change an answer. In flat mode `col` only grows: `nl`
returns at once there, and the one thing that moves `col` back, a token
holding a line break, also sets `broken`. So once `col` is past `limit` it
stays past, `broken` never clears, and `try`'s answer is "no" whatever
follows. `try` keeps its own test spelled out (`m.broken || w.col+m.col >
fmtWidth`), which is `m.done()` with `limit = fmtWidth - w.col`.

`tok` writes nothing once done. `inline` and `seq` (before `clauses`, and
between clauses) return once done, so the walk ends with the writing:
`tok` alone would skip the text but still visit every node below.

Memoising flat widths per node, the item's other suggestion, was not
needed. With the stop, a measurement writes at most about a line.

### The indent

The profile after the stop put `nl`'s `strings.Repeat(fmtIndent, n)` first
for deeply indented output: a string per line, as much allocation as the
output's indents. Appending `fmtIndent` once per level took `calls-1000`
from 4.8 to 10.4 ms (500k two-byte appends), so it was dropped. `nl` now
copies runs from `fmtIndents` (64 levels in one string). Both lengths are
multiples of `len(fmtIndent)`, so every run copied is whole indents. The
result for `calls-1000` is 4.3 ms and 22.7 MB, from 25.9 MB.

### Output unchanged

A throwaway test (`zz_dump_test.go`, deleted) hashed every `FormatWith`
and `FormatSpanWith` result, and the same run was repeated with
`format.go` stashed (HEAD). The corpus was:

- every SQL-looking Go string literal in the repo (4,614)
- the committed and cached `FuzzFormat`/`FuzzSplit` corpora (~3,000)
- `tutorial/workspace/seed.sql`
- 20,000 random statements with names of 1–24 bytes, so lines land
  around column 80
- the deep shapes at 1–300 deep

That is 27,633 inputs × 5 dialects × 3 keyword cases = 414,510 calls,
byte-identical.

### Tests

- `TestFormatCostsWhatItHolds`: bytes allocated per input byte
  (`runtime.MemStats.TotalAlloc`) at 8,000 deep, at most 2,000. It
  measures 351 for the OR chains and 723 for bare parens; HEAD gives
  18,252 / 18,597 / 17,643, so it fails there. It counts the same under
  `-race`.
- TUI e2e (`tui/e2e/tui_test.go`) step "formatting a deeply nested
  generated query keeps the editor (N-281)":
  - A new tab gets a 20,000-deep OR chain (282 KB, one line) by bracketed
    paste. The step then waits on the chain's tail, because the view
    follows the caret to the end of the line.
  - `ESC F` (Alt+Shift+F as a legacy terminal sends it) formats it.
  - "formatted the buffer" arrives in 105 ms. With HEAD's formatter the
    step times out at 15 s.

## Found while fuzzing: N-329, N-330, N-331

`FuzzFormat` was run to check N-281 and failed within seconds. Each finding
was checked against HEAD's `format.go`, and each fix got its own ID and
commit.

### N-329: the check, not the formatter (`4c7ae14`)

- **Input:** `";#\xd1\n;\xb0"` on MySQL, flagged "the text changed beyond
  layout".
- **The formatter's part:** it drops the empty statement's `;`, on purpose.
- **The check's part:** `squeeze` took out blanks and `;`s and only then
  lower-cased. That brought the two invalid UTF-8 bytes together as a
  valid rune (`\xd1\xb0` is Ѱ), which `ToLower` changed.
- **Fix:** lower-case first, so an invalid byte is U+FFFD before anything
  moves. The input is seed `5871c2d2f713d20e`.

### N-330: a digitless exponent ate a comment (`e36291f`)

- **Input:** `0E--;0`: 1 statement in, 2 out.
- **The bug:** `fmtLex` took the exponent's sign whether or not a digit
  followed. So `1e--note` became `1e-` and `-`, written `1e- -note`: the
  comment became code, and a `;` in it a statement of its own.
- **Fix:** the sign belongs to the exponent only before a digit.
- **Tests:** two `TestFormatOperators` cases, which also check the
  splitter's statement count (`TestFormatKeepsTokensAndSettles` runs
  `fmtLex` on both sides, so it cannot see this), and seed
  `6d888e367eca7e4a`.

### N-331: `$name` split in two (`a82ff06`)

- **Input:** `CreAte trigger Begin 0 $end;0` on SQLite: 1 statement in, 2
  out.
- **The bug:** `fmtLex` took only digits after a non-quote `$`, so `$end`
  was written `$ END`. That END closed the trigger's body early, and
  `$1abc` came out `$1 abc`.
- **Fix:** the `$` parameter takes the word bytes after it, and `:name`
  takes `$` as any word does.
- **First attempt:** the `$` parameter took further `$`s too, as the
  splitter's word would. FuzzFormat found `0:A$$;0` (before `:name` took
  `$`) and `$$0$$$$;0`: 2 statements in, 1 out.
  - Cause: `dollarTag` refuses a quote at a `$` only for what is glued on
    before it.
  - The formatter can write the token after a space, and a free-standing
    `$$` or `$a$` opens a quote.
  - Stopping at the next `$` keeps every token tag-safe: `$a` and a lone
    `$` never open one.
- **Seeds:** both inputs (`99d704028ebaf9a7`, `628baf3746e8daba`) beside
  the trigger's (`a7b93633102fc473`).

### Checks on the lexer fixes

- **Scope of the change.** Over N-281's corpus, N-281-only against all
  four commits: 13,604 of 27,938 inputs differ (the random generator
  plants `$x`, `:x`, `1e`, `2E-` prefixes), and every one holds `$` before
  a name byte, `:name…$`, or an exponent sign before a non-digit. None is
  unexplained.
- **Invariants.** Counted over the corpus (same statement count, same text
  but layout):

  | version | violations |
  |---|---|
  | HEAD | 45 |
  | N-281 only | 45 |
  | all four commits | 30 |

  The 30 left are inputs starting with a BOM, which Format strips on
  purpose.
- **Fuzzing.** FuzzFormat then ran 180 s (36 M executions) clean.
- **Each commit alone.** Each of the four commits was checked out in a
  clean worktree (no stray seeds on disk): `go vet` and
  `go test ./sqlsplit` pass at every one.

## Also found

- **N-321 updated, not fixed.** A full `go test -race ./...` reported a
  data race under `web` `TestChatArchiveNewOpenDelete`.
  - `chatEnv` (`web/chat_test.go:29`) gives every chat a test server
    starts the same `aitest.Fake`.
  - `aitest.Start` overwrites its `out` and `cancel`
    (`ai/aitest/fake.go:55`) while the previous session's `serve`
    goroutine still reads them (`:123`).
  - A model switch restarts the chat, so the old session can write into
    the new chat's pipe: a likely cause of the `TestChatEffort` flake.
  - Both tests passed 5 of 5 reruns.
- **Suite and e2e.** Full `-race` suite: everything else green. TUI e2e
  suite green with the new step. Pre-existing `gofmt -l` hits
  (`sqlsplit/folds.go`, `tui/app_test.go`) were left alone.
- **Pitfall.** zsh does not word-split an unquoted `$VAR`. `env $U …`
  failed, and the `&&` chain stopped before the stash, which was
  harmless here. Recorded in the session-pitfalls memory.
- **How the four commits were cut from one working tree.** Intermediate
  `format.go` and `format_test.go` versions were built in the scratchpad
  with Python, by reverting specific hunks of the final files. Each
  version was diffed to confirm it held only its item's hunks.

## Next

Closed: N-281, N-329, N-330, N-331. Declined: None. Raised: N-329, N-330,
N-331 (each closed the same session). Deferred: None. Promoted: None.
Moved: None. Updated: N-303 (`BenchmarkFormatDeep` and the second budget,
`TestFormatCostsWhatItHolds`), N-321 (the shared `aitest.Fake` race).
Full list: `ai_docs/todo/next-list.md`.
