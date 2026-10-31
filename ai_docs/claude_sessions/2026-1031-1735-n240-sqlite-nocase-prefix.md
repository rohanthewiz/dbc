# Designer: a starts-with needs a NOCASE index on SQLite (N-240)

Session: `e347e728-99ac-495a-9010-d08e0383975b`

## Ask

Next-list item N-240, pasted from the cats-todo backlog. On SQLite the
designer counted any index on a column as serving *starts with*
(`catalog.go finds`: off Postgres, a prefix was served). But SQLite's LIKE
ignores the case of A–Z by default, so it uses only a `COLLATE NOCASE`
index for `LIKE 'ab%'`, and the ⚡ over-promised. The item: read each key
column's collation (`pragma_index_xinfo`'s `coll`, keys only) into
`Index.Order`, count a prefix only on a NOCASE one, and say so in the ⚡
title.

Then `/sw`.

## What SQLite does (checked first)

`EXPLAIN QUERY PLAN` in sqlite3 3.51, before any code:

| index                                     | `LIKE 'ab%'` | `= 'ab'` | range | `ORDER BY` |
| ----------------------------------------- | ------------ | -------- | ----- | ---------- |
| `(ref)` on `ref TEXT` (BINARY)            | SCAN         | SEARCH   | SEARCH | index     |
| `(ref COLLATE NOCASE)` on `ref TEXT`      | SEARCH       | SCAN     | SCAN  | temp b-tree |
| `(code)` on `code TEXT COLLATE NOCASE`    | SEARCH       | SEARCH   | —     | —          |
| `(code COLLATE BINARY)` on a NOCASE column | SCAN        | SCAN     | —     | —          |

So a key column serves the LIKE only in NOCASE, and the column's own
comparisons only in the column's own collation. Also confirmed:
- `(status, ref COLLATE NOCASE)` serves `status = ? AND ref LIKE 'ab%'`.
- `ESCAPE '!'` keeps the range.
- On a column without TEXT affinity, a pattern starting with a digit is
  not served.

## What changed

- **`db/indexes.go`**: the SQLite listing joins `pragma_index_xinfo`
  instead of `pragma_index_info`.
  - It keeps key columns only (`ii."key"`). That cuts the rowid and a
    WITHOUT ROWID table's trailing primary key.
  - The ninth column is `nocase` when `upper(coll) = 'NOCASE'`. That
    covers a key named in the index, one inherited from a column declared
    NOCASE, and the name spelled in any case.
  - New `db.OrderNocase`, documented with the two-index example above.
- **`designer/catalog.go`**: `orderNocase`, `Catalog.sqlite()`, and
  `Catalog.btreePrefix(o)`, which says which b-tree order serves a prefix
  on each engine:
  - Postgres: a pattern class or `c`, unchanged.
  - SQLite: NOCASE only.
  - MySQL: any.

  `finds` asks it.
- **`designer/lint.go`**: on SQLite, `ruleFilterIndex` writes
  `(…, ref COLLATE NOCASE)` for a column filtered only by a starts-with.
  - Its why says NOCASE matches SQLite's LIKE, and that = and a sort want
    an index without it unless the column is NOCASE itself.
  - `unserved` names the plain index and gives the SQLite reason.
  - The rule needs row estimates, which SQLite never has, so in practice
    it stays quiet there. Its fix is still the one the planner uses.
- **The ⚡** (`designer.js indexTitle`, now given the analysis's dialect).
  On SQLite the dimmed title reads: "SQLite's LIKE ignores the case of
  A–Z, so a starts-with needs an index on (ref COLLATE NOCASE), beside
  this one for = and ranges". "Beside" is deliberate: a NOCASE index
  replacing the plain one would lose =, ranges and sorts (see N-242).
- **`designer/analyze.go`**: the `ColView.NoPrefix` comment.
- **README**: the filter rule's bullet and the ⚡ legend.

## Tests

- **`TestIndexesOnSqlite`** (new, `db`) runs the listing on a real
  in-memory SQLite. It covers:
  - plain and explicit-NOCASE indexes;
  - lower-case `collate nocase` in a composite;
  - NOCASE inherited from the column, and `COLLATE BINARY` over a NOCASE
    column;
  - an expression with a NOCASE key after it, and a partial index;
  - a WITHOUT ROWID table's primary key, and its secondary index, whose
    trailing primary key is cut.
- **`TestIndexesQueryOrderColumn`**: SQLite reads `pragma_index_xinfo`
  and writes `'nocase'`; the other engines do not.
- **`TestPrefixIndex`** gains SQLite cases:
  - A plain index makes the rule speak with `(ref COLLATE NOCASE)` and the
    SQLite why (no `text_pattern_ops`, CONCURRENTLY or "byte order"). The
    card shows `NoPrefix`.
  - The NOCASE index quiets the rule.
  - A Postgres `c` order is no NOCASE one.
  - An equality plus a prefix gives `(status, ref COLLATE NOCASE)`; a
    prefix plus a range gives a plain index.
- **`TestDesignerSQLitePrefixIndex`** (new, `workspace`, not opt-in) checks
  the card against `EXPLAIN QUERY PLAN` of the designer's own SQL, through
  modernc, the driver dbc ships. A LIKE the driver overrode would show
  here. The cases:
  - A plain index: `NoPrefix`, no SEARCH, and two rows (case-blind).
  - The rule's fix, from a catalog given a size by hand, run as written:
    the card is clear, SEARCH `ref>? AND ref<?`, and the rule quiet.
  - An escaped value (`ESCAPE '!'`): still a SEARCH.
  - A plain index on a NOCASE column: served.
- **`web/e2e` designer step**: two indexes on `customers`. It checks the
  real card's ⚡ class and title, dimmed for the plain index and full for
  the NOCASE one.
- **Mutation checks**:
  - `btreePrefix` returning true on SQLite fails `TestPrefixIndex` and
    `TestDesignerSQLitePrefixIndex`.
  - The listing writing `''` fails `TestIndexesOnSqlite` and
    `TestDesignerSQLitePrefixIndex`.
- **`indexTitle`** was run in node for each dialect and flag mix.
  `node --check` passes on `designer.js`.
- **Runs**: `go vet ./...` and `go test ./...` pass (33 packages, `CATS_*`
  stripped). The full browser suite passes (66 s; the Postgres and Docker
  steps skip). One earlier full run failed at 13.7 s on a step not
  captured; two reruns passed, and the designer step passed every time.
- **Not run**: the Postgres and MySQL live tests (Docker was not
  running). The Postgres prefix test in `btreePrefix` is equivalent to the
  old `o != ""`, and `TestPrefixIndex`'s Postgres cases pass.

## Left open

The reverse over-promise: an index whose key collation is not the
column's. A NOCASE index on a plain `TEXT` column serves only the LIKE,
yet it is counted for =, ranges and sorts. Postgres probably has the same
gap for an explicit `COLLATE "C"` index on an `en_US` column (unchecked).
Telling them apart needs the column's own collation. SQLite has no pragma
for it; a probe works, since a compound compares in its leftmost term's
collation:

```sql
SELECT count(*) FROM (SELECT ref FROM t WHERE 0 UNION SELECT 'a' UNION SELECT 'A')
-- 1 only under NOCASE
```

Raised as N-242.

## Commits

- The work, this doc and the next-list edits, together.

## Next

Closed: N-240. Declined: None. Raised: N-242.
Deferred: None. Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
