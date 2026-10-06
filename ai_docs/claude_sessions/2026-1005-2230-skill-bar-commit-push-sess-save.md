# skill-bar: add /next-list, /commit, /push and /sess-save

Session: `9ee31f98-8cbf-4749-a835-6d5a7064e8b8`

## Ask

Grow the skill-bar mod (built in `2026-1005-2158-skill-bar-mod`), in turn:

1. "add /next-list to my skill-bar" ... "at the end".
2. "Add the /commit skill to my skill bar, but put it at number 1. Also put
   /push as number 2. If I don't have a push skill then please create one for
   me. Move the other items down."
3. "add /sess-save to the skill bar too but insert it at position 3."
4. "clear is not working. When I press 5, I see no confirmation" (fixed; see
   Bug below).

No dbc code changed this session. Everything lives outside the repo, under
`~/.claude/`.

## Result

```
Skills  1: /commit  2: /push  3: /sess-save  4: /sess-wrap  5: /clear  6: /sess-load  7: /next-list
```

### `~/.claude/mods/skill-bar/`

- `hooks/register.tsx`: `SHORTCUTS` is now seven entries on digits 1-7;
  `/clear` keeps `confirm: true` and moved to `5`. The design comments and
  the at-rest/confirm diagram follow the new keys: Yes is `1`, Cancel reuses
  `/clear`'s own `5` (so a double-tap still cancels), and an empty prompt
  can't start with a digit 1-7 typed (the existing trade-off, now wider).
- `tests/skill-bar.test.ts`: the listing test checks all seven names on
  digits 1-7; the bare-run test presses commit, push, sess-save, sess-wrap,
  sess-load, next-list; the /clear test expects Cancel on `5`.
- `.claude-plugin/plugin.json`: description lists all seven; version
  `0.2.0` → `0.5.1` across the four changes.
- `claude plugin validate` passes (only the "no author" warning);
  `claude plugin test`: 4 pass, 0 fail. CATS_* vars were stripped from the
  env for both runs.

### New user commands in `~/.claude/commands/`

Neither `/commit` nor `/push` existed (only `/commit-no-trailer`, and the
official `commit-commands` plugin is in the marketplace but not installed), so
both were written in the same one-paragraph style as the other commands:

- `commit.md`: inspect `git status` / `git diff HEAD`, stage what belongs to
  the work (skip build output, scratch files, likely secrets, and say what was
  skipped), one commit in the style of the recent `git log`, keep the
  Co-Authored-By trailer, don't push. `$ARGUMENTS` guide the message.
- `push.md`: `git status -sb` first; `git push`, or `git push -u origin
  <branch>` with no upstream; `--follow-tags` when the pushed commits carry
  new tags; never force-push, and stop on a rejection instead of pulling or
  rebasing. `$ARGUMENTS` pass through to `git push`.

Both were picked up as skills in-session without a restart.

## Bug: digit 5 did nothing once the row held seven buttons

All tests passed, yet pressing `5` on an empty prompt showed no `/clear`
confirmation. Cause: the row is ~92 cells wide, and the band only gets
`bodyColumns` (the pane's column less the engine's five `[-]` cells), which a
cats split pane falls short of. The overflowing row squeezed/clipped its
Buttons at the right edge, and the engine arms a bare-digit hotkey only for a
Button drawn whole, so the later digits silently went dead. The test kit
never paints (it checks the tree, not Ink's layout), so it couldn't catch it.

Fix (0.5.1): the shortcut row's Box is now
`flexDirection="row" flexWrap="wrap" columnGap={1}` (keyed `shortcuts`), so a
narrow pane gets a second row instead of clipped buttons; `columnGap` rather
than `gap` keeps the wrapped rows flush. The listing test also asserts the
row's `flexWrap` is `wrap`. Confirmed by the user: `5` now shows the
confirmation (they cancelled rather than clear the session).

Debugging notes: the mod loads via `CLAUDE_CODE_PLUGIN_DIRS` in
`~/.claude/settings.json`, which an interactive session watches and
hot-reloads, so a stale load was ruled out first.

## Caveat

Bar presses run commands bare. `/sess-save` builds its filename from
`$ARGUMENTS`, so a press from the bar gives `<YYYY-MMDD-HHMM>-.md`; type
`/sess-save <name>` when the name matters.

## Next

Closed: None. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
