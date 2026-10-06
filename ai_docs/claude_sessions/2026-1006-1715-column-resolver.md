# SQL resolver: columns the statement names (N-114)

Session: `b6f017db-8f7b-4648-8960-63465183a4f1`

## Ask

From the cats-todo backlog, N-114: extend the alias/CTE resolver
(`2026-1005-1352-sql-alias-rename`) to columns. The item named two kinds to
start with:

- a CTE's or derived table's output columns
  (`WITH t AS (SELECT count(*) AS n …) SELECT t.n`);
- select-list aliases used in ORDER BY.

It also asked for e2e coverage of Shift+F12's references list.

## What it does

F12, Shift+F12 and F2 (web and TUI) now work on:

- **A CTE's or derived table's output column.** It is declared by the
  body's select-list item, or by a column list (`WITH t(n) AS …`,
  `(…) AS d(n)`), which wins. It is used as `t.n`, and bare as `n` where
  nothing else could own it.
- **A select-list alias used in ORDER BY**, as a whole item
  (`ORDER BY n DESC`, not `ORDER BY n + 1`). A UNION's ORDER BY reads the
  first arm's names.
- **Pass-through.** A column passed on by another CTE or subquery
  (`SELECT n FROM a`, `SELECT * FROM a`, `a.*`) is the same symbol. F12
  goes to the origin, and a rename follows the whole chain, so
  `b`'s column stays `a`'s.

Refusals:

- **A CTE column that is just a catalog table's column**
  (`WITH t AS (SELECT o.id FROM orders o)`). It is found from `t.id` (F12
  lands on `o.id`) but not renamed; the reason says to alias it.
- **A rename that would change what any other reference in the statement
  resolves to.** That covers a sibling column with the same name, a bare
  use becoming ambiguous, and a subquery's bare name being captured.
- **Catalog columns.** They are still not resolved. A select item naming
  one, used nowhere else, resolves to nothing, as before.

## Design

All of it is in `sqlcomplete/resolve.go`. The new "Columns" section of the
package comment has the diagram.

- **Columns.** A `column` is either an origin or a pass-through:
  - an origin is an alias, a column-list name, or a select item naming a
    catalog column (`fixed`);
  - a pass-through has `via`, pointing at the column it references.

  A symbol is the origin. Its uses are every token whose `via` chain ends
  there.
- **Column sets.** A `colSet` holds a frame's output columns (its first
  arm's SELECT list) or a column list. Sets are read lazily and memoized.
  A set that is still being read reads as opaque, which keeps a
  self-referencing CTE from looping. `columns()` reads every frame up
  front so `declared` (token → column) is complete before uses are
  gathered.
- **Handles.** Each handle now carries `rel`: the CTE's set, the derived
  table's body set, or the alias's column list. `ref.subTok` was added in
  `scope.go` to find a derived table's `(`.
- **Bare names.** A bare name is resolved block by block, outward. In
  each block:
  - if any handle there has unknown columns (a catalog table, or an
    opaque set without the name), the name resolves to nothing;
  - if exactly one column matches, that is the answer;
  - if none matches, the walk moves out to the enclosing block, unless
    this block is sealed.

  CTE bodies and non-LATERAL derived bodies are sealed, since SQL does
  not let them see the statement's FROM.
- **ORDER BY.** `clauses()` records each token's clause per block (at the
  block's base depth) and each frame's SELECT token. `orderItem` checks
  that the name is a whole item.
- **Keywords.** `bare()` skips reserved words, a short list of keywords
  that are not reserved (`first`, `rows`, `zone`, …), `x::type`, `AS n`,
  `NULLS FIRST`, extract's field, and `date '…'`.
- **Rename check.** `columnTaken` sets `renTarget`/`renTo` so every column
  rooted at the target answers to the new name. It then re-resolves every
  token, the uses with the new text, and refuses the rename if any root
  differs from before, or if a set holding the target would hold another
  column of that name.
- **Shared refusal text.** `NotOnSymbol` is one exported phrase that the
  TUI uses. editor.js and the e2e test carry the same words as literals.
  The TUI menu gets a short "is a table's column" reason.

## Files

- `sqlcomplete/resolve.go`, `sqlcomplete/resolve_test.go`,
  `sqlcomplete/scope.go`
- `tui/symbol.go` (column kind, messages), `tui/symbol_test.go`
  (`TestColumnSymbol`), `tui/help.go`
- `web/static/js/editor.js`, `web/static/js/app.js` (F1 row)
- `web/e2e/web_test.go` (`aliasRename` extended; step renamed "go to and
  rename an alias and a column")
- `README.md` (the "Go to definition, usages and rename" section, both key
  tables)

## Verification

- **Resolver tests.** New: `TestResolveColumns` (19 cases),
  `TestResolveColumnsNothing`, `TestRenameColumns` and
  `TestRenameColumnsRefused`. They cover:
  - qualified and bare uses;
  - column lists;
  - derived tables;
  - pass-through by name, `*` and `t.*`;
  - correlated and LATERAL;
  - ORDER BY, including over UNION;
  - recursive CTEs;
  - catalog-bound refusals;
  - ambiguity, sealing, keywords and capture.
- **Mutation checks.** Each of these made a test fail: removing the seal
  check, the ORDER BY rule, and the capture re-check. The seal mutation
  first survived, because the cycle guard masked it, so its test now uses
  a CTE that is not in FROM.
- **TUI.** `TestColumnSymbol` covers F12 from a use, F2 renaming every
  use, and the menu reason on a table's column.
- **Browser.** The `aliasRename` e2e step now covers:
  - Shift+F12 on a CTE column opens Monaco's peek list with 3 rows, the
    first being the declaration and the second (the caret's) selected;
  - F2 renames `n` → `total` at the declaration and both uses, and the
    statement runs;
  - F2 on a catalog column shows the new refusal text.

  The first attempt matched `.ref-tree .reference`, which is wrong; the
  rows are `.monaco-list-row`, and there is no file row when there is
  one file.
- `go vet ./...` and `go test ./...` pass. The full e2e suite
  (`DBC_E2E=1`, `CATS_*` stripped) passes; the Postgres schema-picker step
  was skipped (no DSN).

## Next

Closed: N-114. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
