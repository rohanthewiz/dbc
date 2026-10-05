# Tab completes in the sidebar's schema picker

Session: `ddd5324a-1a73-4031-a680-5618f20641b8`

## Ask

From the cats-todo backlog, with a screenshot of the `dbc web` sidebar
(schema box holding `lym`, list narrowed to `lymbic_stg`): "Tab should
complete to the remaining selected schema, but it doesn't. It chooses another
schema beginning with `la_ea...`."

## Cause

The sidebar's pickers (`combo` in `web/static/js/app.js`, behind both the
schema and the database box) handled the arrow keys, PageUp/PageDown, Enter and
Escape, but not Tab. The browser therefore moved focus on. The box's `blur`
handler closes the list and runs `show()`, which puts the box back to the
**current** pick's name. The `la_ea…` schema was simply the one already
picked, so Tab looked like it chose it, though nothing was chosen.

## Change

- `combo`'s keydown handles Tab (not Shift+Tab) when the list is open, a row
  is highlighted, and the box holds typed text: it `preventDefault`s and
  `pick`s the highlighted row, the same as Enter. Focus stays in the box, so a
  second Tab (the list is closed by then) moves on as usual.
- The new helper `typed()` checks that the box is non-empty and differs from
  `o.label()`. Its job: just after focusing the box, the list is open on the
  current pick and the box shows that pick's name, and Tab there must still
  move focus rather than re-pick.
- The database picker uses `combo` too, so it gets the same Tab behaviour.
- `combo`'s header comment now says that Tab, like Enter, takes the first
  match.

## Tests

- `web/e2e/web_test.go` `pgSchemaPicker`: after the existing Enter → `e2e_b`
  step and its Show columns check, the box is clicked again, `e2e_a` typed,
  and Tab pressed. The step expects focus still on `#table-schema`, the box
  reading `e2e_a`, and the Tables list being `e2e_a.alpha`.
- First attempt put the Tab step *before* the `e2e_b` pick and failed. The
  server opens a Postgres connection on the first schema that has tables, so
  `e2e_a` was already current: `typed()` was false and Tab, correctly,
  moved focus to `#qnew`. Found by logging keydown `defaultPrevented` and
  `focusin` targets in the page. The step was moved after `e2e_b`.
- `DBC_E2E=1 DBC_LIVE_PG_DSN=… go test -count=1 -v .` in `web/e2e` with a
  throwaway `postgres:17` container: all 22 checks passed. `go test ./web`
  passed. The container was stopped afterwards.

## Next

Closed: None. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
