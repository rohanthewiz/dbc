# N-197 and N-196: a check's findings placed by the names the spec declares

Session: `6c6849a2-4642-4fa4-8351-0a6f78cf1bc2`

## Ask

1. N-197: `pipeline.Locate` had N-195's bug server side. It cut a node's
   where at its first dot (`strings.Cut(rest, ".")`), so "w/my.src.query"
   looked for `"id": "my"`. That misplaced the JSON view's marks and the
   TUI's `file:line:col`.
2. Then N-196: an invalid node id or fragment name is named by its place in
   the check (`frag/nodes[3]`, `fragments[2]`). `parseSpec` keeps both on
   the canvas under the names they have, so no card, lane or inspector
   showed their findings.
3. `/sw n197-n196-locate-invalid-names`.

## Where's forms (from `pipeline/check.go`)

| where                        | about                                     |
|------------------------------|-------------------------------------------|
| `name`, `fragments`          | the pipeline                              |
| `params.<p>`                 | a parameter                               |
| `<frag>` / `fragments[i]`    | a fragment / one whose name is invalid    |
| `<frag>:edge a→b`            | an edge                                   |
| `<frag>/<id>` / `<frag>/nodes[i]` | a node / one whose id is invalid     |
| `<frag>/<id>.<field>`, `<frag>/nodes[i].<field>` | a field          |

`<frag>` is itself `fragments[i]` when the fragment's name is invalid, so
`fragments[1]/src.query` and `fragments[1]:edge a→b` occur too. No valid
name holds `[`, so a where shaped like an index is always one. Check
iterates in spec order, and both Go's `Parse` and the page's `JSON.parse`
keep the file's order, so "the i-th" means the same thing on both sides.

## N-197 — `pipeline/locate.go`

`Locate` now parses the text (every host calls it only on text that
parsed) and matches the where against the names the text declares:

- `fragmentAt(spec, part)` → the fragment, the name to search for, and how
  many fragments before it share that name. The index form is tried first,
  then the name. Without a spec, or when nothing matches (a check older than
  a rename), it uses `part` as written.
- `nodeAt(f, rest)` → the id, its earlier namesakes, and the field.
  `nodes[i]` (with an optional `.field`) maps to the i-th node. Otherwise the
  longest of f's ids that is the whole rest or is followed by a dot wins (a
  field name holds no dot). If nothing matches, it falls back to the old
  first-dot cut.
- `indexIn(s, list, n)` reads `list[i]`, bounded by the list's length.
- `findName(key, s, nth)` (a closure beside `find`) walks every
  `"key": "<json string>"`, decodes the literal and compares, skipping nth
  matches. Decoding finds `""` and names written with escapes; skipping finds
  a duplicate as itself.

Why the spec and not the item's "try each cut from the last dot back,
first `"id"` found after the fragment's name": a later fragment's node
with the longer id would be taken. For example, in "A/my.query" with A
having `my` and B having `my.query`.

Found on the way: the fragment's `"name"` was searched from the top, so in a
pipeline called like its fragment ("orders"/"orders"), the fragment's own
findings were marked on the pipeline's name. It is now looked for inside
`"fragments": [`. That regex can't hit a param (params are objects) or a
string value (its quotes would be escaped).

`TestLocateNames` (`pipeline/locate_test.go`) covers 13 wheres:
- the pipeline named like a fragment;
- `my`/`my.src` beside each other, a node and a field;
- `nodes[2]` and `nodes[3].query` on two nodes both called `a b`;
- a fragment named `""` (its own finding, a node's field, an edge);
- `my.src` alone in a dotted fragment `solo.f`.

Plus an unknown node (placed at its fragment) and `fragments[9]` (no
place). It fails in 10 places on the old `Locate`.

## N-196 — `web/static/js/pipelines.js`

- `diagsAt`: `nth(s, list)` reads `list[i]`. `frag(part)` is
  `e.spec.fragments[nth(part, "fragments")] || fragOf(e, part)`. A node
  where takes `f.nodes[nth(head, "nodes")]` first, else the longest-id
  match. `len` tracks how much of `rest` named the node, and the label is
  what follows. Keys are built exactly as the lane and card build theirs:
  `f.name` and `f.name + "/" + n.id`, raw, so `""`, `"a b"` and a missing
  name (`undefined`, which `el()` leaves off `data-*`, so the dataset gives
  `undefined` back) all line up with `e.sel`.
- `fieldMarks(ds, name, text)`: a field's own finding is the one whose
  `label === name`. It used to require `d.where === frag/id.field`, which
  `w/nodes[3].query` never equals. `drawDiags` no longer builds that where.
- **Bug found:** N-195's loop read `n.id.length` for every node of the
  fragment, so a node with no `"id"` threw a TypeError in `diagsAt`. That
  happens whenever the fragment has any node finding, and an id-less node
  always has one ("not a node id (got "")"), so the canvas broke. Only
  string ids are compared now.

## Checks

- A Node harness (scratchpad) extracted `diagsAt`/`fieldMarks` and ran 20
  wheres over a spec with `my`, `my.src`, two `a b`, an id-less node, a
  `""` fragment, a nameless fragment, and `solo.f`. Every finding lands on a
  key a card or lane has, with the field as label. The old code throws, and
  with the id-less node removed it keys `w/nodes[2]`, `fragments[1]` and so
  on.
- e2e: "pipeline tabs" gains `pipelineInvalidNames`
  (`web/e2e/pipelines_test.go`), after `pipelineDottedIDs`.
  `invalidNamesPipeline` has node `a b` (a `sql.read` with `select ${nope}`)
  → `show`, and a fragment named `""` with one `sql.exec`. It checks:
  - ⚠2 on a b's card and "⚠ 1" on both lanes;
  - with a b selected, the inspector's two findings ("✗ not a node id…",
    "✗ query: ${nope} is not a parameter…");
  - one `dbc` marker at 1:8 in the query's mini editor;
  - the JSON view's markers, by where and line:
    `fragments[1]@13 w/nodes[0].query@7 w/nodes[0]@7`;
  - the kept text restored and the tab clean.
- Shown to fail on each half alone. With the old `pipelines.js` it timed out
  on "the ⚠ on a b's card and on both lanes". With the old `Locate`, the
  marks were `w/nodes[0].query@5 w/nodes[0]@5`, on fragment w's `"name"`,
  and the fragment's mark was missing.
- Green: `go test ./pipeline ./jobs ./web ./tui`, and
  `DBC_E2E=1 DBC_E2E_STEPS="pipeline,job,plugin"` (pipeline tabs, job runs
  in the log, job tabs and the runs view, plugin files and the palette).
  The screenshot `pipeline-invalid-names` showed the card, both lanes and
  the marked query as expected.

## Raised

- **N-198** — the same bugs in jobs. A step id may hold dots, but
  `jobs.js` `diagsAt` keys by the first dot (a dotted step's findings fall
  into the job's list), the inspector labels by what follows the first dot,
  `lineOf` (the job JSON view's marks) searches `"id": "<first part>"`, and
  `jobs.Locate` splits on every dot. An invalid step's `pipelines[i]` lands
  in the job's list and on the `"pipelines"` key. Kept out to hold this
  change to the two items asked for.

## Files

- `pipeline/locate.go` — `Locate` (spec-matched, `findName`, the
  `"fragments": [` anchor), `fragmentAt`, `nodeAt`, `indexIn`
- `pipeline/locate_test.go` — `TestLocateNames`
- `web/static/js/pipelines.js` — `diagsAt` (indexes, raw keys, string-id
  guard), `drawDiags`, `fieldMarks` (by label)
- `web/e2e/pipelines_test.go` — `pipelineInvalidNames`,
  `invalidNamesPipeline`
- `ai_docs/todo/next-list.md`

## Next

Closed: N-196, N-197. Declined: None. Raised: N-198.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
