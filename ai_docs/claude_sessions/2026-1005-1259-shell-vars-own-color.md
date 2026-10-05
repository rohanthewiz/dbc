# Shell $variables in answers: their own color (N-108)

Session: `976aad03-a801-4b64-adc6-d1f279902c9c`

## Ask

N-108, dropped from the cats-todo backlog: shell `$variables` in the
assistant's code blocks were drawn as Param, the error color (`synParam`,
`.hl-p` → `--err`), the same as SQL bind parameters. Give them their own
color. Then: commit and merge into local main.

## What it does

- Shell variables (`$HOME`, `${X:-1}`, `$?`, `${#arr}`) are drawn in the
  accent at regular weight, in the TUI and on the page. Shell keywords
  stay accent + bold; SQL bind parameters (`$1`, `?`, `:name`) keep the
  error color.

## Design

The item suggested only `codeStyle` and a CSS rule, but both front ends
color by span kind, and the shell lexers emitted `Param` for variables —
the same kind as SQL bind parameters — so a color change there could not
tell them apart. Shell variables got a kind of their own instead:
`codehl.Var` (Go), `"v"` / `.hl-v` (`hl.js`). The comment on `Var`
records why it was split from `Param`: a shell block has a `$var` on most
lines, and a page of error red read as a page of mistakes.

The color is the accent without bold: the palette has only ten colors,
none free; the accent puts variables with the shell's names (keywords),
and the missing weight keeps them apart from keywords.

## Files

`codehl/codehl.go` (`Var`), `codehl/shell.go` (emit `Var`),
`codehl/codehl_test.go`, `web/static/js/hl.js` (shell emits `"v"`),
`web/static/css/app.css` (`.cblock .hl-v`), `tui/theme.go` (`synVar`),
`tui/chat.go` (`codeStyle`), `web/e2e/web_test.go`.

## Verification

- `TestLex` shell cases now expect `V[$HOME]`, `V[${X:-1}]`, `V[$?]`,
  `V[${#arr}]`.
- e2e `codeHighlight` also draws an `sh` block: its `$HOME` is `.hl-v` in
  `--accent`, and the block has no `.hl-p`.
- `go test ./...` and `go vet` pass, on the branch and on the merged main.
- e2e (`DBC_E2E=1`) passed on the branch. On the merged main it failed once
  (33s, against the usual 16s — looks like a timeout; the failing step
  was not captured), then passed four runs in a row. Possibly the known
  reload flake N-110; not confirmed.

## Merge

Branch `todo/n-108-shell-variables-in-answers-24dd` (`512f21e`) merged
into local main (`30a0a9b`). Main had moved ahead by two commits (web
grid transpose, N-112); the only conflict was `next-list.md`: N-108 left
Open, N-109 stays in Roadmap where main moved it, and Closed lists N-108
above N-112.

## Next

Closed: N-108. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
