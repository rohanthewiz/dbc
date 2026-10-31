# Format: keep (or lower) the keywords' case (N-226)

Session: `2d09f67e-3d3d-4727-9af7-877cc2ea1b82`

## Ask

Next-list item N-226, pasted from the cats-todo backlog. Format
(`sqlsplit.Format`, TUI ⌥⇧F, web Shift+Alt+F) always upper-cased keywords.
Offer keeping their case — a config key or a second menu row — for whoever
wants lower-case SQL kept lower-case.

Then `/sw`.

## Decision: a config key, three values

```toml
format_keywords = "keep"   # "upper" (default) | "lower" | "keep"
```

- **Config key, not a menu row.** The case is a house style: someone who
  writes lower-case SQL wants it kept every time, not picked per format. A
  key also reaches both UIs at once with no UI work.
- **`lower` as well as `keep`.** One extra branch. It covers pasting an
  upper-case query from a log into a lower-case codebase, and it is the
  usual trio in other formatters (upper / lower / preserve).
- **Default unchanged** (`upper`), so nobody's output moves.
- **A typo warns, doesn't fail** (`"preserve"` → warning naming the three,
  formats upper) — `plan_theme`'s rule for a cosmetic key.

## How

```
fmtLex(sql, d)            tokens; keywords keep their source text,
     │                    dotted-name keywords demoted to words
     ▼
KeywordCase.apply(toks)   upper / lower (= t.low) / keep (no-op)
     │
     ▼
fmtParse → fmtWriter      layout reads kinds and t.low only, never the
                          written text, so the breaks are identical
```

- `sqlsplit/format.go`:
  - `FormatOptions{Keywords KeywordCase}`; each field's zero value is the
    old behavior, so `FormatOptions{}` is `Format`'s exact output.
  - `KeywordCase` (`UpperKeywords` = zero, `LowerKeywords`, `KeepKeywords`),
    `ParseKeywordCase(s) (k, ok)` (trimmed, any case, `""` = upper; anything
    else upper with ok false), `String()`. One vocabulary, used by config.
  - `Dialect.FormatWith` / `FormatSpanWith`. `Format` / `FormatSpan` now call
    them with zero options, and the package-level functions are unchanged.
  - The `ToUpper` moved out of `fmtLex` into `KeywordCase.apply`, which runs
    before the tree is built. `stripLead` (the other `fmtLex` caller) reads
    only kinds, lower-case forms and offsets, so it doesn't care. A
    `KeywordCase` outside the three formats upper, the same as `String`
    names it.
  - The writer never synthesizes keyword text (checked: every byte written
    comes from a token), and keywords are ASCII, so their width doesn't
    change with case.
- `config/config.go`: `FormatKeywords string` (`format_keywords`), default
  `"upper"`, normalized by `checkFormatKeywords` after `checkPlanTheme`.
  `Config.FormatOptions()` is nil-safe, and a Config Load never saw gets the
  defaults (the test harnesses build one by hand). `config` now imports
  `sqlsplit`: it is stdlib-only, so it can't close a cycle, and the names
  stay in one place.
- `tui/format.go`: `d.FormatSpanWith(sel, m.cfg.FormatOptions())`.
- `web/grid.go handleFormat`: the same with `s.cfg`. Read server side, so the
  page (Monaco's format provider) needed no change and the two UIs can't
  disagree.
- README "Formatting a query" and `dbc.example.toml` document the key.

## Tests

- `sqlsplit`: `TestFormatKeywordCase` (all three on
  `Select a, T.Key, 'Select' From t -- From here\nwhere b = 1 And c`: only
  keywords move, and the string, comment and `T.Key` keep their case;
  `Format` == `FormatWith(zero)`; `KeywordCase(9)` is upper; `FormatSpanWith`
  keeps the blanks; a laid-out lower-case buffer is unchanged under lower and
  keep). `TestParseKeywordCase`. `TestFormatKeepsTokensAndSettles` now runs
  each input in lower and keep too (same tokens, settled), and under keep
  checks that no token's text changed at all (`rawTokenTexts`).
- `config`: `TestLoadFormatKeywords` (absent, `"Lower"`, `"keep"`, the
  `"preserve"` typo warns; `FormatOptions()` for each and for a nil Config).
- `tui`: `TestFormatKeywordCaseFromConfig` (keep and lower through ⌥⇧F; a
  second press logs "already formatted").
- `web`: `TestFormat` gains a keep-configured server.
- `go build ./...`, `go vet`, and full `go test ./...` (CATS_* stripped)
  pass.
- Real binary: a fresh build of `dbc web` on a scratch config with
  `format_keywords = "keep"` (isolated `HOME`, scratch SQLite), POST
  `/api/v1/format` →
  `select a, b\nFrom t\nwhere x = 1 And y in (select id from u)`. The
  first port tried was already taken by some other process (left alone).
  The test server was stopped by its own PID afterwards.

## Notes

- `sqlsplit/folds.go` shows up in `gofmt -l`. It was already like that and
  is untouched here.

## Commits

- The work, this doc and the next-list edit, together.

## Next

Closed: N-226. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
