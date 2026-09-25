# Assistant context: hidden columns stay out, rows follow the grid's sort

Session: 2c9f6cf6-920e-4698-9871-9b027b0f53ac
Date: 2026-09-24

## Ask

1. Next-list item N-036, pasted as-is: "The assistant's result context
   ignores hidden columns. 'Ask the assistant about this result' builds from
   `m.lastRes`, so it sends every column, while copies and exports now leave
   hidden ones out. Either is defensible … Decide which, and say it in the
   chip if hidden columns are dropped."
2. "do N-037 also". N-037 was raised while doing N-036: the assistant got
   rows in the query's order, not the grid's sort.

## Decisions

- **Hidden columns stay out of what the assistant gets** (N-036). Hiding is
  how a user says "not this one", and a sensitive column (salary, email) is
  what gets hidden before a result is shared. Sending it to a hosted model
  anyway would be a surprise. Getting it wrong the other way costs one `+`
  and asking again. This matches copies and exports.
- **The hidden columns' names still go.** Names are schema, not data, under
  package `ai`'s data rule. The model is told "The user hid these columns in
  the grid, so they are left out above: …", so it can say the answer may be
  in a hidden column instead of acting as if it doesn't exist.
- **Rows follow the grid's sort, and the model is told** (N-037). After a
  header sort, "the first 10 rows" should be the ones on screen. The prompt
  says the order is the grid's, not the query's, so the model doesn't
  explain an ordering the SQL never asked for. NULLs-last is stated because
  the grid does that in both directions.
- **The note** (chip and transcript) says how the rows differ from the
  query's result: `3 of 8 rows (sorted by age desc, 1 column hidden)`,
  worded like the copy log's `the result (8 rows, 1 column hidden)`. Hiding
  and sorting are only mentioned when rows are actually sent; with
  `ai_rows` off only names go, and the hidden names still do.
- **The context chip wraps to a second row** instead of truncating. At the
  default pane width the note was cut off right where "(1 column hidden)"
  sits, which would defeat the chip. Two rows at most, the second truncated,
  with the continuation indented under the note. Clicking either row toggles
  the context.

## What changed

### `ai/prompt.go`

- New doc sections: HIDDEN COLUMNS STAY HIDDEN, ROWS GO IN THE GRID'S ORDER.
- `Context` gained `Hidden []int` (result column indices), `Order []int`
  (the grid's row permutation, possibly only a prefix), `SortedBy`,
  `SortDesc`. The full rows are passed and `Build` narrows only the few it
  sends, because the chip builds a `Context` every frame.
- New helpers: `splitHidden` (ignores bad indices; hiding everything is
  treated as hiding nothing), `names`, `project`, `hiddenText` (names at
  most 40, `maxHiddenNames`), `pickRows` (a bad `Order` only means fewer rows
  are sent, never result-order rows mixed in), `sortText`, `pluralS`, `pick`.

### `tui/chat.go`

- `chatContext` passes `grid.HiddenCols()` and, when sorted, a **clone** of
  the first `ai_context_rows` entries of `grid.order`. The clone matters:
  `applySort` rewrites `order` in place, and a submitted question's context
  waits on its schema lookup before it becomes a prompt. Both are guarded by
  `grid.res == lastRes`.
- The context chip wraps (see above); the empty-pane hint mentions the sort
  order and hidden columns.

### Tests, docs

- `ai/prompt_test.go`: hidden columns with and without rows, bad and
  all-hidden `Hidden`, sorted rows and the combined note, a bad `Order`.
- `tui/chat_test.go`: `TestAssistantLeavesHiddenColumnsOut` (chip shows the
  count on screen, values don't reach the agent, show-all sends them again)
  and `TestAssistantFollowsTheGridSort` (grid's top 3 go, in order; a
  context taken before a re-sort keeps its order).
- A first version of the sort test re-sorted after Enter to test the clone.
  It passed with the clone removed, because the test harness runs the schema
  lookup synchronously, so it proved nothing. It now checks a context taken
  before the re-sort, and fails without the clone.
- README: the assistant section and the hidden-columns paragraph say the
  assistant gets the grid's view.

`go vet ./...` and `go test ./...` pass.

## Next

Closed: N-036, N-037. Declined: None. Raised: N-037.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
