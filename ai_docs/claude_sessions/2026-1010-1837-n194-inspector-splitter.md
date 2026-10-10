# `/next-list` review, then N-194: a drag edge for the canvas tabs' inspector

Session: `8e642965-dd57-4d6b-9332-b619516e0c78`

## Ask

1. `/next-list`, living-list mode: date every unfinished item, look for
   lapsed IDs, re-check each Open and Validate premise against the code,
   re-rate, write the findings back into `ai_docs/todo/next-list.md`, and
   print the sorted view.
2. "Do N-194": the pipeline inspector is a fixed 280px (`.pinsp`), tight
   for a Go field. Add a splitter on its edge, as the sidebar has, or a
   "⤢ wider" on a code field.

## The `/next-list` pass

- **No lapses.** All 199 IDs in the file's git history are still in the
  file, none are duplicated, and **Next ID** N-201 is above the highest
  (N-200). The last 15 session docs all use the `Closed: … Raised: …` form,
  and each one's Closed, Raised and Deferred IDs match the file.
- **Premises verified.** I checked the 23 Open and 15 Validate items (Open
  in two halves by agents, Validate by a third), plus N-156 and the
  Roadmap's lighter pass myself. Every item still holds. No value changed,
  nothing moved between Open and Validate, and no Roadmap item is ready to
  promote.
- **One wrong premise.** N-170's earlier correction said this machine had
  Homebrew's libpq 17.5 and no postgresql@16. The reverse is true now. No
  libpq keg exists, no pg_dump is on PATH, and the only pg_dump `Locate`
  finds is postgresql@16's 16.6 (installed 2024-12-05). So neither 18 nor
  17, the two newest majors "Postgres in Docker…" offers, can be dumped
  here.
- **N-188 evidence.** The module cache holds an untagged yaegi master build
  (`v0.16.2-0.20260209085605-fcb76d1ece0c`). I ran
  `TestYaegiStoreComputedBug` against it through a scratch `-modfile`,
  leaving the repo's go.mod untouched. It still passes, so the bug is still
  on master and a report upstream is still news.
- **Caveats on proposed fixes:**
  - N-182: only without `-o`; with `-o FILE` stdout has the stats alone.
  - N-187: `RunHead.Rows` is summed, not a stored header field.
  - N-193: `WrapSnippet` returns a header line count, not a byte length.
- **Scope grew:**
  - N-087 and N-106: the sidebar's Routines mode (`85952ae`).
  - N-087 also: the `/` find line (N-141).
  - N-150: `refreshPickLocked` now returns a loading pick, and DDL relists
    by itself (`kindRelist`).
  - N-166: the pipeline inspector's small editors draw the same marks.
- **Smaller notes:**
  - N-157: the e2e Postgres seed has no procedure to complete on.
  - N-169: a hand check on this machine needs a server of 16 or older.
  - N-161 (Roadmap): Docker Hub's newest 19 tag is still `19beta4`.

## Decisions (N-194)

- **A splitter, not "⤢ wider".** A splitter serves every field, not only
  code fields, and job tabs get it too. It is also the gesture the sidebar,
  the assistant pane and the row bars already use.
- **The bar is a sibling of `.pinsp`, not a child.** `renderInspector`
  replaces the inspector's children on every redraw. In `.pbody`, a 6px bar
  with `margin: 0 -3px` straddles the border and takes no width, and
  `position: relative; z-index: 3` lifts it over both neighbours.
- **One width for pipeline and job tabs.** `--insp-w` is set on the root
  element and saved as the layout key `inspWidth` (the layout is a free
  string map, so no server change was needed). The two inspectors are the
  same column in the same place.
- **The shared code goes in `stage.js` (`dbc.inspector`).** It is the file
  both canvas tabs already share, so the page's script list
  (`web/pages/workbench.go`) is unchanged. app.js boot calls
  `dbc.inspector.boot(layout)` next to `dbc.chat.boot`.
- **The clamp is flexbox's, not the drag's.** `.pinsp` is
  `flex: 0 1 var(--insp-w, 280px)` with `min-width: 200px`. `.pcanvas`'s
  `min-width` went from 0 to 160px. A width dragged past a bound stops
  there, and a window made narrower later is clamped by the same rules. On
  release the code stores the width the column actually got, so the
  variable never holds more than is shown.
- **Why not `max-width: calc(100% - 350px)`**, the first try: the e2e found
  the canvas left at 108px, not 160. The palette renders 242px, not its
  190px basis, because its min-content (a plugin name) wins over
  `flex-basis`, so any sum with 190 in it is wrong.
- **Floored at 0 in JS.** A negative `flex-basis` is invalid at
  computed-value time, so the property would fall back to `auto` and size
  the inspector to its content.
- **A bare click saves nothing.** As with `dragRows`, only a press that
  moved more than 2px fixes a width.

## How it flows

```
pointerdown on .pisplit
  └─ capture, body.insp-dragging (col-resize cursor, no selection)
pointermove
  └─ --insp-w = startW − dx  (on <html>; the stylesheet clamps)
pointerup, moved
  └─ --insp-w = the inspector's rendered width
     PUT /api/v1/layout {inspWidth: "<px>"}
dblclick
  └─ remove --insp-w (back to 280px), PUT {inspWidth: ""}
boot
  └─ app.js: dbc.inspector.boot(layout) → --insp-w from inspWidth
```

## Changes

- `web/static/js/stage.js`: `dbc.inspector` with `split(bar, insp)` and
  `boot(layout)`, plus a header note that the inspector edge lives there.
- `web/static/js/pipelines.js`, `web/static/js/jobs.js`: `mount` makes the
  `.pisplit` bar (`role="separator"`, a tooltip) between canvas and
  inspector and wires it.
- `web/static/js/app.js`: boot applies the saved width.
- `web/static/css/app.css`:
  - `.pinsp`'s flex basis is `--insp-w`, with `flex-shrink` and a 200px
    `min-width`.
  - New rules: `.pisplit` with its hover and drag state, and
    `body.insp-dragging`.
  - `.pcanvas` gets `min-width: 160px`.
- `README.md`: a sentence each in the pipeline and job inspector
  paragraphs.
- `web/e2e/pipelines_test.go`: `pipelineInspectorWidth`, a new step in
  "pipeline tabs".
- `ai_docs/todo/next-list.md`: the `/next-list` findings above, N-194 moved
  to Closed, and new N-194 notes added to N-064, N-061 and N-166.

## Checks

- `pipelineInspectorWidth` uses real pointer drags in headless Chrome, with
  src's go field open:
  - 200px left makes the inspector 200px wider, the field's editor follows
    (`getLayoutInfo().width`), and `inspWidth` is saved.
  - Far left leaves the canvas exactly 160px (it was 108 on the
    `max-width` try); far right stops the inspector at 200px.
  - A reload keeps 480px.
  - A double-click restores 280px, clears the variable and saves "".
- A screenshot (`DBC_E2E_SHOTS`) shows the wide inspector with its Go
  editor at full width.
- The full web e2e suite passes: 34 steps, 2 skipped (the schema picker
  and routines need `DBC_LIVE_PG_DSN`; the dump dialog step passed but
  skipped its Postgres half for the same reason). `go test ./web/` passes, `go vet` passes in
  `web/e2e`, and `node --check` passes on the four JS files.
- Not seen: the edge in WKWebView (N-064), Safari or Firefox (N-061), and
  a window too narrow for palette + 160 + 200. That last case now overflows
  `.pbody`, where the canvas used to shrink to 0.

## Files

- `web/static/js/stage.js`, `pipelines.js`, `jobs.js`, `app.js`
- `web/static/css/app.css`
- `web/e2e/pipelines_test.go`
- `README.md`
- `ai_docs/todo/next-list.md`

## Next

Closed: N-194. Declined: None. Raised: None. Deferred: None. Promoted: None.
Moved: None. Updated: N-061, N-064, N-087, N-106, N-150, N-157, N-161,
N-166, N-169, N-170 (premise corrected), N-182, N-187, N-188, N-193.
Full list: `ai_docs/todo/next-list.md`.
