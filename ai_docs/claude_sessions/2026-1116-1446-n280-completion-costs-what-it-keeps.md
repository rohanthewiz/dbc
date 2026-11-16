# N-280: completion costs what it keeps, not the catalog

Session: `05d26188-5eb8-4de4-a95c-d3bdd366781c`

## Ask

"continue with N-280" (RPU plan, phase 3), then `/sw`.

N-280 said completion built every doc before filtering. `ddl()` and
`colDoc()` each called `outgoing()`, which scans all of the schema's Rels
and allocates a map, for every table or column, before `add` checked the
typed prefix. It had been measured at 32–34 ms a request on 5,000 tables ×
20 columns, even for `SELECT col▮` returning 5 items. The suggested fix was
to match the prefix first, build a doc only for the items kept (≤400), and
index rels by child once.

Commit: `627e510`.

## Measured first

New `BenchmarkCompleteBig` in `sqlcomplete/cost_test.go`. `bigShop(n, cols,
schemas…)` builds n tables spread over the schemas in turn, each after the
first with a foreign key (`prev_id → id`) to the one before, so a pass over
Rels per candidate shows as it would on a large real catalog. The benchmark
reports `items` and `docB` (doc bytes in the result) as metrics.

| case | before | after |
|---|---|---|
| `select-prefix`: `SELECT col▮`, 5 items | 31–34 ms, 155,841 allocs | 0.73 ms, 5,601 |
| `from-empty`: `SELECT * FROM ▮`, 400 of 5,000 | 32 ms | 3.1 ms |
| `from-prefix`: `SELECT * FROM t_012▮` | 31 ms | 1.6 ms |
| `join`: `SELECT * FROM t_00100 a JOIN ▮` | 33–52 ms | 3.1 ms |
| `scope`: two tables joined, `SELECT ▮` | 0.94 ms | 0.75 ms |
| `from-schemas`: 3 schemas, path `app, public`, `FROM ▮` | 67 ms | 3.5 ms |
| `select-schemas`: the same, `SELECT col▮` | 65 ms | 0.73 ms |

The two `*-schemas` rows were not in the item. They are the quadratic
`bare()` scan described below, found while choosing the benchmark cases.

## What was built

### Match before building

The loops over the catalog now ask `c.matches(name)` before building an
item: `table`, `columnsOf`, `refColumns`, `joinClauses` and
`routineSuggestions`. Each tests the same text `add` would (the label, or
for a qualified column the bare name it filters on), so a name the prefix
drops costs a compare and nothing else: no detail string, no quoting, no
bare-name test, no doc.

In `joinClauses` the check comes before `newAlias`. That is safe because
`newAlias` is pure (it only reads the scope's refs), so the aliases of the
clauses that are kept do not depend on which were skipped.

### Docs after the cap

An item on its way to the result is now a `cand`: the `Item` plus a
`docSrc`, which names what its doc is made from (`docTable`, `docColumn` or
`docRoutine`, with the table, column or routine group). `add(it, rank)` is
`addDoc(it, rank, docSrc{})`. `finish` sorts, cuts at `MaxItems`, then
builds the docs of the items kept:

```
catalog ─► matches(name)? ─no─► dropped
              │yes
           addDoc: Item (label, detail, insert, sort key) + docSrc
              │
           finish: sort ─► cap ─► indexRels ─► doc(docSrc) for those kept
```

Zero items is still a nil list, as before, so JSON still says `null`.

### Rels indexed for the kept tables only

The first version indexed every relationship by child table, lazily, on the
first doc. That made `scope` slower (0.93 → 1.11 ms): with two tables in
scope the old code scanned Rels for about 40 columns, while the index cost a
map entry and an append for each of 5,000 relationships. `indexRels` now
seeds a map with just the tables whose docs are kept (≤400), then makes one
pass over Rels, appending only where the child is in the map. `scope` went
to 0.75 ms.

`ddl`, `colDoc` and `outgoing` take a `[]*erd.Rel` (the table's own) in
place of the schema. `outgoing` still skips a rel whose child is not the
table, so any list is safe to pass.

### The quadratic bare-name test

`bare(t)` decides whether a table can be inserted without its schema. With a
search path and several schemas, it looped over every table of each earlier
schema on the path, for each table offered: on 5,000 tables over three
schemas, 67 ms with nothing typed. It now looks among `byName[lower(name)]`,
the tables of the same name. Both find exactly the tables with schema `s`
and name `t.Name`.

`routineBare` had the same scan over `rbySchema`. `indexRoutines` already
built a map keyed by exact `schema + "\x00" + name`. It is now kept as
`c.rbyKey`, and the test is one lookup per earlier schema.

### Smaller things

- `addDoc` matches the prefix before it builds the `seen` key. The outcome
  is the same either way round (kept iff it matches and its Kind + Insert
  was not kept before, and only a kept item is marked).
- `sortKey` writes "rank · quality · label" with a `strings.Builder` in
  place of `fmt.Sprintf` (rank clamped to 0–9; no rank is negative).
- The prefix is lower-cased once (`c.lowPrefix`). `quality(word, p)` takes
  it lowered; `matchQuality` stays as the wrapper `TestMatchQuality` uses.
- `refColumns` uses the column it iterates, not `r.table.Col(col.Name)`,
  which is a scan per column, so quadratic in a wide table. Of two columns
  of one name, `add` keeps the first either way.

### Still there

- `index()`, which builds `byName` and `bySchema` over every table on every
  request: ~0.7 ms at 5,000 tables, about 42% of what remains in
  `select-prefix`. Left alone: far under a frame, and caching it per
  `*erd.Schema` would need care about the schema's lifetime.
- The 400 kept docs are still built, and dbc web sends them all (177 KB on
  `FROM ▮` at 5,000 tables), though each UI shows one. Raised as N-328.

## Verification

- **Output unchanged.** A throwaway test (`zz_dump_test.go`, deleted)
  marshalled every `Result` to JSON over 218,160 requests and wrote a
  SHA-256 plus the last-caret results per buffer. It was run with the
  change and then with the three source files stashed (HEAD), and the dumps
  were identical. The corpus was 37 buffers at every caret × 4 drivers × 3
  focus schemas × 5 search paths × 4 catalogs. One catalog had
  `sales.orders` shadowing `public.orders`, a case-variant
  `sales.Customers`, and routines of one name in two schemas. The dump held
  `public.orders` inserts (216), qualified routine inserts (588),
  `references customers(id)` docs (1,320) and `app.t_…` inserts (210), so
  the shadowing paths were exercised.
- **`TestDocsOfKeptItems`:** a table's DDL still carries its foreign keys
  (`-- → customers(id)`), a table with none has no arrow, and a join
  clause's doc is the joined table's DDL.
- **`TestCompleteCostsWhatItKeeps`,** the first budget under N-303.
  Allocations (`testing.AllocsPerRun`) on 2,000 tables × 10 columns, bounded
  per table in the catalog:

  | buffer | HEAD | now | bound |
  |---|---|---|---|
  | `SELECT col▮` | 21.4 | 1.3 | 4 |
  | `SELECT * FROM ▮` | 23.1 | 7.1 | 12 |

  It fails on HEAD (checked with the source stashed) and counts the same
  under `-race`. Counting allocations rather than time keeps it stable
  across machines.
- `FuzzComplete` for 60 s: 2.86 M executions, clean.
- `go test -race ./...`: green except `web` `TestChatEffort` once, the known
  flake N-321 (`chat_test.go:406`, "view medium, agent high"). It passed 3
  of 3 reruns.
- e2e (`web/e2e`, `DBC_E2E_STEPS=completion`): "editor completion" and "a
  stale completion request is aborted" pass in headless Chrome. "routine
  completion" skips without a live Postgres; the dump covered routines.

## Found on the way

- The language server reported `resolve.go:289` passing a `Dialect` to
  `stmtWindow` mid-edit. It was stale: `go build` was clean throughout.
- Test files hold benchmarks and tests together, as `tui/gridcell_test.go`
  does. The new file was renamed from `bench_test.go` to `cost_test.go`
  once it held both.

## Next

Closed: N-280. Declined: None. Raised: N-328. Deferred: None. Promoted:
None. Moved: None. Updated: N-303 (`BenchmarkCompleteBig` and the first
budget, `TestCompleteCostsWhatItKeeps`). Full list:
`ai_docs/todo/next-list.md`.
