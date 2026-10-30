# Designer LIKE escapes that sqlsplit reads right (N-235)

Session: `15ff10e0-8817-427f-bfc1-38a1d064c59d`

## Ask

Next-list item N-235, pasted from the cats-todo backlog. sqlsplit's scanner
honors `\'` in every dialect, MySQL's rule. The designer wrote `ESCAPE '\'`
for any LIKE whose value held `%`, `_` or `\`. In a query tab beside another
statement, Split then saw one statement where there were two: Ctrl+Enter ran
both, and the editor colored what followed as a string. The item proposed
`ESCAPE '!'` on Postgres and SQLite, with bytdb and MySQL keeping theirs.

Then `/sw`.

## What changed

- **`designer/sql.go`**: a new `likeEscape` picks the escape for each
  engine, and `like` uses it. The trigger is now `%`, `_` or the escape
  character itself; the escaping set is the same three.

  | engine          | escape | clause        | why                                            |
  | --------------- | ------ | ------------- | ---------------------------------------------- |
  | postgres, bytdb | `\`    | none          | backslash is the default (bytdb's only one)    |
  | mysql           | `\`    | `ESCAPE '\\'` | unchanged; scans right                         |
  | sqlite          | `!`    | `ESCAPE '!'`  | no default escape; `\` is plain text there     |

- **Two departures from the item, decided unasked and reported at the end:**
  - **No `ESCAPE '!'` on Postgres.** With `!`, a value's own backslashes
    would be written single. An "ends with `a\`" filter would then write
    `'%a\'` and misread again. Escaping with backslash always pairs them
    (`\\`, `\%`, `\_`), and Postgres needs no clause for it.
  - **bytdb could not keep `ESCAPE '\'`.** The scanner knows no dialect, so
    it misreads bytdb's clause exactly as it misreads Postgres's: the new
    test failed there against the old generator. bytdb's source
    (`sql/expr.go likeRegex`, `docs/gotchas.md`) confirms backslash is its
    default and only escape, so the clause is simply dropped.
- **Comments**: `importparse.go` notes the generator no longer writes
  `ESCAPE '\'`; `import.go`'s `like` names bytdb among the backslash-default
  engines. The import itself needed no change: it already read any
  one-character ESCAPE, and a missing one as `\` everywhere but SQLite.

## Tests

- **`TestLikeEscapeScans`** (new): each engine's exact LIKE for a value with
  `!`, `%`, `_` and `\`. Each statement must still split from a following
  `SELECT 2`. Against the old `sql.go` it fails on Postgres, bytdb and
  SQLite; MySQL passed both before and after. It also checks SQLite's two
  edge cases: a value with `!` gets `!!` and the clause, and a value with
  only `\` is written plain, with no clause.
- **`TestLikeEscapeRunsOnSQLite`** (new): modernc SQLite in memory runs the
  `!` patterns for contains, starts with and ends with, with and without
  ignore case. The rows must equal what Go's `strings.Contains`,
  `HasPrefix` and `HasSuffix` pick, over values holding `%`, `_`, `!`, `!!`
  and `\`. The designer's tests now import `modernc.org/sqlite`, test-only.
- **`TestImportRoundTrip`**: an ends-with filter on `x!\y` added, read back
  on all four dialects.
- **`TestLiveDesigner`**: a backslash value (`p\q`, set on order 12 and
  reset to NULL after) is matched as itself on Postgres 17 and MySQL 8.4.
  It confirms that Postgres reads the pattern's `\\` with no clause.
- **Updated expectations** in `TestFiltersAreWrittenSafely` and
  `TestIgnoreCase`: the Postgres LIKE and ILIKE lose their `ESCAPE '\'`.
- **Real binary**: `dbc --driver bytdb --dsn … -f` ran the generated
  `LIKE '%5!0\%\_\\%'` beside a `SELECT 2`. It matched only the literal row,
  and the next statement ran on its own.
- `go test ./...` passes, with `CATS_*` stripped. The live run used the
  docker recipe; both containers were stopped after.

## Left open

A string literal ending in a backslash still misreads on Postgres, SQLite
and bytdb. The designer writes one for an equality value ending in `\` and,
on SQLite, for an ends-with value ending in one; typed SQL can hold one
anywhere. Raised as N-238: a dialect-aware split, with a driver passed by
Split's and Lex's many callers.

The user had already moved N-230 from Open to Roadmap, uncommitted, before
this session; that move is committed here.

## Commits

- The work, this doc and the next-list edits, together.

## Next

Closed: N-235. Declined: None. Raised: N-238.
Deferred: N-230 (moved by the user before this session). Promoted: None.
Moved: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
