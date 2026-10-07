# Release v0.9.0 (tag only); the statement marker skips header comments

Session: `6bfa4cee-88b7-4430-afc3-cb1ec0b55388`

## Asks

1. "Tag a minor rel for dbc, but don't run the CI pipeline this time."
2. "commit the next-list changes" (the user's own edit: a new Validate
   section).
3. N-140 from the cats-todo backlog: a comment on its own line between two
   statements is inside the next statement's gutter marker; start the
   marker at the statement's first code line instead.
4. "/sw" (sess-wrap). The user confirmed they had moved N-142 and N-143
   to Roadmap themselves.

## Release v0.9.0, tag only

The patch recipe from memory, applied to a minor bump:

- `version/version.go` and `cats-plugin.toml` 0.8.2 → 0.9.0, checked by
  `go test -run TestVersion .` with `CATS_*` stripped.
- `772beba Release v0.9.0 [skip ci]`, an annotated tag `v0.9.0`
  ("dbc v0.9.0"), and only `main` and the tag pushed. `release` was not
  pushed, so neither release.yml nor goreleaser ran, and there are no
  archives or GitHub Release for v0.9.0. `[skip ci]` on the pushed head
  kept test.yml off.
- The commit body summarizes what has landed since v0.8.2: the scripts
  revamp, headless `dbc copy` and the ETL layer, per-connection result
  tabs and logs, and Postgres NOTICE/DETAIL/HINT/CONTEXT.
- The release-recipe memory now notes that a minor bump can be tag-only
  too, when the user says so.

## Next-list Validate section

`1f8c1c5 Next-list: a Validate section for test-only items` commits the
user's edit: Validate sits below Open for items whose remaining work is
only a check, with its rules added to Conventions. N-064, N-087, N-104,
N-106, N-115, N-145 and N-150 moved there from Open.

## N-140: the marker starts at the code

### Cause

After N-139, a semicolon's own line ends the statement before it. The next
chunk starts on the following line, so a `---` separator between two
statements heads the statement below. `Stmt.Start` skips only blanks, and
`workspace.StmtRange` returned `[Start, End]`, so the marker took in the
comment lines. The TUI (`currentStmtRange` → `editor.Draw`) and dbc web
(`/api/v1/stmt` → `editor.js mark()`) both draw from `StmtRange`.

### Fix

- **`sqlsplit/sqlsplit.go`.** `Stmt` gains `CodeStart`, the offset of the
  statement's first byte outside blanks and comments. It equals `Start`
  when nothing heads the statement. A diagram on `Stmt` shows `Start` and
  `CodeStart`. The new `codeAt` skips whitespace, line comments and
  (nested) block comments. Unlike `keywordAt`, it stops at `(`, which is
  code (`(SELECT 2) UNION (SELECT 3)`). `Split` calls it from `Start`,
  which sits on a token boundary because only blanks were skipped to reach
  it, so a comment's inside cannot pass for code.
- **`workspace/pick.go`.** `StmtRange` returns `[CodeStart, End]`.

### Decisions

- **Only the marker moves; the text stays.** N-140 mentioned the text
  ("and its text") but proposed only the marker. `Text` still carries the
  header comment, so what runs, what history and headless envelopes hold,
  and `explain`'s input are all unchanged. I offered to strip it from the
  text too.
- **The caret pick is unchanged.** A caret on a header comment still
  picks the statement below (`IndexAt` matches on the span). The marker
  then starts on the line under the caret, as it already did for a caret
  on a blank line between statements. The marker still shows what Ctrl+R
  runs.

### Tests

- **`sqlsplit_test.go` → `TestSplitCodeStart`.** Cases: no header, a
  `---` line after a trailing comment, several comments with a blank line,
  a block comment over lines, a nested block comment, a comment before
  code on the same line, an open paren, and a `$$` body. Each checks
  `Text`, where `CodeStart` points, that it lies inside `[Start, End)`, and
  that a statement with no header has `CodeStart == Start`.
- **`workspace_test.go` → `TestPickTrailingComment`.** The DO block's
  range now starts at `DO`, both with the caret in the body and with it on
  the `---` line. That case failed before the fix, when the range started
  at `---`.

`go vet ./...` and `go test ./...` pass, with `CATS_*` stripped.

### Checked in the real binary

The check used a fresh build in the scratchpad, `CATS_*` unset, a scratch
`HOME` and cwd, and `dbc web --no-open --listen 127.0.0.1:18457 --secret …`.
The screenshot's shape plus a second header was POSTed to `/api/v1/stmt`:

```sql
select max(t.ts) from s.t t; -- 2026-10-05T15:29:34-05:00   -- line 1
---                                                         -- line 2
DO $$ … END $$;                                             -- lines 3–8

/* report:                                                  -- lines 10–11
   daily */
-- another                                                  -- line 12
select 3;                                                   -- line 13
```

| caret | marked lines |
| --- | --- |
| line 1, in the trailing comment | 1–1 |
| line 2, on `---` | 3–8 (was 2–8) |
| line 7, in the DO body | 3–8 |
| line 11, in the `/* report */` header | 13 |
| line 12, on `-- another` | 13 |
| line 13, on `select 3` | 13 |

The server was then stopped by its pid.

## Next

Closed: N-140. Declined: None. Raised: None. Deferred: N-142, N-143 (by
the user, before the wrap). Promoted: None. Moved: N-064, N-087, N-104,
N-106, N-115, N-145, N-150 → Validate (the user's edit, committed this
session). Updated: None. Full list: `ai_docs/todo/next-list.md`.
