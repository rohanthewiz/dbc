# Designer: a starts-with needs a pattern index on Postgres (N-236)

Session: `dd5fbe29-9ba4-429c-bff6-76dc62f227ae`

## Ask

Next-list item N-236, pasted from the cats-todo backlog. The designer took
a b-tree index on a column as serving *starts with* (`lint.go sargable`),
and its fix was a plain `CREATE INDEX … (col)`. On Postgres that index
serves `LIKE 'abc%'` only under the C collation. In any other (the usual
`en_US.UTF-8`) it needs `text_pattern_ops` or `COLLATE "C"`. So the
advice was wrong and the ⚡ mark over-promised. The item: count a b-tree
for a prefix only when its operator class is a pattern one (the listing
would need `indclass`), and suggest `(col text_pattern_ops)`.

Then `/sw`.

## What changed

- **`db/indexes.go`**: the listing has a ninth column, the key column's
  *order*. Only Postgres fills it; MySQL and SQLite write `''`.

  | value     | what it is                                   | serves                 |
  | --------- | -------------------------------------------- | ---------------------- |
  | `pattern` | `text_`/`varchar_`/`bpchar_pattern_ops`      | =, prefix; no range or sort |
  | `c`       | default class under a C-ordered collation    | =, range, prefix, sort |
  | `''`      | default class under a linguistic collation   | =, range, sort; no prefix |

  `indclass` and `indcollation` are unnested beside `indkey`. A collation
  is C-ordered when it is libc `C`/`POSIX` or builtin (Postgres 17+). That
  matches Postgres's own `lc_collate_is_c`. The database default goes by
  `datlocprovider`: ICU is never C, builtin always is, and otherwise
  `datcollate` decides. `datlocprovider` is read through
  `to_jsonb(pg_database row)`, so the statement still runs on Postgres
  11–14, which lack that column. `BuildIndexes` keeps `Order` in step with
  `Cols`, including gap padding, and drops an all-`''` one. It still reads
  the eight-column shape.
- **`designer/catalog.go`**: `Index.Order`, `Index.sorts` and a new
  `Catalog.finds(et, col, need)`. It asks per kind of filter (`need{eq,
  rng, prefix}`), and one served is enough. A b-tree always serves =. It
  serves a range unless it is a pattern index. On Postgres it serves a
  prefix only when pattern or `c`; off Postgres, as before. Hash serves =
  only. SP-GiST (`text_ops`) and a trigram index serve a prefix. `leads`
  is unchanged and still drives joins and whether the ⚡ shows.
- **`designer/lint.go`**:
  - `filterCols` also returns each column's needs.
  - `ruleFilterIndex` asks `finds`. When an index starts with the column
    but cannot serve it, the why names it and says why (`unserved`).
  - For a column filtered only by a starts-with on Postgres, the fix is
    `(…, col text_pattern_ops)`. `char(n)` gets `bpchar_pattern_ops`,
    because `text_pattern_ops` does not accept it (`patternOps`). The why
    adds that the index serves = but no range or sort. It writes
    "equalities first, then …" only when there are two or more columns.
  - `ruleSortIndex` no longer counts a pattern or hash index as holding
    the sort's order.
- **Decided unasked and reported:** counting the reverse case. A pattern
  index serves no range and no `ORDER BY` (EXPLAIN confirmed both).
  Without this, the designer would have waved through a sort on the very
  index it had just suggested. Hash indexes now count for = only. An
  SP-GiST or trigram index quiets the prefix rule.
- **The ⚡** (`analyze.go`, `designer.js`, `app.css`): `ColView.NoPrefix`
  and `NoRange` are set only when the indexes are listed. The ⚡ is then
  dimmed, and `indexTitle` says which filters can use the index, e.g.
  "=, IN and ranges can use it, not starts with — in this collation a
  starts-with needs a text_pattern_ops index".
- **`workspace/design.go`** passes `Order` into the catalog.
- **README**: the filter rule's bullet and the ⚡ legend.

## Tests

- **`TestPrefixIndex`** (new) covers these index cases, checking each
  card's flags along the way:
  - A plain Postgres index: the rule speaks with
    `(ref text_pattern_ops)`. Adding that index quiets it.
  - `c` order, GIN and SP-GiST indexes stay quiet. A hash index speaks.
  - An equality plus a prefix gives `(status, ref text_pattern_ops)`. A
    range plus a prefix on the same column gives a plain index.
  - A pattern index finds no range and serves no top-N sort.
  - MySQL stays as before.
  - `patternOps` returns the right class for each type.
- **`TestBuildIndexes`, `TestIndexesQueryOrderColumn`** (new, `db`).
- **`TestIgnoreCase`**: its Postgres index on `ref` is now a pattern one,
  since a plain one no longer serves the uncased starts-with it checks.
- **`TestLiveDesigner`** (`livePrefixIndex`, Postgres):
  - grows `qd_orders` to 20k rows and runs `ANALYZE`;
  - takes the rule's own fix SQL and runs it, then checks that the rule
    goes quiet and the card says pattern;
  - checks with EXPLAIN (in a session with `enable_seqscan = off`) that
    the pattern index finds the prefix and not a range;
  - checks that, for a plain index, the card's `NoPrefix`, the rule and
    the planner agree, on whatever collation the database has;
  - checks that a `COLLATE "C"` index finds the prefix.

  On MySQL it checks a plain index's card has neither flag.
- **By hand on Postgres 17**, the listing classified every index kind
  correctly: plain, `text_`/`varchar_`/`bpchar_pattern_ops`,
  `COLLATE "C"`, a C column, `ucs_basic`, and an INCLUDE column left out.
  EXPLAIN agreed for each one.
- **Collations:** the live test also passed against three more
  databases: a C database, a builtin `C.UTF-8` one, and an ICU database
  with `datcollate` C. The ICU one is not C, and the planner indeed only
  filters there.
- **Mutation check:** with `finds` reverted to counting any b-tree for a
  prefix, `TestPrefixIndex` and `TestLiveDesigner` both fail.
- `go test ./...` passes, and all live tests (`-run Live ./...`) pass on
  Postgres 17 and MySQL 8.4, with `CATS_*` stripped. Both containers were
  stopped after. The ⚡ titles were checked by running `indexTitle` in
  node, not in a browser.

## Left open

SQLite has the same over-promise in another form. Its LIKE ignores the
case of A–Z by default, so only a `COLLATE NOCASE` index serves
`LIKE 'ab%'`. `sqlite3`'s `EXPLAIN QUERY PLAN` shows a plain index
scanned past. Raised as N-240. The size rules never speak on SQLite, so
only the ⚡ is wrong there.

## Commits

- The work, this doc and the next-list edits, together.

## Next

Closed: N-236. Declined: None. Raised: N-240.
Deferred: None. Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
