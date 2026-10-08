# TUI assistant: ask about the script that just ran

Session: `976ee921-3a8d-46c4-8613-5ff4c7d08a2f`

## Ask

1. From the cats-todo backlog: N-137. dbc web's script tabs send a script's
   context to the assistant (N-134: the source as Go, the connections, the
   run's error or result, the sdb API summary). In the TUI, scripts are edited
   in `$EDITOR`, so the assistant got none of that. After a TUI script run it
   should attach `ScriptChatContext` for that script.
2. `/sess-wrap`.

## Finding

The note's premise was off. It said the TUI assistant saw "only a script
run's result", but it saw nothing of the run. After a script run `lastStmt`
is "" (N-134), so `ChatContext` takes the editor's SQL as "this query". That
statement is not the last run's, so no error goes, and the script's result tab
has no statement (`resultTab.stmt` ""), so `attachTabLocked` matches nothing.
A throwaway probe confirmed it: the query was the editor's SELECT, with no
error, no columns and no rows.

## What changed

**workspace**
- `RunDone.ScriptPath`: the script file as `RunScript` was given it. `Tag`
  names only the base name.
- `resultSet.lastScriptPath` is set and cleared alongside `lastScript`, in
  `landRun` (run.go) and the explain landing (explain.go).
- `Workspace.LastScript()` returns the path for the active connection, or ""
  once a statement or explain has run there since.

**tui**
- `editor.Stamp()` / `editor.Touched()` (editor.go) record the edit version
  and caret at one moment. `Touched` is true after an edit or a caret move,
  and also when the editor was never stamped.
- `runScript` (run.go) stamps the editor, but only when `ws.RunScript`
  started the run: a refused one leaves things as they were.
- `chatContext` (chat.go): when `ws.LastScript() != ""` and the editor is not
  `Touched`, it calls `ws.ScriptChatContext(question, base, source, views…)`
  instead of `ChatContext`. A diagram in the comment shows the decision.
  - The yield to the editor is a design choice. Without it, a user who ran a
    script and then typed a query to ask about would get the script answered.
    An edit or caret move hands the subject back to the editor, and so does a
    statement run (the workspace forgets the script then). The chip always
    shows which one goes.
- `chatPane.scriptSource(path)` caches the file's text, keyed by path, size
  and mtime. The chip builds the context every frame, so this costs a stat
  per frame rather than a read, and it still picks up an edit made outside
  dbc. A file that can't be read gives "": the question still carries the
  run's error or result under the script's name.
- `chatPane.apiSent` + `withAPI`: the sdb API summary goes on the
  conversation's first script question only (web's `withAPILocked` rule).
  `finishSubmit` marks it sent, and `resetChat` clears it along with `first`.
  The chip's forecast goes through the same `chatContext`, so it says
  "sdb API" exactly when the question carries it.
- The empty pane's hint gains a line about script runs. Go blocks in answers
  already get only ⧉ copy in the TUI (`isSQLLang`), so that needed no change.

**Docs**: the README's "Editing scripts from the TUI" section gains a
paragraph on what goes after a script run and when it stops.

## Tests

`tui/chat_test.go`:
- `TestAssistantAsksAboutTheScriptThatRan`: a failing script. It checks the
  chip's forecast, then the prompt: the Go fence and name, `demo-sqlite
  (sqlite)`, the error and the sdb API, with no editor SQL. The second
  question carries no API. After a caret move, the editor's statement goes.
- `TestAssistantScriptResultThenAStatementRun`: the shown result goes with
  ai_rows. Ctrl+R clears `LastScript`, and the script no longer goes.
- `TestAssistantRereadsAnEditedScript`: an on-disk edit is picked up, and a
  removed file still goes by name with an empty source.

All three fail with the script branch disabled. The full `go test ./...`
passes, with the `CATS_*` variables stripped.

## Next

Closed: N-137. Declined: None. Raised: None. Deferred: None. Promoted: None.
Moved: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
