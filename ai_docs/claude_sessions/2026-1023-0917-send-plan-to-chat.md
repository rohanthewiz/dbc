# Send an Explain plan to the AI chat (TUI and dbc web)

Session: `a065be83-1382-4376-a04a-98d1e7c5d5ce`

## Ask

From the cats-todo backlog: "Give me an option to send the Explain results
to the AI Chat".

## What was there

- Both UIs already had "✦ Ask the assistant about this plan". In the TUI it
  is on the plan menu; in dbc web it is the plan header's `✦ Ask`. It only
  drafted a question.
- The plan itself went with a question only when the statement under the
  caret normalized to the plan's statement (`workspace.planForChatLocked`).
- So once the caret moved (to write the fix, or into another statement),
  asking "about this plan" sent no plan at all.

## Design

| Question | Decision |
|---|---|
| One-shot attach, or a sticky share | Sticky, mirroring `ShareResultTab`: the chip forecasts it, and no "consume on submit" hook is needed in each UI. ChatContext runs every frame, so it cannot consume anything |
| What is shared | The connection's plan SLOT (`resultSet.planShared`), not one plan. Explaining again sends the new plan, so "is it better now?" is about the plan on screen |
| Needs `ai_rows`? | No. A plan is the database's strategy, not its rows, and already goes on every connection |
| Framing | `ai.Context.PlanShared` / `PlanFrom`. A shared plan of another statement is introduced with that statement, so "Its query plan" is not read as the plan of the SQL above it. With no SQL in question, it says "of this statement" |
| Script / pipeline tabs | A shared plan goes from them too, always with its statement. An unshared plan still never does |
| Plan detected in a typed EXPLAIN result | `resultSet.planStmt` is recorded when a plan lands (the explained statement, or the run's statement). It is a fallback for when `Plan.Statement` is "". In practice SQLite's detected plans already carry a Statement |
| Starting it | TUI: the `✦ Chat` header chip, the `A` key, and the plan menu's ✦ rows (the step and plan asks now share as well). Web: `✦ Send to chat` (replaces `✦ Ask`), `A`, and a step's ask |
| Stopping it | TUI: the plan menu row "✦ Stop sending the plan with questions", shown only while shared. Web: a `✕ Stop sending` header button, hidden unless shared |
| Marks | The Plan tab is labelled `✦` in both UIs; the TUI chip uses the hot style; the web button gets `.on`; the chat chip says "shared plan" |
| Keys | `A` because `s`/`S` already share the result tab from the plan pane, and `a` is analyze |

## Changes

- `ai/prompt.go`: `PlanShared`, `PlanFrom`, three framings, and a note that
  says "shared plan".
- `workspace`:
  - `SharePlan(bool)` refuses when there is no plan; unsharing always works.
  - `PlanShared()`.
  - `planForChatLocked(ctx *ai.Context, query)` now fills ctx: the
    implicit rule, or a shared plan with `PlanFrom` when the statements
    differ.
  - `ChatContext`, `ScriptChatContext` and `PipelineChatContext` call it.
  - `planStmt` is set in `landExplain` and `landRowsLocked`.
- `tui`:
  - `chipChat` and `planView.shared`, set each frame in layout before the
    draw.
  - `sendPlanToChat` / `unsharePlan` and the shared `askPlanQuestion`.
  - `A` in `planKey`; the plan tab label gets `✦`; a help row.
- `web`:
  - `POST /api/v1/ws/:id/plan/share {share}` → `{shared}`, which logs only
    on a change.
  - `planOut.Shared`.
  - `planview.js`: `sendPlan`, `unsharePlan`, `markShared`, the `A` key,
    and refreshing the chat chip after a stop.
  - `app.css`: `.rtab.shared::after` and `#plan .iconbtn.on`.
- README:
  - The plan paragraph now points to sending.
  - A new keys-table row for `A`.
  - A new **Sending a plan** paragraph under the AI assistant section.
  - The web assistant paragraph.

## Tests

- `ai.TestBuildSharedPlan` covers the three framings and the note.
- `workspace.TestSharedPlanGoesWithEveryQuestion`:
  - refused with no plan;
  - the caret rule while unshared;
  - shared, with `PlanFrom`, with and without the caret on the statement;
  - from a script tab;
  - the slot follows a re-explain;
  - unsharing restores the caret rule.
- `workspace.TestTypedExplainPlanGoesToTheChat`.
- `tui.TestSendPlanToChat`:
  - the chip click shares the plan and drafts the question, unsent;
  - the prompt carries "of a different statement" and the explained SQL;
  - the menu row stops it;
  - `A` sends it again.
- `web.TestPlanShare`: the 400 with no plan, `GET …/plan` shared, and the
  forecast with and without the share.
- Web e2e step "send the plan to the assistant":
  - The first version really opened the chat. That started the agent and
    left a transcript, which broke "fold the recent conversations": its
    recent list was gone. The baseline suite passed without the step.
  - The step now stands in for `dbc.cmd.askAbout` and records the question.
- Results:
  - `go test ./...` is green.
  - Full web e2e (`DBC_E2E=1`) is green, about 60 s; the Postgres steps
    were skipped (no DSN).
  - TUI e2e (`DBC_TUI_E2E=1`) is green.

## Notes

- With the chat open, the TUI plan pane is narrow enough that `✦ Chat` gives
  way like the other right-hand chips. `A` and the menu still reach it. The
  TUI test clicks the chip before the chat opens for this reason.

## Next

Closed: None. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
