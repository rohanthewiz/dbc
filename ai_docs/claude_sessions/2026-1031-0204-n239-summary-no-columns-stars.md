# Designer: a summary with nothing ticked shows o.*, not * (N-239)

Session: `d126a64b-16a4-43d1-95b1-fe3bfb52273c`

## Ask

Next-list item N-239, pasted from the cats-todo backlog and worked in a
cats worktree (`todo/n-239-a-design-with-a-summary-an-0771`). A design
with a summary and nothing ticked wrote `SELECT *` (`sql.go selectList`:
the stars skip the summary, and a single star was written as `*`), so the
result also showed the summary's own columns: its key and the `count` the
generator puts in a derived table nothing asks anything of. The item:
write the parent's `o.*` whenever a summary is on the canvas, and let the
import read that `o.*` back (it refused `*` over a summary).

Then: merge the branch back into main, and `/sw` on main.

## What changed

- **`designer/sql.go` `selectList`**: with nothing ticked, the stars are
  still those of the main, cross and joined tables, and a summary marks
  the design as `summarized`. The single-star collapse to `*` now needs
  no summary on the canvas, so one table beside a summary is `o.*`. Two
  or more tables were already written as `c.*, o.*`; only that collapse
  was wrong. A grouped design (`count(*) AS count`) is unchanged.

  | canvas, nothing ticked        | before   | now          |
  | ----------------------------- | -------- | ------------ |
  | `orders`                      | `*`      | `*`          |
  | `orders` + Σ `payments`       | `*`      | `o.*`        |
  | `customers`, `orders` + Σ     | `c.*, o.*` | `c.*, o.*` |

  The derived table still holds the count when nothing asks it for a
  column. Nothing shows it now, but it keeps the derived table in the
  one shape the import's `summaryItem` reads.
- **`designer/import.go` `selectList`**: stars that cover every FROM
  table *but the summaries*, once each and in order, are read as
  nothing ticked. Before, `all` included the summary, which no star
  could cover, so `o.*` fell through to the per-item branch and was read
  as each of o's columns. That design then wrote the columns out, so the
  generator's own SQL did not round-trip. The `b.outs` for `ORDER BY 1`
  under stars now skip a summary, whose `et` is nil (unreachable before,
  a panic had it been reached).
- **The refusal is kept** for `*` and `p.*` over a summary: the derived
  table's columns as written are not what the generator writes from the
  aggregates asked of it. The reason now names the fix:
  `` `*` over the summary `p`: write o.* for the other tables' columns,
  or list the columns — … `` (names through `dialect.ident`).
- **README**: the import paragraph says a summary with no columns writes
  the other tables' stars, never `*`, that they open back as no columns,
  and that `*` over a summary is refused.

## Tests

- **`TestSummaryJoin`**: nothing ticked beside a summary writes
  `SELECT o.*` with the count-only derived table, is valid, and still
  gets the select-all hint.
- **`TestImportRoundTrip`**: two new designs, `orders` + Σ `payments`
  and `customers`, `orders` + Σ `payments` with a sort, both with
  nothing ticked. Each must write the same statement read back, on all
  four dialects.
- **`TestImportHandWritten`**: a hand-written `o.*` over a summary with
  `ORDER BY 1 DESC` opens with no columns, a summary join and a sort on
  `o.id`, and writes the designer's statement.
- **`TestImportRefuses`**: `*` (with the `write o.*` reason), `p.*`,
  `o.*, p.*`, and `*` over two tables and a summary (`write c.*, o.*`).
- **`TestLiveDesigner`** (Postgres 17, MySQL 8.4 in Docker): a
  hand-written `o.*` over a summary returns the same rows as its design.
- **Mutation checks**: with the old `sql.go` alone, `TestSummaryJoin`,
  `TestImportRoundTrip` and `TestImportHandWritten` fail, and the live
  test shows the bug itself: every row gains the summary's
  `order_id|count` (`…|10|2`). With the old `import.go` alone, the round
  trip fails on designs 10 and 11 on every dialect, and the hand-written
  case reads `o.*` as seven columns.
- `go build ./...`, `go vet` and `go test ./...` pass, with `CATS_*`
  stripped, except `tui.TestCatsReportsRunTransitions`, which timed out
  once in the full run (idle, idle, working) and passed three times
  alone. It is N-209's flake; `tui` was not touched.

## Merging into main

Main had gained N-238 (`72feb4b`, the dialect-aware sqlsplit), which
also changed `designer/import.go` and `designer/designer_test.go`. A
trial merge of main into the branch was clean, and the full suite passed
on it. That merge was dropped (the branch reset to its commit) so main
would get the repo's usual `Merge branch 'todo/…'`. Main's merge
(`2bca21b`) has the same tree as the tested one (`dc5dcaf`).

## Commits

- `cdb9a69` the work, on the todo branch; `2bca21b` its merge into main.
- This doc and the next-list edits, on main.

## Next

Closed: N-239. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None. Updated: N-209.
Full list: `ai_docs/todo/next-list.md`.
