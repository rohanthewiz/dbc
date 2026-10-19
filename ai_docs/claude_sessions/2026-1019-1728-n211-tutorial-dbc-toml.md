# dbc tutorial: the workspace's dbc.toml is committed (N-211)

Session: `1bfc666c-8aba-46f6-9fc5-955c78d6b175`

## Ask

1. From the cats-todo backlog: next-list item N-211. `dbc tutorial` was
   missing `tutorial/workspace/dbc.toml`, so lesson 0 could not be written
   and `go test ./tutorial` failed on a clean checkout.
2. Then: wrap the session and cut a release.

## Cause

`.gitignore`'s rule `dbc.toml` was meant for the repo root's local config,
but an unanchored gitignore pattern matches at any depth, so it also
matched `tutorial/workspace/dbc.toml`. The tutorial's commit (`d0766e0`)
went in without the file. The working tree here no longer had a copy
either, so the session that wrote it had nothing left to recover.

## Fix (`2843568`)

- `.gitignore`: `dbc.toml` → `/dbc.toml`. Checked with `git check-ignore -v`
  that a root `dbc.toml` is still ignored and the tutorial's is not. No
  other `dbc.toml` exists in the tree, so nothing else is un-ignored.
- `tutorial/workspace/dbc.toml`: extracted from the `dbc.toml` block in
  `docs/scripting-and-etl-tutorial.md`. That block is the authoritative
  source: `TestWorkspaceMatchesDoc` holds every embedded workspace file to
  its doc block byte for byte. The file sets the six `*_dir` settings
  (matching `tutorial.Dirs`), `default_connection = "shop"`, and the two
  connections `CheckWorkspace` requires: `shop` (sqlite, `file:shop.db`)
  and `warehouse` (bytdb, `warehouse.bytdb`).

## Verification

- `go test -count=1 ./tutorial` (CATS_* stripped) passes in the working
  tree, including `TestTutorialEndToEnd`, `TestWorkspaceMatchesDoc`,
  `TestLessonFilesEmbedded` and `TestInitAndWriteFile`.
- The same run in a fresh `git clone` of the commit passes too, which is
  the clean-checkout case the item was about.

## Release

The release is cut after this doc. It is recorded in its own
`Release v…` commit, which is the last commit before the push.

## Next

Closed: N-211. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
