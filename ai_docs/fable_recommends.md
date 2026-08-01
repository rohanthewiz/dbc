# Fable's recommendations — low-hanging fruit review (2026-07-31)

A pass over the whole codebase (~3.2k lines) looking for cheap wins in
features, performance, and robustness. Overall: the code is in good shape and
performance has no real problems at this scale — the fruit is mostly in
robustness edges and a few small features.

## Robustness

1. **Garbled panic messages from scripts** — `script/engine.go` builds the
   panic message with `reflect.ValueOf(r).String()`. For any panic value that
   isn't a string (an `error`, an int, a struct) that returns literally
   `"<*errors.errorString Value>"`, not the message. `fmt.Sprint(r)` is the fix.

2. **`BEGIN` in the TUI leaks transactions into the pool** — the editor's
   Ctrl+R goes through `mgr.RunContext`, which grabs whichever pooled
   connection is free. Headless mode got a pinned `Session` for exactly this
   reason, but the TUI didn't: running `BEGIN`, then `UPDATE`, then `COMMIT`
   as three Ctrl+R presses can land on three different connections, leaving a
   pooled connection stuck inside a transaction, and `SET`/temp tables
   silently don't carry over. Pin one `db.Session` per active connection in
   the TUI.

3. **A multi-statement selection runs as one statement** — `stmtToRun`
   returns the raw selection text, so selecting two statements and hitting
   Ctrl+R sends both to the driver in one call, which errors on
   Postgres/MySQL. Run the selection through `sqlsplit.Split` and execute
   sequentially.

4. **Ctrl+C quits even while a query runs** — anyone with psql muscle memory
   pressing Ctrl+C to cancel a slow query loses the whole session. Make
   Ctrl+C cancel when busy, quit when idle. Related: when a modal is open the
   input capture passes Ctrl+C through to tview's default handler, which
   stops the app without going through `quit()`'s cancel.

5. **Unbounded log pane** — the log view never trims, so a long session
   (especially chatty scripts) grows memory forever.
   `tview.TextView.SetMaxLines(n)` is a one-liner.

6. **Silent truncation in headless single-statement output** — the
   `Truncated` flag only surfaces in multi-statement banners and JSON
   envelopes. `./dbc -f csv "SELECT …"` that hits `max_rows` produces a
   clean-looking, quietly incomplete file. A one-line note to stderr fixes it
   without polluting the data stream.

7. **Unset env vars in DSNs expand to empty string** — `os.ExpandEnv` turns a
   typo'd `${PGPASS}` silently into `""` and yields a confusing auth failure.
   Check referenced vars and warn by name. Same neighborhood:
   `default_connection` naming a nonexistent connection and duplicate
   connection names are both accepted without complaint at load time.

8. **File-based SQLite has no busy handling** — `mode=memory` is
   special-cased, but a file-backed SQLite gets a full connection pool with
   no `busy_timeout`, so a script doing concurrent writes hits
   `database is locked` immediately. Set `MaxOpenConns(1)` or a busy_timeout
   pragma for the sqlite driver generally.

9. **JSON export drops duplicate columns** — `rowMaps` keys by column name,
   so `SELECT a, b AS a` loses a column. At least document it; suffixing
   duplicates (`a_2`) is the cheap fix.

## Features (low effort, high payoff)

- **Persist the editor buffer across sessions** — losing the buffer on quit
  is the sharpest UX edge right now. Save to `~/.config/dbc/buffer.sql` on
  quit, restore on start.
- **Query history** — even a minimal version (executed statements appended to
  a file, a modal to recall one) transforms daily use.
- **Honor `-f`/`-o` for headless scripts** — `runScriptHeadless` hardcodes
  text tables to stdout, so `./dbc -f csv script foo.go` silently ignores the
  flag. The `Show` callback could render via the chosen format.
- **A "list tables" key** — a per-driver catalog query (`\dt` equivalent)
  behind one keybinding.
- **Style NULLs in the results table** — `Raw` already distinguishes `nil`
  from the string `"NULL"`; render real NULLs in the muted color.
- **Copy row/cell from the results table** — the clipboard dep is already
  there; a `y`-style key on the table.

## Performance

Nothing pressing — the `max_rows` cap protects the TUI, rendering is all
string-builder based, and connections are cached. Only latent item:
`renderResult` materializes a tview cell for every value, so a very large
`max_rows` (say 50k) would make the table sluggish; a separate display cap
would decouple "rows fetched for export" from "rows rendered".

## Status

Done (2026-07-31): robustness items 1–5 and editor-buffer persistence, one
commit each. Remaining: robustness 6–9 and the other feature/performance
items above.
