# A project skill for driving dbc

Session: `aed3ad7e-ef6d-4ba8-911c-5c9471439e37`

## Ask

From the cats-todo backlog: "Create a skill for DBC under the project's
.claude dir".

## What it does

`.claude/skills/dbc/` is a project skill, listed as `dbc` in sessions in
this repo. It teaches an agent to use dbc from a shell and to verify
changes to it:

- **`SKILL.md`** — the working subset of the README:
  - before running: build fresh to a scratch path (the root `./dbc` is
    usually stale), unset `CATS_*`, use a scratch `HOME` and cwd so the
    user's config and the repo's `./dbc.toml` stay out of it, and ask
    before writing to a real connection (`copy --truncate`,
    `migrate up/down`, DML/DDL);
  - which database a run uses: config resolution, `-c`, `--driver/--dsn`
    (banner name `dsn`), the `demo-bytdb`/`demo-sqlite` fallback with its
    `cats` table, `conn/otherdb`, `${VAR}` expansion, placeholder style;
  - headless SQL: prefer `-t json`, the one-argument rule, `-f` vs `-t`,
    the pinned session and `--tx`/`-k`, the `max_rows` note on stderr,
    exit codes 0/1/2/3/130;
  - a subcommand table (`script`, `copy`, `explain`, `erd`, `migrate`,
    `web`, `version`) pointing at README sections;
  - working on dbc: build/vet/test, the three opt-in suites, how to show a
    change working, a package map.
- **`scripting.md`** — loaded on demand: the `sdb.S` API, ETL
  (`Copy`/`Reader`/`Writer`) and its per-engine behavior, and the yaegi
  `m[k], _ = …` bug with the workaround. Scripts get stdlib + `sdb` only
  (checked in `script/engine.go`: `stdlib.Symbols` plus dbc's exports).

## Design choices

- **Usage first, development second.** The skill is mostly about driving
  the binary, because that is what both a dbc user's agent and a dbc
  developer verifying a change need; the dev section is short and points
  at the README's Tests and Releasing.
- **Points at the README rather than copying it.** Section names, not line
  numbers, so the pointers survive edits. The skill repeats only what an
  agent gets wrong without it.
- **Nothing from private memory.** The skill is committed, so the live-DB
  note uses a generic `docker run` line, and releasing says only "don't
  push `release` or tags unless asked" — the README's release-branch flow
  and the tag-only habit for patch bumps don't agree, and the skill
  shouldn't pick one.

## Verification

Against a fresh build in a scratch `HOME`/cwd with `CATS_*` unset: a demo
query (text and `-t json`), a three-statement run on an ad-hoc SQLite DSN,
a script with `-t csv` (Print → stderr, `Show` → stdout), `explain`, and a
bad column (structured error on stderr, exit 1). Package roles in the map
and the `sdb.S` method list were checked against the source. `/skills`
picked the skill up in this session.

## Next

Closed: None. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
