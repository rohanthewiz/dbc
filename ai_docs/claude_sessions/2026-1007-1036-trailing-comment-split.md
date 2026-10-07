# A trailing comment after `;` no longer joins the next statement

Session: `57b7eee5-c2f7-40a8-956f-5d65e8a5b647`

## Ask

From the user's cats-todo backlog, with a screenshot of the dbc web editor:
"The previous query is being counted with the current query if there is a
comment at the end of the previous query as if the semi-colon of the
previous query is being ignored."

The screenshot's buffer:

```sql
select max(t.edp_last_updated_timestamp) from la_ea_temp.data_timeseriesquarterly t; -- 2026-10-05T15:29:34-05:00
---
DO $$
DECLARE … BEGIN … END $$;
```

The gutter marker ran from line 19 (the select) to line 37 (the end of
the DO block), with the caret at the end of line 19.

## Cause

`sqlsplit.scan` started the next chunk right after each `;`. So the
`-- 2026-10-05…` comment on the select's line became the DO block's
leading comment, which had two effects:

- **Gutter marker.** The DO statement's `Start` was on line 19, so the
  marker (`workspace.StmtRange` → `/api/v1/stmt` in the web, and
  `currentStmtRange` in the TUI) covered both statements' lines.
- **Caret pick.** `IndexAt` matches on the span end, which stopped at the
  `;`. With the caret in the comment, Ctrl+R (`workspace.Pick`) ran the
  DO block, not the select.

The semicolon was never ignored: the statements always ran separately. It
only looked that way. The existing test case `"line comment"` in
`sqlsplit_test.go` encoded the old behavior (`"SELECT 1; -- drop; this\nSELECT 2"`
→ second text `"-- drop; this\nSELECT 2"`).

## Fix (`sqlsplit/sqlsplit.go`)

- `chunk` gained `tail`: where the statement's span ends, as opposed to
  `end`, where its text ends (just past the `;`). A diagram in the
  `chunk` comment shows the two.
- The new `remarkEnd(s, i)` runs from just past the `;` to the end of its
  line, stopping before the newline, so a caret at the start of the next
  line is not in it. It does this only when the rest of the line holds
  only blanks and comments. Otherwise it returns `i`, so code on the same
  line still starts the next statement there, with any comment before
  that code (`SELECT 1; /* x */ SELECT 2` is unchanged).
- A block comment that opens on the line counts even if it closes on a
  later one. The end of the buffer ends the line too, which covers an
  unterminated comment.
- `scan` resumes past the tail, so the next chunk starts on the next
  line. `Stmt.spanEnd` is now `c.tail`, and `IndexAt`'s doc says that
  the rest of a semicolon's line belongs to the statement it ends.
- Comments on their own lines between statements still head the
  statement below (unchanged; see N-140).

The splitter is shared by both UIs' run and marker paths, the headless
runner, `explain`, and `sqlcomplete.stmtWindow`. So this one change
covers all of them. With the caret after `SELECT 1; ` and a newline,
completion now treats the caret as a new statement, through the
existing "just past the terminator" branch.

## Tests

- **`sqlsplit_test.go` → `TestSplit`.** `"line comment"` now expects
  `"SELECT 2"`. New cases:
  - a comment on its own line (still the next statement's);
  - a trailing comment followed by a `---` header;
  - a trailing block comment;
  - a block comment spanning lines;
  - CRLF line endings.
- **`TestIndexAtTrailingComment`.** The screenshot's shape. Checks that
  the DO statement starts at `---`, and which statement the caret picks
  just past the `;`, inside the comment, at the end of that line, at the
  start of the next line, inside the `$$` body, and at the end of the
  buffer.
- **`TestIndexAtSameLine`.** A block comment before code on the same
  line belongs to that code. Blanks after a `;` with nothing else on the
  line stay with the statement before.
- **`workspace_test.go` → `TestPickTrailingComment`.** `Pick` with the
  caret at the end of the commented line runs the select
  (`statement 1/2`). `StmtRange` of the DO block starts on the next line,
  and the range at the comment covers only the select.

The new tests failed before the fix (`second statement starts at 29
("-- 2026-10-0")`). `go vet ./...` and `go test ./...` pass, with
`CATS_*` stripped.

## Checked in the real binary

The check used a fresh build in the scratchpad, `CATS_*` unset, a
scratch `HOME` and cwd, and `dbc web --no-open --secret …`. The
screenshot's buffer was POSTed to `/api/v1/stmt` with a Bearer secret:

| caret | range |
| --- | --- |
| end of line 19, in the comment | lines 19–19, the select only |
| inside the comment | lines 19–19 |
| on the `---` line | lines 20–end, the DO block |
| inside the DO body | lines 20–end |

A headless run used `dbc --demo sqlite -t json` with
`select 1 as a; -- <ts>\n---\nselect 2 as b;`. The envelopes' statements
were `'select 1 as a'` and `'---\nselect 2 as b'`.

## Left for the user

The `---` separator on line 20 is still inside the DO block's marker
(lines 20–37, not 21–37), because comment lines between statements head
the statement below. The marker could start at the first code line
instead. I offered this; it's raised as N-140.

## Next

Closed: N-139 (raised and fixed this session). Declined: None.
Raised: N-140. Deferred: N-138 (moved to Roadmap by the user before the
session; committed with it). Promoted: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
