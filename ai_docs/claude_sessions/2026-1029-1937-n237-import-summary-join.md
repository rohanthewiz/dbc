# Open a summarized join as a design (N-237)

Session: `15ff10e0-8817-427f-bfc1-38a1d064c59d` (its first part, N-235, is
`2026-1029-1920-n235-like-escape-split`)

## Ask

Next-list item N-237, pasted from the cats-todo backlog. ◇ Open as a design
refused every derived table, so the designer's own *summarize* join
(`LEFT JOIN (SELECT key, sum(…) … GROUP BY key) s ON …`) could not be read
back after ⇪ Query. Every other shape round-trips. Reading it means
matching the derived table's select list to aggregates of one table, and
mapping `s.sum_x`, `COALESCE(s.count, 0)` and the re-aggregated forms back
to refs.

No choices were asked. Decisions made unasked, and reported at the end:
- **Faithful or refused, as the rest of the import is.** Only the
  generator's shape is read as a summary. A summary column written in
  another form is refused with the form that is meant, never read
  approximately. That includes a bare count with no COALESCE, which is
  NULL rather than 0 where there are none.
- **Hand-made names are kept.** A bare `s.paid` becomes an output with
  alias `paid`; the derived table's alias becomes the canvas table's.
- **`*` over a summary is refused.** The generator writes a summary's
  columns from the aggregates asked of it, so `*` would show different
  columns once regenerated.

Then `/sess-wrap`.

## What changed

- **Parser** (`designer/importparse.go parseFrom`): `(SELECT …) alias` in
  FROM is read into `fromItem.sub` instead of refused. An alias is
  required. Everything else about a derived table is left to the binder.
- **Binder** (`designer/import.go`):
  - `summaryItem` binds a derived table as a summary when it is the
    generator's shape (`sql.go summarySelect`):
    - LEFT JOIN, not first in FROM, no USING;
    - one table, its WHERE read as that table's filters (kept inside);
    - grouped by exactly the key columns (names or ordinals), every key
      shown is grouped by, and every key is paired in the ON with one
      earlier table;
    - nothing else in the ON; no HAVING, LIMIT or OFFSET inside. An ORDER
      BY inside is dropped with a note.

    The two parts that carry the meaning are LEFT JOIN (a parent with none
    keeps its row) and "grouped by exactly what it joins on" (each parent
    meets one summary row at most, so nothing repeats).
  - A `bound` may be a summary (`bound.sum`, a `summed` of `sumCol`s: key,
    or agg of col). `colName` answers for either kind.
  - Column lookup is split. `colRef` resolves to the table and the name it
    has there; `column` refuses a summary's column. So joins, EXISTS,
    USING and GROUP BY can never read a summary's column as a table's.
  - `summaryRef` (called from `subject`, before columns and aggregates)
    reads the outer forms that `sql.go summaryExpr` writes:

    |                 | not grouped              | grouped                                          |
    | --------------- | ------------------------ | ------------------------------------------------ |
    | sum(x)          | `s.sum_x`                | `sum(s.sum_x)`                                   |
    | count(x)/(*)    | `COALESCE(s.count_x, 0)` | `COALESCE(sum(s.count_x), 0)`                    |
    | min/max(x)      | `s.min_x`                | `min(s.min_x)`                                   |
    | avg(x)          | `s.avg_x`                | `sum(s.sum_x) * 1.0 / NULLIF(sum(s.count_x), 0)` |
    | count(DISTINCT) | `s.count_distinct_x`     | — (refused)                                      |

    Each other spelling naming a summary column is refused with the meant
    form: the key, a bare count, COALESCE of a sum, a sum of counts without
    COALESCE, an average of averages, a distinct count re-aggregated,
    arithmetic on a summary column, and a grouped form in WHERE.
  - `summaryForms` runs after the whole statement is read. It decides
    "grouped" as `sql.go isGrouped` does: an aggregate of a table that is
    not summarized, or count(*). It then refuses a summary column read in
    the other form. That covers a per-row value in a grouped query, and
    groups of nothing but re-aggregated summaries, which the generator
    would not group at all.
  - Smaller fits:
    - `(x op v OR x IS NULL)` reads its subject through `subject`, so a
      summary's total can be "or empty".
    - ORDER BY takes an arith (the average).
    - The RIGHT/FULL order check in `place` skips summaries.
    - USING refuses a summary's column.
- **Docs**: the import's header in `import.go` (read as written and
  refused) and README's "Opening a query as a design".

## Tests

- **`TestImportRoundTrip`** gets three summary designs, read back to the
  identical SQL on Postgres, MySQL, SQLite and bytdb:
  - every aggregate per row, with a filter inside, totals filtered in
    WHERE (one with OR NULL), and sorted;
  - grouped outside, with the average from its parts, a HAVING, and
    sorts on a count and the average;
  - two summaries under one parent, one asked for nothing (so it holds
    the generator's count).

  All three were checked valid with no lints before relying on them.
- **`TestImportHandWritten`**: a summary named by hand (bare key, GROUP BY
  1, ORDER BY inside, `LEFT OUTER JOIN … AS s ON o.id = s.order_id`),
  checked against the exact SQL the imported design writes; and a grouped
  one (`coalesce(sum(t.cnt), 0)`).
- **`TestImportRefuses`**: 19 summary cases, each checked for its reason.
  The old "derived table" case now refuses as "first in FROM".
- **`TestLiveDesigner`**: two hand-written summaries (per order with OR
  NULL; per customer with a count, the payments, a sum and the average)
  return the same rows as their imported designs on Postgres 17 and
  MySQL 8.4. I also ran them in psql directly: Ana 2 orders, 3 payments,
  120 paid, average 40; Bo and Cy none.
- **`TestDesignImportRoute`**: a hand-written summary through the HTTP
  route; the design's SQL run on SQLite returns `10|50|2 11|70|1 12||0`.
- `go test ./...` passes (`CATS_*` stripped); gofmt and go vet are clean.
  The live run used the docker recipe; both containers were stopped after.

## Raised

- **N-239**: with a summary on the canvas and nothing ticked, the
  generator writes `SELECT *`. That also shows the summary's key and its
  filler `count`, since `selectList` skips summaries but writes a single
  star as `*`. Writing the parent's `o.*` would fix it, and the import could
  then read it back. Checked in the generator.

## Commits

- The work, this doc and the next-list edits, together.

## Next

Closed: N-237. Declined: None. Raised: N-239.
Deferred: None. Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
