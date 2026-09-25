# A write with RETURNING shows its rows, not rows_affected

Session: 26affab2-33bb-43b6-839a-9d16b97a46b6
Date: 2026-09-24

## Ask

Next-list item N-008, pasted as-is: "`INSERT … RETURNING` (and
`UPDATE`/`DELETE … RETURNING`) shows `rows_affected`, not the returned rows.
`isQuery` (`db/manager.go:297`) keys off the leading verb only, so this hits
Postgres and bytdb alike. The fix is cross-driver: detect a `RETURNING`
clause (via `sqlsplit`, so one in a string literal doesn't count) or fall
back to Query when Exec is wrong."

## Decisions

- **Detect the clause; don't fall back.** The item offered two fixes. The
  fallback doesn't work: drivers never object to `INSERT … RETURNING` run as
  an Exec. They run the write and drop the rows, so there is no failure to
  fall back on, and a retry would run the write a second time. The reasoning
  is in the `isQuery` comment.
- **`sqlsplit.HasKeyword(sql, word)`** is a new, general word finder built
  on the package's own scanner (`skipQuoted`, `skipBlockComment`,
  `dollarTag`…), so it agrees with the splitter and the highlighter about
  where strings and comments end. It ignores the word inside `'…'`, `"…"`,
  `` `…` ``, comments and dollar-quoted bodies. It also ignores a word right
  after `.` (`t.returning`, a qualified name) and right after `:` or `@`
  (named parameters or variables), and steps past `$1` digits so they aren't
  read as part of a word. It only needs a word list, not a grammar.
- **Only DML verbs are checked** (`returningVerbs`: insert, update, delete,
  replace, merge). This covers Postgres, SQLite and bytdb
  INSERT/UPDATE/DELETE, MariaDB/SQLite REPLACE and Postgres 17 MERGE. DDL
  that contains the word unquoted (a rule body, say) stays an Exec.
- **No nesting check.** Postgres only allows data-modifying CTEs at the top
  level, and a `WITH …` statement is already a query by its first word, so a
  `RETURNING` inside a sub-statement doesn't have to be told apart.
- **Stopping early is safe.** Once the `MaxRows` cap is hit, `run` stops
  reading and closes the rows. For DML with RETURNING, Postgres
  (`PORTAL_ONE_RETURNING`) and SQLite (all changes happen on the first step)
  have already finished the write by then, so truncating the display doesn't
  truncate the write.

## Knock-on: Session statefulness

`Session.Run` marked the session stateful when `res.IsExec`. With this
change an `INSERT … RETURNING` comes back as rows, so inside a `BEGIN` it
would no longer count. A dead connection could then be reported as a
harmless retry instead of `SessionLost`. The rule now looks at the statement:
any successful statement that isn't a plain read (`isRead`, the old verb-only
check under a new name) makes the session stateful. The comments on the field
and on `Stateful()` say so.

## Changes

- `sqlsplit/sqlsplit.go`: `HasKeyword`.
- `db/manager.go`: `returningVerbs`, `isRead`, `isQuery` now also counts
  DML + RETURNING, `Session.Run` stateful rule, doc comments.
- Tests:
  - `TestHasKeyword`: 17 cases covering strings, quoted identifiers, line and
    block comments, `$$` and `$q$` bodies, `s.returning`, `:returning`,
    `returning_id`, `''` escapes, `$1`.
  - `TestBytdbReturning`: multi-row INSERT and parameterized UPDATE with
    RETURNING come back as rows; `'returning'` inside a string stays an Exec.
  - `TestSessionStatefulAfterReturning`: SQLite; the rows come back and the
    session counts as stateful.
- `go test ./...` passes; `gofmt -l .` is clean.

## Next

Closed: N-008. Declined: None. Raised: N-039. Deferred: None.
Promoted: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
