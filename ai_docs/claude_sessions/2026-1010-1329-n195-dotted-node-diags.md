# N-195: a pipeline check's findings on a node whose id has a dot

Session: `ed102a18-83ad-4e52-b424-f39a647e768f`

## Ask

1. N-195, from the cats-todo backlog: a node id may hold dots
   (`pipeline.ValidName`), but `diagsAt` in `pipelines.js` keyed a node's
   diags by the `where` up to its first dot ("load/my.src.query" → node
   "my"), and `drawDiags` labelled a field diag by what follows the first
   dot. So a node called `my.src` got no ⚠ on its card, no list in its
   inspector and no marks in its code fields. Split `where` against the
   fragment's node ids instead (the longest id that prefixes it).
2. Make the gap found on the way (an invalid name's findings) N-196, then
   `/sw n195-dotted-node-diags`.

## What the check's `where` looks like

Read from `pipeline/check.go` before touching the JS:

| where                       | about                               |
|-----------------------------|-------------------------------------|
| `name`, `fragments`         | the pipeline                        |
| `params.<p>`                | a parameter                         |
| `<frag>`                    | a fragment                          |
| `fragments[i]`              | a fragment whose name is invalid    |
| `<frag>:edge a→b`           | an edge                             |
| `<frag>/<id>`               | a node                              |
| `<frag>/nodes[i]`           | a node whose id is invalid          |
| `<frag>/<id>.<field>`       | a field (`${…}` refs, conn names)   |

`nameRe` (`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,99}$`, no trailing dot) allows
dots in pipeline, fragment and node names but never `/` or `:`, so the
fragment is always what precedes the first `/` or `:`. Field names (a
plugin's config keys) hold no dots.

## The fix — `web/static/js/pipelines.js`

`diagsAt(e)` now:

- **Node wheres** (`frag/rest`): looks the fragment up in the spec
  (`fragOf`) and picks the longest node id that is the whole `rest` or is
  followed by a dot. Longest wins because a field name holds no dot: beside
  both `my` and `my.src`, `my.src.query` is my.src's `query`, never my's
  `src.query`. A `rest` naming no node the spec has (an invalid id's
  `nodes[3]`, a check older than a rename, no spec while the JSON does not
  parse) falls back to the old first-dot cut.
- **Labels**: each diag comes back as a copy with `label`: the field after
  the node id, what follows `:` for a fragment (`edge a→b`), what follows
  the first `.` for the pipeline (`params.days` → `days`). `drawDiags`
  prints `label: msg` instead of splitting `where` at its first dot.
- **Fragments first**: a `where` whose part before `:` names one of the
  spec's fragments is that fragment's, before the pipeline's own places are
  tried, so a fragment called `params.v2` keeps its findings on its lane.

Beyond the item, the same first-dot cut had also:

- mislabelled a dotted fragment's findings (`my.load` → "load: …");
- shown an edge's finding with no label at all (`load:edge a→b` has no
  dot), so "names a node the fragment does not have" did not say which edge.
  It is now "edge a→b: names a node …".

`fieldMarks` was left alone: it already matches the whole `frag/id.field`.
`card()` and the lane's count read only `msg` and `severity`, which the
copies keep.

## Checks

- A Node harness (scratchpad) ran the extracted `diagsAt` over 15 wheres:
  nodes `my`/`my.src`/`myx`, `nodes[3]`, a dotted fragment and its edge, a
  fragment named `params.v2`, `params.days`, `name`, `fragments`,
  `fragments[2]`, an empty where, and the no-spec fallback. All landed and
  labelled as above.
- e2e: "pipeline tabs" gains `pipelineDottedIDs`
  (`web/e2e/pipelines_test.go`), after `pipelineCodeField`. It loads
  `dottedPipeline` through the JSON view (nodes `my.src`, a `sql.read`
  on `lite` whose query is `select ${nope}`, and `my`, a preview), then
  checks:
  - the ⚠1 and `bad` on my.src's card, nothing on my's;
  - with my.src selected, the inspector's one finding reads
    "✗ query: ${nope} is not a parameter…";
  - the query's mini editor has one `dbc` marker at line 1, column 8 (the
    `${`);
  - the kept text put back, the tab is clean again (`text === saved`).
- Shown to fail on the old `pipelines.js` (`git show HEAD:…`): timed out
  waiting for "the ⚠ on my.src's card, not on my's". With the fix it
  passes. `DBC_E2E=1 DBC_E2E_STEPS="pipeline,job,plugin"` is green
  (pipeline tabs, job runs in the log, job tabs and the runs view, plugin
  files and the palette).

## Raised

- **N-196** — the gap the fallback leaves. `parseSpec` keeps a node with
  an invalid id (and a fragment with an invalid name) on the canvas under
  the name it has, but the check names it by index (`frag/nodes[3]`,
  `fragments[2]`), so neither a card, a lane nor the inspector shows its
  findings. Map the index to the spec's i-th node / fragment.
- **N-197** — found while checking N-196's wording: `pipeline.Locate`
  has N-195's bug server side. It does `strings.Cut(rest, ".")` for the
  node, so "w/my.src.query" looks for `"id": "my"`. Alone, my.src's finding
  is placed on the fragment's `"name"` line; beside a node `my`, on my's
  `"id"` line. That affects the JSON view's marks and the TUI's
  `file:line:col`. Try the longest prefix first.

## Files

- `web/static/js/pipelines.js` — `diagsAt` (split against the spec's
  names, `label`), `drawDiags` (prints `label`)
- `web/e2e/pipelines_test.go` — `pipelineDottedIDs`, `dottedPipeline`
- `ai_docs/todo/next-list.md`

## Next

Closed: N-195. Declined: None. Raised: N-196, N-197.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
