# N-284, N-290, N-295: dbc web's small events, columns past max_rows, a load's failing rows

Session: `cbde2eba-f688-4850-8468-0b28475b5b48`

## Ask

`/work-next-list`, the third batch. The triage is in
`2026-1118-0042-n321-n286-n288-n291-plain-errors-value-text-status-error`.
The batch before this one is
`2026-1118-0103-n292-n243-n302-n283-tui-find-live-view-ddl-fuzz-row-counts`.

## N-284: dbc web stops doing whole-buffer work on small events (`b6f5d49`)

Done:

- **The asset hash.** `assetVersion` (`web/server.go`) is now
  `sync.OnceValue(hashAssets)`. The 4.7 MB hash is computed once per
  process, not on every sign-in or error page, which an unauthenticated
  client could ask for.
- **The statement marker.** `editor.js` `mark()` remembers the last answer
  (`marked`: text, driver, range). It skips `/stmt` while the caret stays
  inside a non-empty marked range with the text and driver unchanged.
  Both sides count in UTF-16 units, so the comparison is sound.
  `isScript` clears it.
- **The console event.** `consoleEvent` now carries the revision, not the
  text.
  - `onConsoleSaved` (`app.js`) reads the text with a GET, and only in a
    window that will take it: the console open there and clean.
  - It adopts the text only if the console is still clean when the answer
    lands. A failed GET leaves the next save to meet the conflict.
- **The tab row.** A console-backed tab row now saves `buffer: ""`
  (`tabBody`), so a typing pause sends and stores the text once, in the
  console.
  - The cost: a dbc from before consoles, or a load that cannot read the
    console, opens such a tab empty.
  - The boot path now logs where the text is when a console cannot be
    read.

Not done:

- `/folds`: Monaco already debounces it and cancels a request the text has
  moved past.
- Dropping results at the idle release. With the default
  `conn_idle_timeout` (1 h), a laptop asleep over lunch would come back to
  empty result tabs: a change the user should weigh. Raised as **N-335**
  with the options.

Tests:

- `TestConsoleSaveBroadcast`: the event holds no text, and a GET at its
  revision returns it.
- The web suite.
- The full web e2e (`DBC_E2E=1`, 33 steps). Its step "tabs survive a
  reload" failed first: it waited for the tab row's copy of the text. It
  now wants the row to name its console with an empty buffer, and passes.

## N-290: columns for the assistant are not cut at max_rows (`719431f`)

`Manager.Columns` now reads through `stringRows` on the pool, not
`RunContext`. Errors carry `op=read columns`.

- `TestColumnsIgnoreMaxRows` (SQLite, `max_rows` 2, two 3-column tables)
  fails on the old code with `[[a1 a2] []]` and passes now.
- `TestLiveColumnsPostgres` and `TestLiveColumnsMySQL` pass on the Docker
  servers.

## N-295: a load's failure says which rows, with Postgres's detail (`963b6c3`)

**A new leaf package, `pgerr`.** `db.withPgDetail` and `pgContext` (and
their limits) moved there unchanged, as `WithDetail`, `Context`,
`ContextFrames` and `ContextRunes`.

- `db` keeps thin wrappers, so its callers and `notice_test.go` are
  unchanged.
- `etl` imports `pgerr`, not `db`: `db` would have pulled explain, erd and
  raster into the loaders.

**Where etl adds the server's DETAIL, HINT and CONTEXT:**

- the COPY goroutine's `copyResult`, so every path that reports the COPY's
  failure has them;
- the upsert merge;
- the INSERT flush.

**Which rows failed:**

- A batched INSERT names its rows (`rows=4-6`) beside `batch_rows`.
- `Writer.Write`'s `row` field is now `sent`. It named the row that filled
  a batch, not the one that failed.
- The wrong-length-row error still names its `row`.

Tests:

- `TestInsertFailureNamesTheBatchRows` (SQLite, batches of 3, a duplicate
  key at row 5) gives `rows=4-6` and no `row` field.
- Live `TestLiveCopyFailureCarriesDetail` on postgres:17:
  `ERROR: duplicate key value violates unique constraint
  "dbc_live_loaderr_pkey" (SQLSTATE 23505) — DETAIL: Key (id)=(42) already
  exists. — CONTEXT: COPY dbc_live_loaderr, line 51`. A CHECK failure
  names `line 2`.
- The root, etl, pipeline, jobs, sdb and db suites pass.

## CI

Every push this run supersedes the one before (`test.yml`'s concurrency
group), so the runs for `fe34747` … `719431f` were cancelled. The run
for `b6f5d49` was in progress at this wrap. The new "Fuzz (brief)" step
has not completed a run yet; the final wrap checks it.

## Next

Closed: N-284, N-290, N-295. Declined: None. Raised: N-335. Deferred:
None. Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
