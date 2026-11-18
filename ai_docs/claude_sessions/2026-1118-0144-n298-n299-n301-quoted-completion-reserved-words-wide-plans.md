# N-298, N-299, N-301: completion inside quoted names, per-engine reserved words, wide plans

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the sixth batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.

## N-299: names quoted by each engine's reserved words (`9dabfbd`)

**The problem.** Completion and the designer each kept one shared list
of Postgres clause words, used for every engine. On MySQL, completion
inserted `groups`, `rank`, `condition`, `range` and `interval` bare, and
the designer wrote `FROM rank r`. On SQLite the designer's
`SELECT r.transaction` failed.

**The fix.** New `sqlsplit/reserved.go` holds `IsReserved(engine, word)`
with each engine's own list:

- MySQL 8.4's reserved words, keeping 8.0's;
- every SQLite keyword (where SQLite's fallback lets one stand bare
  depends on its position, so quoting all of them is the safe rule);
- Postgres's reserved and "can be function or type" words. bytdb uses
  Postgres's list.

`sqlcomplete` `dialect.quote` and `designer` `dialect.ident` consult it
alongside their existing lists. Over-quoting is harmless.

**Checked on the servers.** On mysql:8.4, `SELECT 1 AS rank` is a syntax
error bare and works quoted; so do groups, condition, range, interval and
qualify. `manual` is accepted bare: it is over-quoted. On SQLite, bare
`transaction`, `foreign` and `commit` are syntax errors.

**Tests:** `sqlcomplete.TestQuoteEngineReserved` and
`designer.TestIdentEngineReserved`.

## N-298: completion inside quoted names, and a table's columns in its DDL (`1e66ac0`)

**Typing inside an open quoted name.** `FROM "User▮`, `WHERE "Em▮`,
MySQL ``FROM `ord▮``.

- `openQuotedName` lexes the statement and finds the quoted identifier
  the caret sits in, when it is not closed before the caret. A name
  holding an escaped quote is left alone.
- The word now starts at the opening quote (`Result.From`), and the
  prefix used for matching is the name after it. `completer.from`
  replaces the `caret-len(prefix)` arithmetic, so a dot directly before
  the quote still qualifies the name (`sales."Inv▮`).
- `quotedItems` keeps only names (tables, views, schemas, columns,
  aliases). Keywords, types, joins, snippets and routines are dropped.
- Each item replaces the quote with the name written as it needs. Its
  `Filter` gets the quote in front, because Monaco matches the typed
  range, quote included, against filterText.
- The scope is tokenized with the name closed at the caret
  (`buf[:caret]+q+buf[caret:]`). Without that, an open quote ran on to
  the next quote, and `SELECT i."Am▮ FROM sales."Invoices" i` lost its
  FROM.

**Columns in DDL.**

- `ALTER TABLE t DROP|ALTER|RENAME|MODIFY|CHANGE COLUMN ▮` offers t's
  columns (`alterTableTarget`, which takes IF EXISTS, ONLY and a schema).
  It used to offer clause keywords.
- `insertTarget` now also answers `CREATE [UNIQUE] INDEX [name] ON t (`
  (`createIndex`) and `REFERENCES t (`, with t's bare columns. The index
  case used to offer every catalog column, qualified. A join's `ON (` is
  unchanged.

**Tests:** `TestCompleteInsideQuotedName` and `TestCompleteDDLColumnLists`.
`TestNothingInLiterals` no longer lists `SELECT "ord▮`. `FuzzComplete`
ran 60 s (1.47M execs) clean.

## N-301: wide plans (`ba58f74`)

**The problem.** A long run of sibling steps drew side by side. An
Append over 200 partitions became a 20000×199 px strip.

**The fix.** `explain/picture.go` caps a step's children at
`picMaxKids` (8).

- `capKids` keeps the 6 heaviest children by inclusive share of the
  picture's metric (`picShowKids`), in plan order.
- `shownKids` drives the layout and the walk.
- One "+N more" box takes a card's slot after the kept children: "of the
  200 steps under Append", with their rows and their share together.
- The parent is centred over its children and the box.

The text tree and plan.js are unchanged.

**Test:** `TestPictureCapsSiblings`. 200 and 1,000 partitions now draw
about eight cards wide at full scale; the kept scans are the heaviest; 8
children are not capped. A 200-partition render was looked at.

## CI: the script API was stale (`5f09c98`)

The run for `3f1d690` (N-297) failed on both jobs: `sdbapi.TestAPIUpToDate`
found `sdb/sdbapi/api.json` stale, because of the new `S.PlanCopy`.
Regenerating it then failed `TestSummary`, the assistant's 14 KB budget
for the API summary.

`PlanCopy` left the script API. `copycmd.go` `planCopy` calls
`etl.PlanCopy` directly on `s.ETLConn`'s connections. N-297's closed
entry and the session doc before this one now say so.

From here, the full `go test ./...` runs before every push. Earlier pushes
ran only the touched packages, and that missed `sdbapi`.

## Next

Closed: N-298, N-299, N-301. Declined: None. Raised: None. Deferred:
None. Promoted: None. Moved: None. Updated: N-297 (its closed entry).
Full list: `ai_docs/todo/next-list.md`.
