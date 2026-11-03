# N-242: index keys in another collation than their column's

Session: `1a7f9e6d-d1ff-4ca0-982b-9827135d7249`

## Ask

From the cats-todo backlog, next-list item N-242: an index whose key
collation is not the column's own serves none of the column's `=`, ranges
or sorts, yet the query designer counted them (`finds`, `Index.sorts`,
`leads`). On SQLite an index on `(ref COLLATE NOCASE)` over a plain `ref`
showed a fully lit ⚡; Postgres was suspected to have the same gap for
`COLLATE "C"` over an `en_US` column.

## What the engines do (checked first)

- **Postgres 17** (docker, `en_US.utf8` database): `(ref COLLATE "C")` on a
  default-collated column — `ref = 'x'`, `ref > 'x'`, `ORDER BY ref` all
  seq scan / sort; `LIKE 'ab%'` uses the index. The same in a database
  created with `lc_collate 'C'`: the planner compares collation **OIDs**
  (`IndexCollMatchesExprColl`), and "C" (950) ≠ "default" (100). The
  reverse (an `en_US` key on a `COLLATE "C"` column) serves nothing. A
  `text_pattern_ops` key in another collation serves the prefix, not `=`.
- **SQLite**: a NOCASE key on a BINARY column serves only the LIKE; a
  `COLLATE BINARY` key on a NOCASE column serves nothing (`=` compares in
  NOCASE, LIKE wants NOCASE). The UNION probe works: 'a'/'A' collapse to
  one row only under NOCASE, 'a'/'a ' only under RTRIM.

## What changed

- **`db/indexes.go`** — the listing has a tenth column, read into
  `Index.Recollated []bool` (parallel to `Cols`, nil when all false).
  Postgres: `k.coll <> 0 AND a.attcollation <> 0 AND k.coll <>
  a.attcollation`. MySQL: `''`. SQLite: the key's collation name, which
  `Indexes` resolves via `sqliteRecoll` — one statement of paired scalar
  UNION probes per distinct (table, column), names quoted whole (a dot in
  a name is kept). A custom collation is not judged; a failed probe leaves
  every key as its column's own (the old reading). `BuildIndexes` counts
  only the `recoll` mark.
- **`designer/catalog.go`** — `Index.Recollated`, `recollated(i)`.
  `finds`: a recollated btree key gives no eq/range, keeps
  `btreePrefix(order)`. `sorts(from, to)` rejects a recollated key in
  `[0, to)` (equalities before the sort too). `leads` unchanged (it still
  starts with the column) — callers needing `=` ask `finds`.
- **`designer/lint.go`** — `ruleJoinIndex` asks `finds(eq)` instead of
  `leads`; when an index does lead, its why comes from `unserved`, which
  has a new case: "X, which starts with c, keeps it in another collation
  than c's own, so it serves no = or range on it".
- **`designer/analyze.go`** — `ColView.NoEq`.
- **`web/static/js/designer.js`** — ⚡ dims on `noEq`; title lists `=`/`IN`
  as cannot, with "they need an index on (col) without the COLLATE"; a
  column nothing serves gets its own sentence.
- **`workspace/design.go`** — carries `Recollated` into the catalog.
- **README** — both the filter-rule bullet and the ⚡ paragraph.

## Tests

- `db`: `TestIndexesOnSqlite` (recollated both ways, an RTRIM column, a
  table/column name with a quote and a dot), `TestBuildIndexes`,
  `TestIndexesQueryOrderColumn`, new `TestSqliteRecoll`.
- `designer`: new `TestRecollatedIndex` (card, `=`, prefix, sort, sort
  after a recollated equality, join, a plain index beside; SQLite NOCASE,
  Postgres C, Postgres en_US-on-C).
- `workspace`: `TestDesignerSQLitePrefixIndex` extended — NOCASE index
  alone and BINARY on a NOCASE column, card vs `EXPLAIN QUERY PLAN`;
  `TestLiveDesigner` C-collated step now also asserts `=` and a range are
  not index-served (run against postgres:17, passes).
- `web/e2e` designer step: its fixture `(country COLLATE NOCASE)` on a
  plain `country` was this exact case and asserted the fully lit ⚡; now
  asserts the dimmed one. Ran headless: passes.
- Mutation checks: reverting the `finds` half, the `sorts` half, or the
  Postgres `recoll` column each fails its tests.
- `go test ./...` green.

## Next

Closed: N-242. Declined: N-244 (by the user, before this session's work;
committed here). Raised: None. Deferred: None. Promoted: None.
Moved: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
