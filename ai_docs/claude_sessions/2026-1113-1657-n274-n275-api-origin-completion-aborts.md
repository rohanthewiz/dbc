# N-274 and N-275: the API refuses other origins, and completion lets go of the browser's connections

Session: `a87c1248-1734-49b1-82fd-05d5621a02bd`

## Ask

Two items from the next-list (RPU plan, phase 2), one commit each, then
`/commit`, `/push` and `/sw`.

- **N-274.** dbc web checked `Origin` on non-GET requests only
  (`web/auth.go:116`). SameSite doesn't look at the port, so a page on
  another `127.0.0.1` port is same-site with dbc web, and the browser sent
  the session cookie with its GETs. One GET has a side effect:
  `/api/v1/conn-tables?conn=` dials the named connection. The caller can't
  read the response. Fix inside the existing checks: on `/api/*`, refuse a
  present `Origin` or `Sec-Fetch-Site` that isn't same-origin, GETs
  included. No new auth machinery.
- **N-275.** Completion requests could use up the browser's six
  connections. `handleComplete` blocked on the schema load in flight (up
  to 30 s for each catalog read). The editor never aborted a request
  Monaco had dropped, and `dbc.api` has no timeout. On a slow catalog,
  five pending completions plus the event stream left Run, Ctrl+K and
  saves queued for a minute or more. Fix suggested: answer at once with
  the vocabulary and `incomplete: true` while loading, or abort superseded
  requests with an `AbortController`.

Commits: `4ac63f0` (N-274, pushed) and `e0b90cf` (N-275).

## What was built

### N-274: `web/auth.go`

`guard`'s Origin line is now
`sameOrigin(method, path, Origin, Sec-Fetch-Site, Host)`:

| request | checked | why |
|---|---|---|
| POST, PUT, DELETE, … | yes | it changes something (as before) |
| GET/HEAD under `/api/` | yes | some reads have effects |
| GET/HEAD of a page or asset | no | a link from elsewhere may open dbc; `frame-ancestors` already keeps it out of frames |

- **`Origin`**, when present, must be this server. `null` (a sandboxed
  frame, a `file://` page, a cross-origin redirect) is refused.
- **`Sec-Fetch-Site`** (`fetchSiteOK`), when present, must be
  `same-origin` or `none`. `none` is a navigation only the user starts:
  typed, reloaded or bookmarked. So a reopened plan tab or export URL
  still works. `same-site` (exactly the other-port page) and `cross-site`
  are refused.
- When both headers are absent the request passes: curl, scripts, tests
  and older browsers. Those still need the secret.

Both headers are needed. A no-cors GET (`<img>`, `<script>`, `fetch` with
`mode: "no-cors"`) carries no Origin, and an older browser sends no
Sec-Fetch-Site. A page's script can't set either (both are forbidden
header names). Sec-Fetch-Site now applies to non-GET requests too. That
changes nothing for a browser, which always sends Origin there.

The README's dbc web **Access** paragraph now says the API answers only
dbc's own page and clients that aren't browsers.

### N-275: server

- **`workspace.Workspace.Complete`.** The not-ready answer is now the
  dialect's vocabulary marked `Incomplete`; it used to be empty. The TUI
  ignores it and waits for its load as before (`tui/complete.go`).
- **`Workspace.CompletionErr`** (new). The error of the cached failed
  read, or nil while reading or when nothing is cached. dbc web answers
  before a slow read lands, so a read that fails afterwards would
  otherwise never reach the page. It now goes on every answer while the
  failure is remembered, and the page's `noteOnce` logs it once.
- **`web/complete.go`.** `awaitLoad` starts the read on a `goSafe`
  goroutine and waits up to `completeWait`, which is 500 ms: a quick
  catalog still answers in full on the first ask. After that the request
  answers with the vocabulary marked incomplete. Because of that mark,
  Monaco asks again on the next keystroke, and the read carries on into
  the cache. A request that comes in while a read is in flight starts a
  goroutine that only waits: `LoadCompletions` joins the read in flight.
  The file's header has a timeline diagram.

  The reads are package variables (`loadCompletions`,
  `loadConnCompletions`), as is `completeWait`, so that tests can swap
  them. This follows the existing `prepareDump` style.
- **Pipeline fields (`conn-complete`).** These work the same way, through
  new `ConnCompletions.Ask` (Complete without a read) and `Load`. `load`'s
  checks moved into `peekLocked`, which returns `(st, ready, wait)` and is
  documented as a table. It covers an unknown connection, introspect off,
  a fresh entry, a recently failed one, a read in flight, and a read due.
- **`conn-tables` still waits for a whole read.** It is one request per
  connection, and the page keeps the answer 30 s. If it answered with no
  tables while loading, the page would cache "no tables" for that time.

### N-275: page

- **`dbc.api(method, path, body, signal)`** (`core.js`). The optional
  AbortSignal is new. There is still no timeout of its own, because a
  run, an explain or a dump may rightly take minutes.
- **The completion provider** (`editor.js`) ties Monaco's cancellation
  token to an `AbortController`. An abort lands in the existing `catch`,
  which returns no suggestions. It also returns early if the token is
  already cancelled.

The README's **Completion** section now says that in dbc web a slow read
offers the dialect's keywords and functions until it has finished.

## Tests

**N-274**

- `web` `TestCrossSiteAPIGetIs403` sends the session cookie, as the
  other-port page would.
  - Refused (403): an Origin of another port, of another site, or
    `null`; a `same-site` or `cross-site` Sec-Fetch-Site; HEAD; the
    event stream; a POST with only a cross-site Sec-Fetch-Site.
  - Allowed (200): the page's own requests (Sec-Fetch-Site same-origin,
    with or without Origin); `none`; neither header; a page or asset
    from elsewhere.
- `TestSameOriginTable` covers the function directly. Both tests fail
  with the old GET exemption put back.
- New e2e step **"a page on another port is refused"**
  (`web/e2e/crossport_test.go`). A page from an `httptest` server on
  another 127.0.0.1 port makes three requests to conn-tables: an `<img>`,
  a no-cors fetch and a cors fetch. The step reads the real headers and
  status off CDP (`Network.requestWillBeSentExtraInfo` and
  `responseReceivedExtraInfo`).
  - Chrome sends the session cookie with `Sec-Fetch-Site: same-site` on
    all three, and Origin only on the cors one.
  - dbc answers 403 to all three, and the workbench's own request
    answers 200.
  - With the old guard the step sees 200s. That confirms the item's
    premise in a real browser.

**N-275**

- `web`: `TestCompleteDoesNotWaitOutASlowRead`,
  `TestCompleteWaitsForAQuickRead` and
  `TestConnCompleteDoesNotWaitOutASlowRead`.
  - The read is held on a channel by swapping the load variables, so
    "slower than `completeWait`" is certain rather than timed.
  - Three asks in a row answer promptly with the vocabulary and no
    tables. After the read is released, `cats` is offered and the list is
    complete again.
  - With `awaitLoad`'s timer set to an hour, the first test times out.
- `workspace`: `TestCompletionNotReadyIsVocabulary`, `TestCompletionErr`
  (the failure is set directly, since a sqlite read doesn't fail) and
  `TestConnCompletionsAsk`. The last checks that Ask doesn't read, that
  an unknown connection is answered at once, and that Load joins a read
  in flight (here it waits until its context ends).
- New e2e step **"a stale completion request is aborted"**
  (`web/e2e/complabort_test.go`). The page holds `/complete` requests for
  30 s, rejecting a held request when its signal aborts.
  - `c.` sends ask 1, which is held.
  - Ctrl+Space replaces it, and ask 1 is aborted.
  - Escape on the "Loading…" list aborts ask 2.
  - Every request is settled. The step fails with the signal removed.

**Runs**

- Full `go test ./...` (`CATS_*` stripped) passed after each item.
- Full e2e suite (~80 s), N-274: green.
- Full e2e suite, N-275: green in 2 of 3 runs. One run failed in "share
  a result with the assistant": a 20 s wait, with the message not
  captured. That step passed 4 of 4 runs alone. Its only completion
  request is the warm-up ask its connect sends. Not raised: there's
  nothing to go on yet.

## Notes

- **How Monaco cancels a completion ask**, found in Chrome, not from its
  source:
  - Ctrl+Space (a new ask) cancels a pending one.
  - Escape over a visible list cancels; "Loading…" counts as visible.
  - Escape right after an auto-triggered ask (`.` or a word start)
    cancels nothing. No list is on screen yet, and Escape's binding needs
    one.
  - Typing while an ask is pending asks nothing new. Monaco filters the
    answer when it arrives.

  So the stale requests come from asks that were replaced or lists that
  were closed. Saved to the e2e harness memory.
- **N-323, raised.** A panic in a schema read would leave `loading` set
  for good. Before N-275, every later completion request then hung. Now
  the answer is the vocabulary only, plus a waiting goroutine per ask,
  until a reconnect. No such panic is known.
- **gofmt.** It flagged a doubled blank line in `web/server_test.go` that
  was already there at HEAD; fixed in the N-274 commit.
- **Mutation checks.** Each one swapped a line with `sed` and restored
  it the same way, or from a scratch copy, before the next step.

## Next

Closed: N-274, N-275. Declined: None. Raised: N-323. Deferred: None.
Promoted: None. Moved: None. Updated: None. Full list:
`ai_docs/todo/next-list.md`.
