# SQL alias / CTE resolver: go to definition, usages and rename in dbc web

Session: `da3fcb6d-f627-4fc9-81c6-0b2fe77a7ec8`

## Ask

From the cats-todo backlog: "Do we have some kind of LSP for SQL so that I
could lexically rename, go to definition, and usages?" The answer was no:
only completion (`sqlcomplete`) and Monaco's plain-text Ctrl+F2. I proposed
adding a resolver to `sqlcomplete` instead of an outside SQL language server
(`sqls`, `sql-language-server`), because those keep their own connection
and their own idea of the schema. The user said: "start with the resolver
and web rename".

## What it does

In dbc web, on a table's alias or a CTE's name:

- **F12** (or Ctrl/⌘+click) goes to where it is declared.
- **Shift+F12** lists every use.
- **F2** renames it.
- All three are also in Monaco's right-click menu.

All of this works within the statement under the caret. The new name is
quoted as the dialect needs (`"Ord"` on Postgres, `` `order` `` on MySQL);
a name the user typed already quoted is used as written.

Refusals, shown beside the caret:

- a column: nothing is resolved;
- a table referenced without an alias: found, but never renamed, since the
  edit would only stop the query finding the table;
- a name already taken in the same block (aliases) or the same WITH (CTEs).

## Design

`sqlcomplete/resolve.go` reuses completion's scanner (`tokenize`,
`parseRefs`, `parseCTEs`), so the two agree on what a reference and an
alias are. `scope.go` only gained token indexes: `ref.nameTok`,
`ref.aliasTok` and `cte.tok`.

- **Every FROM is read.** `parseScope` skips the bodies of CTEs and derived
  tables, so the resolver visits every FROM / JOIN / UPDATE / INTO / USING /
  TRUNCATE / WITH keyword itself and has `parseRefs` / `parseCTEs` read each
  list.
- **Blocks.** A parenthesis that starts a query (followed by SELECT, WITH or
  VALUES) opens a block; UNION / INTERSECT / EXCEPT cut a block into arms of
  one frame. Aliases belong to a block and CTEs to a frame. A qualifier
  resolves to the innermost enclosing block that declares the name. This
  keeps a subquery's own `o` apart from the outer one, and still sends a
  correlated `o.id` outward.
- **Guards.** The resolver ignores:
  - FROM inside a function's parentheses (`extract(year FROM d)`);
  - `IS DISTINCT FROM`;
  - WITH not followed by `name [(cols)] AS` (`with time zone`,
    `WITH ORDINALITY`).
- **Web side.** `POST /api/v1/ws/:id/symbol` and `…/rename` take and return
  UTF-16 offsets. `utf16Counter` walks the buffer once across a symbol's
  uses, which arrive sorted. `Workspace.Rename` supplies the active
  connection's driver. Neither endpoint waits on a schema load.
- **Monaco side.** `editor.js` `registerSymbols` adds a definition, a
  reference and a rename provider. `resolveRenameLocation` gives the
  refusal reason, and the edits carry `versionId`, so Monaco rejects them
  if the text changed in flight.

Known limits (documented in the README): a parenthesized join's aliases are
not seen outside its parentheses, and a bare alias used as a whole-row value
or as MySQL's `DELETE o FROM` target is left alone.

## Files

- `sqlcomplete/resolve.go` (new), `sqlcomplete/resolve_test.go` (new),
  `sqlcomplete/scope.go`
- `workspace/complete.go` (`Workspace.Rename`)
- `web/symbol.go` (new), `web/symbol_test.go` (new), `web/server.go`
  (routes)
- `web/static/js/editor.js`, `web/static/js/app.js` (F1 key list)
- `web/e2e/web_test.go` (`aliasRename` step)
- `README.md` (new section "Go to definition, usages and rename", plus a
  row in the web key table)

## Verification

- **Resolver tests** (`TestResolveUses`, `TestResolveDefinition`,
  `TestResolveNothing`, `TestRename`, `TestRenameRefused`) cover:
  - shadowed and correlated aliases;
  - UNION arms;
  - recursive and chained CTEs;
  - quoted names and derived tables;
  - UPDATE;
  - strings and comments;
  - function FROMs;
  - per-dialect quoting.

  As a mutation check, removing the function-FROM guard failed its test.
- **HTTP test.** `TestSymbolAndRename` checks UTF-16 offsets past an emoji,
  applies the edits as Monaco does, and checks the 400 refusal.
- **Browser test.** The e2e step `aliasRename` covers:
  - F12 landing on the declaration;
  - F2's box holding the alias;
  - only the outer alias renamed;
  - the renamed query running;
  - F2 on a column showing the reason.

  The first version typed before the rename box had focus; it now waits
  for `document.activeElement`.
- `go vet` and `go test ./...` pass. The full e2e suite (`DBC_E2E=1`) passes;
  the Postgres schema-picker step was skipped (no DSN).

## Next

Closed: None. Declined: None. Raised: N-113, N-114.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
