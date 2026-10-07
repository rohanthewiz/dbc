# Scripts revamp phase 6: the README, the dbc skill, and script descriptions

Session: `f623ed0b-b7ea-49e7-8679-887a43c758ff`

## Ask

`/sl`, then "start phase 6" of `ai_docs/plans/scripts-revamp.md`: rewrite
the README's scripting section around the scripts browser, and update the
dbc skill (`SKILL.md`, `scripting.md`) for the new dir, `dbc scripts` and
`--check`.

## What was already current

Phases 1–2 had already updated the headless parts:

- The README's *Scripts headless* section: by name, `dbc scripts`,
  `--check`.
- The config block and `dbc.example.toml`'s `scripts_dir`.
- The skill's subcommand table and most of `scripting.md`.

What was stale:

- "Scripting in Go" said the browser "will offer" the examples.
- The `dbc web` section's Scripts paragraph described a run-only picker.
- The skill did not mention either browser.

## README

- **"Scripting in Go".**
  - The first paragraphs now say the dir is made on the first save, and
    that saves are revision-checked against edits made elsewhere.
  - New subsections:
    - *The scripts browser*: a sketch, the Scripts / Examples / Trash
      sections, and a table of the TUI and dbc web keys side by side. A
      paragraph says why the keys differ.
    - *Script tabs in dbc web*: Run saves first, explicit save, drafts,
      conflicts, errors as you type, completion, never connected, and
      "Result 1 · 2 · 3".
    - *Editing scripts from the TUI*: `$VISUAL`/`$EDITOR`/`vi`, `code -w`,
      the check on exit. It notes the TUI keeps only the last `s.Show`.
    - *Checking without running*: the old `--check` paragraph, plus yaegi's
      limits.
  - The example script now has a description comment.
  - The samples line says they are the browser's Examples.
- **`dbc web` section.** The Scripts paragraph covers the browser and
  script tabs, with links to the new subsections. The key table has
  `Ctrl+S`.
- **The intro** says "Write a `.go` file — in dbc's own script editor or
  yours" instead of "Drop a `.go` file in the scripts directory".
- ***Scripts headless*** says the built-in examples are not looked up by
  name (N-132). Descriptions are "the comment above `package main`".
- Checked against the code, and fixed in the draft before it landed:
  - Scripts are listed by name (`ListScripts` sorts by name), not newest
    first.
  - The web browser has no right-click on rows. Its ⋯ menu has those
    items.

## Skill (`.claude/skills/dbc`)

- **`SKILL.md`**, under "Go scripts":
  - Where a user's scripts go: write there, `--check`, run by name.
  - The two browsers.
  - The examples are embedded but not found by headless `dbc script
    NAME`, so run them by path from a checkout.
  - Under "Working on dbc itself", that the TUI e2e points `EDITOR` at a
    stand-in.
- **`scripting.md`**:
  - The dir may not exist yet, so `mkdir -p` it.
  - A four-step recipe for writing a user's script: `dbc scripts`, write
    with a valid name and a description comment, `--check` until clean,
    run only with a go-ahead if it writes.
  - A note about conflicts with a tab the user has open.
  - A "scripts browser" section: sections, keys, script tabs, and the e2e
    steps that cover them.

## Fix: descriptions after the build tag (`userdata/scripts.go`)

The skill's recipe was run against a fresh build in a scratch `HOME`. A
script with `//go:build ignore`, a blank line, then `// Count the demo cats
by breed.` above `package main` was listed with an empty description. The
README's own example has that layout.

- **The cause.** `scriptHeaderOf` took `f.Comments[0]`, which is the build
  tag's group. `CommentGroup.Text` drops directives, so that group's text is
  "". The templates and the repo samples put the comment *before* the tag,
  which is why nothing had shown it.
- **The fix.** It now takes the first group above the package clause that
  has text. `DescOf`'s doc says the comment may go before or after the tag.
- **The test.** `TestScriptDesc` gained a tag-first case and a tag-only case
  (description ""). Run against HEAD's `scripts.go`, it fails.

## Plan and list

- `ai_docs/plans/scripts-revamp.md`: the header says "All six phases are
  done", and phase 6 has an Outcome.
- Next list:
  - N-130 is closed (the revamp is finished).
  - N-132 is updated: the headless examples gap is documented, not closed.

## Verification

- `go build ./... && go vet ./... && go test ./...` all pass, and gofmt is
  clean.
- With a fresh build and a scratch `HOME`:
  - `dbc scripts` on a missing dir: "0 script(s) in …", exit 0.
  - After writing `breeds.go`: listed with "Count the demo cats by breed."
    (empty before the fix).
  - `dbc script --check breeds`: no output, exit 0.
  - `dbc script breeds`: the 4-breed table.
  - `dbc script loop_params`: "no script …", exit 2. That is the N-132 gap
    the docs now state.
- Not done: no UI run. The README's sketches and key tables were written
  from the code (`web/static/js/scripts.js`, `tui/scripts.go`, the web's
  keys help) and from the phase 4–5 session docs, not from new screenshots.

## Next

Closed: N-130. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: N-132. Full list: `ai_docs/todo/next-list.md`.
