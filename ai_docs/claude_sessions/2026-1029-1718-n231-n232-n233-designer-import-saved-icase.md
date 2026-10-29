# Query designer: open SQL as a design, saved designs, ignore case (N-231, N-232, N-233)

Session: `72a6141d-4326-4e53-9491-33117d9a0a1d`

## Ask

"Work next-list items N-231, N-232, and N-233": the three designer
follow-ups the query-designer session raised.

No choices were asked up front. Decisions made unasked, and reported at the
end:
- **Saved designs live in `web.bytdb`** (a `designs` table), per the
  standing "bytdb for persistence" preference. They are not files beside
  pipelines; the designer is dbc web only.
- **A named design saves as you go**, as a console does, rather than on
  Ctrl+S. Each save is revision-checked.
- **Closing a design kept only in its tab now asks.**
- **The import is faithful or it refuses.** It never approximates silently;
  each reading in the designer's own terms is logged as a note.
- **Keys:** `Alt+Shift+D` for the saved designs. "◇ Open as a design" is in
  the editor's right-click menu only.

Then `/sw`.

## N-233: ignore case on the LIKE filters

- **`Filter.IgnoreCase`** (`designer/model.go`, json `ignoreCase`) applies
  to contains, starts with and ends with (`likeOp`, `folds`).
- **The generator** (`sql.go like`): Postgres and bytdb write `x ILIKE …`
  (a new `dialect.ilike`); MySQL and SQLite write
  `lower(x) LIKE lower('…')`. The engine lowers the pattern, so its
  `lower()` and its LIKE agree on what a letter is. `ESCAPE` is kept.
- **The story** says ", ignoring case". On SQLite it says ", ignoring the
  case of A–Z (SQLite folds no other letters)".
- **The rules** (`lint.go`):
  - `filterCols` no longer counts a folded starts-with as sargable on a
    b-tree.
  - `ruleContains` now has two rules. `leading-wildcard` covers contains
    and ends with, as before. The new `ignore-case-like` covers a folded
    starts with.
  - Fixes on Postgres: a trigram index. On MySQL: an index on
    `lower(x)` (`((lower(x)))`), and a note that a `_ci` column already
    ignores case. Everywhere: "Match case instead", plus "Use starts with
    instead" for contains.
  - Quiet on evidence. `Catalog.trigram` takes a listed GIN or GiST index
    on the column for a trigram one, which now quiets `leading-wildcard`
    too (it used to warn beside an existing trigram index).
    `Catalog.exprIndex` is an index on an expression the listing cannot
    name; on MySQL it may be `lower(x)`, so it keeps the rule quiet.
- **The page** (`designer.js filterRow`): an "ignore case" checkbox on the
  three operators. Changing to another operator drops the flag.

## N-232: saved designs

- **The store** (`web/store.go`):
  - A `designs` table: name, conn, design, sql, updated.
  - `SavedDesign`, whose `Rev()` is a hash of conn, design and sql.
  - `Designs`, `SaveDesign(old, rev, d)` and `DeleteDesign`.
  - `SaveDesign` checks and writes in one transaction (one lock in
    memory). It covers create, save, rename, a taken name
    (`errDesignExists`), a stale revision (`errDesignChanged`) and a
    deleted one (`errNoDesign`).
  - Tabs gain `design_name` and `design_rev` (`Tab.DesignName`,
    `DesignRev`), added with `ADD COLUMN IF NOT EXISTS`.
- **The routes** (`web/designs.go`):
  - `GET /api/v1/designs` lists each design with its tables and rev.
  - `POST /api/v1/designs` saves, with `{old, rev, name, conn, design,
    sql}`. A conflict is a 409, a missing design a 404.
  - `POST /api/v1/designs/delete`.
  - Every change is broadcast as "designs".
  - The name rides in the body because rweb hands path params over
    undecoded, and a design name is a free title.
  - `saveTab` checks a tab's design name.
- **The page:**
  - The designer's bar shows "✓ saved" or "in this tab only", with
    **⤓ Save as…** and **⤒ Open…**.
  - The saved designs dialog (`designer.js pickSaved`): filter, SQL
    preview, Enter opens, `Alt+D` deletes after asking.
  - `app.js`:
    - `saveDesignAs` names the tab and ties it to the design. On a named
      tab it is Save As.
    - `saveNamed` sends one save at a time per tab, from the last revision.
    - `designNameLost` runs on a 409 or 404: the tab keeps its copy,
      unnamed, and the log says so.
    - `openSavedDesign` focuses the tab that has it, or opens one on its
      connection.
    - `renameDesign`: the tab rename renames the design.
    - `closeUnsavedDesign` asks: Save as… and close, Close without saving,
      or Keep it open.
    - `reallyClose` flushes a named design's pending save.
  - The design save timer is now per tab, so a second tab's edit no
    longer cancels the first one's save.
  - The tab menu has "Open a saved design…" and "Save the design as…".
    `Alt+Shift+D` works page-wide and in Monaco. KEYS rows added.

## N-231: SQL into a design

- **`designer.Import(sql, cat)`** reads one SELECT as a Design, with
  notes. A failure is an `*ImportError` with the reason, offset and line.
- **`designer/importparse.go`** holds the lexer and parser.
  - A dialect-aware lexer:
    - Postgres: `''` only, dollar quotes, nested block comments.
    - MySQL: backslash escapes, `"…"` strings, `#` comments.
    - SQLite: `''` only.
  - A recursive-descent parse into a small AST covering SELECT, FROM and
    joins, WHERE, GROUP BY, HAVING, ORDER BY, LIMIT/OFFSET/FETCH, and
    expressions down to calls, casts, typed literals and intervals.
  - Everything outside that grammar is refused here, with the reason.
- **`designer/import.go`** is the binder.
  - Names: tables resolved exactly, then case-blind (the default schema
    wins an unqualified tie). Columns innermost scope first, with USING
    columns read from their earlier table.
  - Joins: ON `col = col` pairs become a join per earlier table. Other ON
    conditions become InJoin filters on the joined table. RIGHT JOIN
    becomes a left join from the kept table. USING becomes pairs.
  - The select list: columns, aggregates, `count(1)`, `*` and `t.*`
    (expanded unless they cover every table), and aliases.
  - Filters: a column against values, OR groups, BETWEEN, IN, IS NULL, and
    `IS TRUE` or a bare boolean column. LIKE/ILIKE/`lower()` map to
    contains, starts or ends, with the default escape per engine.
  - Read in the designer's terms, each with a note:
    - EXISTS / NOT EXISTS over one table → has any / has none.
    - `x IN (SELECT …)` → has any.
    - `a.x = b.y` in WHERE → an inner join. Refused on an outer-joined
      table.
    - Adjacent `>= a AND < b` on dates → range, year or day.
    - The engines' "last N days" spellings → in the last N days.
    - `(x op v OR x IS NULL)` → "or empty".
    - GROUP BY with no aggregate → DISTINCT.
    - LIMIT -1 and 2^64-1 → no limit.
  - GROUP BY must be shown columns. HAVING on a plain column must be on a
    grouped one.
  - Joins get their foreign key's name (`markFKs`). Tables are laid out by
    depth, stacked by card height.
  - A RIGHT or FULL JOIN is refused when `plan`'s breadth-first FROM order
    differs from the statement's: moving an inner join across one changes
    the rows. So is a comma join beside one, since a comma binds looser
    than JOIN.
- **The route** `POST …/design/import` takes `{buffer, caret, selection}`,
  picks the statement with `workspace.Pick` (so it is the one Run would
  run), imports it, then analyzes it with the indexes of its tables.
- **The page:**
  - "◇ Open as a design" in the editor menu (`designFromQuery`) opens a
    new design tab beside the query tab. A refusal goes to the status bar
    and the log.
  - `newDesign` takes a design, a connection and SQL.
  - Group options include letters past C.
  - A design shown with no view yet (imported, reopened, after a reload)
    is fitted.
  - `designer.js filled()` gives a server-made design its empty lists
    (Go writes them as null).

## Found on the way

- **sqlsplit misreads the designer's own Postgres SQL.** Its scanner
  honors `\'` in every dialect, so `ESCAPE '\'` runs on past its quote. A
  probe split `… ESCAPE '\'\n AND x = 'a';\nSELECT 2` as one statement, so
  Ctrl+Enter beside another statement would run both. This predates the
  session. The import uses its own lexer for this reason. Raised as N-235,
  with the cheap fix: `ESCAPE '!'` on Postgres and SQLite.
- **Postgres prefix LIKE needs `text_pattern_ops`** (or the C collation)
  for a b-tree to serve it. The designer counts a plain index and suggests
  one. Raised as N-236.
- **The summarize join cannot be read back.** It is a derived table, and
  the import refuses those. Raised as N-237.
- **The `shop` fixture's `"User Name text"`** is a column `User` of type
  `Name text`. The round-trip test uses `User`.
- **e2e:**
  - The bar redraws on every analysis, so its buttons are clicked by
    script (a rod handle detaches and `MustClick` waits forever).
  - The stage's `set` fires onChange, which records a view, so "no view
    yet" must be read before it.
  - One full-suite run failed in "go to and rename an alias and a column"
    on Monaco's internal `Canceled` rejection. The rerun passed: a flake,
    not this change.

## Tests

- **`designer/designer_test.go` `TestIgnoreCase`:**
  - The SQL on Postgres, bytdb, MySQL and SQLite, and the SQLite story.
  - `ignore-case-like` and its fixes. Quiet beside a GIN index and
    beside a MySQL expression index.
- **`designer/import_test.go`:**
  - `TestImportRoundTrip`: seven design shapes, written by the generator
    and read back to the identical SQL on all four dialects. The shapes
    cover every filter kind, a left join with an InJoin filter, has
    any/none, a self-join, grouping with HAVING and sorts, a quoted
    reserved name, and `a.*, b.*`.
  - `TestImportHandWritten`: fan-out found, comma joins, IN-subquery,
    anti-join, RIGHT JOIN, USING-free multi-joins, MySQL `LIMIT m, n` and
    `lower()`, Postgres `ILIKE` and `now() - interval`, `count(1)`,
    ordinals, GROUP BY as DISTINCT, SQLite `date('now', …)` and
    `LIMIT -1`.
  - `TestImportRefuses`: 22 refusals, and the line of one.
- **`web/designs_test.go`:** `TestSaveDesignStore` (a file store reopened,
  and the memory store) and `TestDesignsAPI`.
- **`web/design_test.go` `TestDesignImportRoute`** (SQLite): the statement
  under the caret is imported, and its SQL runs to "Ana|120 Bo|20". A
  refusal with its line, and the empty buffer.
- **`workspace/live_design_test.go` `TestLiveDesigner`**, on Postgres 17
  and MySQL 8.4:
  - Ends-with and escaped contains, ignoring case, return the same rows on
    both.
  - Six hand-written queries return the same rows as their imported
    designs.
- **Web e2e "query designer"**, new parts 6 and 7:
  - 6: closing an unsaved design asks; Save as; a named design closes
    silently; `Alt+Shift+D` reopens it.
  - 7: "Open as a design", ignore case on its LIKE, and a CTE refused.
- **Results:**
  - `go test ./...` passed, with `CATS_*` stripped.
  - The live suites for db, workspace, web and designer passed on both
    engines.
  - The full web e2e passed on its second run (66s); the first hit the
    Monaco flake above.
  - gofmt and go vet are clean.

## Commits

- The work, this doc and the next-list edits, together.

## Next

Closed: N-231, N-232, N-233. Declined: None. Raised: N-235, N-236, N-237.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
