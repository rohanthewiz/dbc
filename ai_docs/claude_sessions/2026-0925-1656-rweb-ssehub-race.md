# rweb v0.1.32: SSEHub drop-counter race (N-050)

Session: 7376c9ed-94c2-4029-ac4a-89053d571cb5
Date: 2026-09-25

## Ask

Next-list item N-050: rweb v0.1.31's `SSEHub.broadcastToClients` bumps each
client's `dropped` counter under the hub's READ lock, so concurrent
`Broadcast` calls race. dbc worked around it with a per-window send mutex in
`web/hub.go`. Fix it in rweb (the sibling repo, `~/projs/go/rweb`), then drop
the mutex.

## rweb (v0.1.32, `3169f85`)

| Piece | Change |
|---|---|
| `sse_hub.go` | `hubClient.dropped` is now `atomic.Int32`: `Store(0)` on a successful send, `Add(1)` on a full channel, and the eviction check uses `Add`'s result. The write lock was the alternative; it would serialize every broadcast to protect one counter. Under concurrent broadcasts "consecutive" drops is approximate (another goroutine's success can reset the count), which only delays evicting a client that is draining again. Two broadcasts may both mark a client stale; `unregisterLocked` is idempotent. |
| `sse_hub_test.go` | `TestSSEHubConcurrentBroadcast`: 8 senders × 200 `BroadcastRaw` against one full and one drained client, eviction off. Reproduced the race before the fix. |

Tagged v0.1.32, pushed master and the tag to `origin` (GitHub) only — not
`go_origin`, at the user's choice.

## dbc

| Piece | Change |
|---|---|
| `go.mod` | rweb v0.1.31 → v0.1.32 |
| `web/hub.go` | `window.sendMu` and its lock in `window.send` removed (the item said `tab.sendMu`; it lived on `window`). The `sse` field's comment says why concurrent sends are fine now. Each send is one channel send per stream, so a stream still sees a single order of events. |
| docs | `ai_docs/plans/web-ui.md` note updated; N-050 closed. |

## Verified

- rweb: `go test -race ./...` green; vet, gofmt clean.
- dbc on v0.1.31 with the mutex removed: `web` tests hit `DATA RACE`
  (`TestChatAskWithholdsRowsAndSaves` fails) — the tests do cover it.
- dbc on the fix (local replace, then v0.1.32): `go test -race ./...` green
  (CATS_* stripped); vet, gofmt clean.

## Next

Closed: N-050. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
