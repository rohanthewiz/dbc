# yaegi map-element bug: checked newer yaegi, reported upstream (N-127)

Session: `5f506041-d9fc-4d8d-a31d-2366e9961a63`

## Ask

Next-list item N-127: yaegi v0.16.1 silently drops `m[k], _ = v.(string)`
(the map stays empty, no error). Check a newer yaegi, or report it upstream
with a minimal repro.

## Findings

- **No newer yaegi.** v0.16.1 is still the newest tag. Master (`fcb76d1`,
  2026-02-09) behaves the same way. Upstream is quiet: that was the last
  commit, and 187 issues are open.
- **Wider than the type assertion.** A standalone repro (embedded
  interpreter, then a plain `repro.go` run under both `go run` and
  `yaegi run`) shows that any two-value assignment whose first target is a
  map index is broken:

  | Form | Go | yaegi |
  |---|---|---|
  | `m[k], _ = v.(string)` | stores | stores nothing |
  | `m[k], _ = other[k]` | stores | stores nothing |
  | `m[k], _ = f()` | stores | stores nothing |
  | `m[k], _ = <-ch` | stores | panics: `reflect.Value.SetBool on interface Value` |
  | `m[k], ok = v.(string)` | stores | stores nothing, though `ok` is `true` |
  | slice element, plain variable, single-value `m[k] = …` | stores | stores |

  The likely cause is that the two-value assign paths write to a temporary
  copy of the map element and never store it back into the map.
- **Already reported upstream.** traefik/yaegi#1655 (open since 2024-08,
  no replies) covers the `m[k], _ = f()` form. To avoid a duplicate issue,
  the repro, the Go/yaegi outputs, the scope and the workaround went there
  as a comment:
  https://github.com/traefik/yaegi/issues/1655#issuecomment-6026199993

## Changes

- `README.md` ("ETL across connections"): the interpreter-quirk note lists
  every affected form, says the bug is in v0.16.1 and on master as of
  2026-10, and links #1655.
- `scripts/copy_table.go`: the workaround comment references #1655 and the
  wider scope. The code is unchanged and the two-step workaround stays.
- `ai_docs/todo/next-list.md`: N-127 moved to Closed.
- Memory `yaegi-map-commaok-bug`: wider scope, the issue number, and a
  reminder to re-check when yaegi tags a release after v0.16.1.

## Tests

- Repro run against yaegi v0.16.1 and master, and under `go run` for the
  expected output. The repro files lived in the session scratchpad and were
  not committed.
- The repo change to Go code is a comment only. No test run.

## Next

Closed: N-127. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
