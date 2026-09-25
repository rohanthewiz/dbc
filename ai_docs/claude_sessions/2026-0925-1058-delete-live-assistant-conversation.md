# Delete the live assistant conversation

Session: 1c5a2f44-8975-4b83-8909-bf91dc7e60c8
Date: 2026-09-25

## Ask

One next-list item, pasted as-is:

- **N-038**: "No way to delete the live assistant conversation. Saved ones
  can be deleted (right-click a row, or `d` twice in Recent conversations),
  but the one on screen is never offered, since its next save would write it
  back. A "Delete this conversation" row in the transcript's menu would
  remove its file and clear the pane without saving."

## Design

- **A menu row, no key.** `openChatMenu` gains **Delete this conversation**
  after Recent conversations…. Same rule as deleting a saved conversation
  from the mouse: the right-click menu is the deliberate second step, so no
  confirmation and no shortcut. Disabled (`why: "no conversation to
  delete"`, logged on click) when the pane has no messages and no archive id.
- **`newChat` split in two.** `newChat` = `chatSave` + new `resetChat`
  (close the agent, clear messages, forget `archiveID`/`archiveStart`,
  reload the recent list, `ensureChat`). Forgetting the archive id is what
  stops any later save — per-answer, ⟲ new, quit — writing the file back.
- **`chatDeleteLive`** (`tui/chatarchive.go`): `userdata.RemoveChat(dir,
  archiveID)` (a no-op for a never-saved conversation), then `resetChat`,
  then logs `deleted the conversation "<title>"`. On a remove error the pane
  is left alone, so the user is never looking at an empty pane while the
  file they think is gone is still on disk.
- **Fresh session, not just an empty list.** Clearing only the messages
  would leave the ACP session remembering the deleted conversation, and a
  follow-up could bring it back in all but name.
- `chatPane.archiveMsgs` factored out of `chatSave` so the title for the log
  line comes from the same conversion as a save.
- Header comment in `chatarchive.go` (flow diagram, "nothing is destroyed"
  paragraph) and `chatDelete`'s doc now point at the live-delete path.

## Tests

`TestAssistantDeletesTheLiveConversation` (`tui/chat_test.go`): with one
seeded saved conversation — the row is disabled on an empty pane; after one
answer, Delete this conversation removes only the live file, clears the pane
and archive id, logs the deletion and no "saved" line, and the empty pane
still offers the seeded one; the next question carries the session preamble
(new session); after quit only the new conversation and the seeded one are
on disk. `go vet ./...` and `go test ./...` pass (CATS_* unset).

## Docs

README "Conversations are kept" mentions Delete this conversation.
Commit `66f6fbc`.

## Next

Closed: N-038. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
