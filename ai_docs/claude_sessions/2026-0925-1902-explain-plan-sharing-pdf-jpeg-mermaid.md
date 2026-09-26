# Share explain plans as PDF, JPEG/PNG and Mermaid

Session: fb83b2ed-aa5f-438a-9384-fe3269e592a1
Date: 2026-09-25

## Ask

"I'd like to share the explain plan as a pdf, jpeg, and mermaid diagram."

## Approach

All three formats are rendered in Go, in the `explain` package, so the CLI (no
browser, CI boxes), the TUI and `dbc web` share one tested renderer. A
browser screenshot or canvas export would only have served the web.

- **Picture** (`explain/picture.go`): the graph view, drawn with
  `golang.org/x/image` (the `vector` anti-aliasing rasterizer and the Go
  fonts, all pure Go). It uses plan.js's layout numbers (card 250×108, gaps
  30/64, fold below depth 5 past 120 steps), its heat ramp and its edge
  weights. The header holds the headline, a facts line, the notes and the
  statement (up to 14 lines); the findings go under the tree. It is laid out
  in CSS px, measured at 1× with unhinted faces, and painted at `Scale` (2 by
  default), capped at 36M px and 20,000 px a side. The rasterizer is sized to
  each shape's bounding box, because a picture-sized Reset per shape is
  O(pixels). Strokes are built as offset polygons, since `vector` only fills.
  The Go fonts lack the kind glyphs (▤ ⋈ ◈ …), so cards show a kind tag
  (SCAN, JOIN) instead.
- **PDF** (`explain/pdf.go`): the picture on one page sized to fit
  (1 CSS px = 0.75 pt). It is written by hand: an RGB image XObject
  compressed with Flate after the PNG Up predictor, plus Info metadata
  (UTF-16BE for non-ASCII titles). It is a raster, so the text isn't
  selectable (N-058).
- **Mermaid** (`explain/mermaid.go`): `flowchart BT` with child --> parent,
  so the root sits on top and arrows follow the data. Labels carry
  op/target/summary/rows/share; edges carry the relationship and rows.
  classDefs (hot/warm/crit/warn/never) use the light palette. Only `"`, `#`,
  `<`, `>`, `&` and backtick are escaped as entity codes.

## Changes (`6841f17`)

| Piece | Change |
|---|---|
| `explain/picture.go` | `PictureOptions{Metric, Palette, Compare, Scale}`, `Picture`, `JPEG` (q90), `PNG`. |
| `explain/pdf.go` | `PDF`, `pdfText`. |
| `explain/mermaid.go` | `Mermaid`, `worstSeverities`, `clipRunes`. |
| `explain/save.go` | `WriteFile(dir, ext, data)` and `FileName(ext)`; `WriteHTML` now uses them. |
| `explain.go` | `-t pdf|jpeg(jpg)|png|mermaid(mmd)` via `explainFormat` (plan-only formats live in main, not `export`). Binary output on a terminal is refused before the explain runs. Unknown formats list explain's formats. |
| `web/plan.go`, `web/server.go` | `GET …/plan.pdf|.jpg|.png|.mmd` → `handlePlanFile` (`?metric=`, `?theme=light`, `?download=1`, compare text from the previous plan). `plan/text?what=mermaid`. |
| `explain/assets/plan.js` | Actions get `act(button)` and an optional `id` → `data-act`. |
| `web/static/js/planview.js` | ⤓ Save ▾ menu (Page, PDF, JPEG, PNG, Mermaid .mmd, ⧉ Copy as Mermaid). Keys: `s` opens it, `m` copies Mermaid. |
| `tui/explain.go` | Plan menu: Save as PDF, Save as JPEG (written to `planDir`, then opened), Copy as Mermaid chart. No new keys, since `m` is the metric key there. |
| `go.mod` | `golang.org/x/image v0.46.0`. `go get` also bumped `x/sys` 0.48.0, `x/text` 0.42.0 and `x/sync` 0.23.0. |
| `README.md` | "Sharing it where a page won't go" table, and the headless examples and `-t` list. |

## Verified

- New tests: `explain/picture_test.go` (Mermaid shape: one node per step,
  every edge child→parent; hostile-text escaping; classes; every fixture
  renders in both palettes with the right background; scale and metric
  fallback; a 400-leaf plan stays inside the limits; a 130-deep chain folds
  to 6 rows; JPEG/PNG decode; PDF xref offsets, MediaBox, UTF-16 title, and
  the image stream inflates + un-filters to the picture's pixels;
  `pdfText`; `WriteFile` naming). `web/plan_test.go` `TestPlanFiles`,
  `tui/explain_test.go` `TestPlanFilesFromTheMenu`, and `explain_test.go`
  `TestExplainOnlyFormats`.
- `go test ./...`, vet and gofmt are clean.
- By eye: fixture PNG/JPEG in dark and light; a PDF with statement, note,
  Cyrillic connection and compare text, rendered back through macOS `sips`.
- CLI against the demo: mermaid to stdout, `-t pdf -o`, jpg piped, png on a
  tty refused.
- Headless Chrome (go-rod scratch harness): the Save menu opens with its
  six items; the PDF and JPEG download as `plan-demo-bytdb-<stamp>.pdf|jpg`;
  the JPEG follows the light theme after a flip; `m` puts the Mermaid source
  on the clipboard.

## Notes

- The Claude Code safety check blocked a relative-glob `rm` of the scratch
  downloads folder after a `cd`. It wasn't needed, since the folder was new.
- Gopls showed stale diagnostics (an unrelated `main.go` importing go-rod,
  `WriteFile` undefined) that `go build`/`go vet` did not reproduce.

## Next

Closed: None. Declined: None. Raised: N-058, N-059.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
