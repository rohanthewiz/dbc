# ERD ports, partitions, widened gaps and a shared raster package

Session: `d2b6cb32-6e5b-43e1-b094-bd996e1a9d28`
Date: 2026-09-30

## Ask

From the user's cats-todo backlog, Next-list item N-066. When several foreign
keys reference one parent column, their parent-end markers are drawn on top
of each other. A NOT NULL key over a nullable one reads `||o`, which is no
notation. The user also asked for "all other ERD related items in the Next list":
N-067, N-068, N-069, N-070, and N-062 (whose live tests include the ERD's).

## What changed

```
erd:     setPorts ─► route (measure) ─► widen ─► place again … ─► route ─► draw
db:      TablesQuery · SchemaColumnsQuery · SchemaKeysQuery · PartitionsQuery (new)
raster:  Fonts · Faces · Painter · Pt · CubicPts · Mix · RGB   ◄── erd, explain
```

### N-066: one slot per marker (`erd/route.go` `setPorts`)

- A **port** is one row of one box, on one side. Every line ending at the
  same port attaches to the same row from the same direction.
- A **slot** is one per distinct marker at a port (`||`, `|o`, `>o`). Slots
  are `portGap` (14) apart, centred on the row, and spread over at most
  `portSpan` (20), so every end stays inside its 21 px row.
- Keys with the same marker share a slot, and so share a marker: lines join
  it like a bus. `drawer.markers` now draws each distinct (point, direction,
  kind) once, after every line. Drawing the same marker once per line had
  made it look heavier from repeated anti-aliasing.
- Slots are ordered by the mean height of their lines' other ends.
- A **loop** (same-column or self-reference) takes the outermost slot toward
  its other end (`loopOrder`). The first render of the fixture showed why:
  with the loop in the upper slot, the line to `visits` crossed it.
- A key column referencing itself still attaches its parent end to the
  header, now as a port of its own (`"\x00header"`).
- The marker kinds moved into the path (`path.ckind/pkind`, `endMarkers`).

### N-070: widen crowded gaps (`widen`, `layGroup`)

- Placement is now a closure, `place()`, taking `extra[i][j]`: room added
  to the gap above column i's box j.
- **Measure.** `route(..., measure=true)` chooses holes only. Past a gap's
  capacity it charges `growLoad` (12), not `overLoad` (120): lines still
  spread into gaps with room first, but a full gap near them beats a detour
  round the column.
- **Widen.** `widen` grows only gaps past capacity, and only enough to fit
  their lines `minLane` apart. A first try that widened every used gap to
  `laneGap` spacing broke star(45)'s grid for no gain, so it was dropped.
  It loops up to 4 rounds; extra only grows, so the rounds end.
- **Numbers** (1×):

  | schema | lines round the group, before → after | size before → after |
  | --- | --- | --- |
  | star(120) | 7 → 0 | 2466×1996 → 2466×2099 |
  | star(400) | 98 → 0 | 4426×4018 → 4426×4571 |
  | star(45) | 0 → 0 | 1626×1197 → 1626×1208 |
  | fixture, chain(12), star(12) | 0 → 0 | unchanged |

### N-068: hide Postgres partitions (`db/schema.go`)

- `PartitionsQuery` lists `pg_class.relispartition` tables. It is Postgres
  only: bytdb has no partitioning and gets `""`, and `Manager.Schema` skips
  an empty query.
- `BuildSchema(..., hidden)` gives no box to a hidden table. So the keys
  Postgres cloned onto partitions, and the clones that reference partitions,
  fall away because one end has no box. `conparentid` is not needed.
- Labels still come from the whole catalog, so hiding a schema's only
  tables cannot turn every other label bare.

### N-069: the `raster` package

- The shared kit lives in `raster/raster.go`: the Go fonts, `Faces`
  (`Width`, `Fit`, `Wrap`), `Painter` (`Fill`, `RoundRect`, `Box`,
  `Circle`, `Polyline`, `Text`), `CubicPts`, `Mix` and `RGB`.
- `erd/paint.go` keeps only its text styles and palette. `pt2` and `txt`
  are aliases of `raster.Pt` and `raster.Style`.
- `explain/picture.go` keeps its styles, palette and heat ramp.
  `painter`, `picFaces` and `txt` are aliases there. Its edge `curve` is now
  `Polyline` over `CubicPts`.
- **Result.** The ERD renders are byte-identical before and after (6
  schemas). The plan renders differ only in the edge anti-aliasing (≤28/255,
  ~2,000 px per picture), checked by eye.
- `go vet` refuses unkeyed literals of an imported struct, so every
  `pt2{a, b}` became `pt2{X: a, Y: b}`.

### N-067: the ERD dialog in a real browser

- A scratch go-rod program ran against a built `dbc web`: a sqlite pets
  schema, a scratch HOME, clipboard permissions granted.
- **Copy.** Copy as Mermaid by mouse and by `m` both put the same erDiagram
  on the clipboard, and the log said so.
- **Downloads.** All three come down as `erd-pets-<stamp>.<ext>`. The PNG
  and JPEG decode, and the `.mmd` equals the copied text.
- 12/12 checks passed. The program was not committed (N-051 covers that).

### N-062: live tests

- Docker was up. All 15 `db` live tests pass on postgres:17 and mysql:8.4.
- **Test bug found.** The first run showed `checkPets` keyed relationships
  by `Name`. On Postgres the prefix is a schema, so it needs `Label`; the
  test was fixed, not the code.
- `TestLiveSchemaPostgres` now also builds a partitioned table: one
  partition in each schema, one of them sub-partitioned, and a composite
  key to it.

## Tests

- **New tests:**
  - `TestPortsFanOutByMarker`, which also covers the loop's slot
  - `TestCrowdedGapsWiden`
  - `TestBuildSchemaHidesPartitions`, `TestPartitionsQuery`
  - `raster/raster_test.go`
- **Proven to catch regressions.** Each new check was run once with its
  fix turned off, and failed:
  - `loopOrder = 0` fails the loop check.
  - `widenRounds = 0` fails the widening test on star(120) and star(400).
  - A `PartitionsQuery` that returns `""` fails the live partition check.
- `go vet ./...` and `go test ./...` are green, and so is the live `db`
  suite.

## Notes

- `sips -c` crops around the centre, not from the top-left corner. Use
  `sips -Z` to view a whole picture scaled down.
- rod's `WaitDownload` switches the browser to `allowAndName`, so a file is
  saved under its download GUID. Read `info.GUID`; the suggested name is
  in `info.SuggestedFilename`.
- BSD `sed -E` has no `\b`. Use `perl -pi -e` for word-bounded
  replacements.

## Next

Closed: N-062, N-066, N-067, N-068, N-069, N-070. Declined: None.
Raised: N-071. Deferred: None. Promoted: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
