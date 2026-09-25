# Copilot sign-in from inside dbc

Session: f9ea82b2-24e2-43ed-8066-36202884ae80
Date: 2026-09-25

## Ask

One next-list item, pasted as-is:

- **N-028**: "Copilot sign-in from inside dbc. Today an unsigned-in user is
  told to sign in from another editor; ced shows the device-flow code itself
  by running the language server in LSP mode (`signIn` →
  `workspace/executeCommand`). Contingent on someone using dbc without ced or
  another Copilot editor."

## Decision

Device flow over LSP mode, as the item names, ported from ced
(`~/projs/go/ced/internal/app/copilot.go`). The server's ACP mode does
advertise an auth method (`github_oauth`), but reading the bundled server
(`@github/copilot-language-server` 1.526.0, `dist/main.js`) shows its
`authenticate` runs an OAuth *code* flow: the server opens a browser itself
and waits for a localhost callback. That fails for dbc over SSH. The device
flow shows a code you can enter on any machine.

Server shapes confirmed in the bundle: `signIn` →
`{status:"AlreadySignedIn",user}` or
`{status:"PromptUserDeviceFlow",userCode,verificationUri,command:{command:"github.copilot.finishDeviceFlow"}}`;
ACP's refusal is `uo.authRequired()` → `Authentication required (-32000)`.

## What changed

- **`ai/rpc.go`**: Content-Length framing back alongside NDJSON, chosen per
  connection (`newLSPConn`, `spawn(..., lsp bool)`, `readFramed`). The port
  had dropped it as unused.
- **`ai/signin.go`** (new): `BeginSignIn(agent, dir)` spawns
  `binary SignInArgs` (`--stdio`), runs initialize (with editorInfo /
  editorPluginInfo, processId = dbc's pid) → initialized →
  didChangeConfiguration → `signIn`; returns `*SignIn{UserCode,
  VerificationURI, Status}`. `Wait()` echoes the command through
  `workspace/executeCommand` (15 min) and closes the server; `Close()`
  abandons it. `answerLSPRequest` answers `workspace/configuration` with one
  `{}` per item and null for the rest. `BeginSignInPipes` for tests.
  `AuthStatus.SignedIn()` / `NoSubscription()`.
- **`ai/agents.go`**: `Agent.SignInArgs` + `CanSignIn()`; set for Copilot
  only. Copilot's `Auth` hint now says dbc can sign in.
- **`ai/chat.go`**: `explain` returns an `authError` that matches
  `ai.ErrAuthRequired` via `errors.Is` (message unchanged in shape).
- **`ai/aitest`**: `Fake.SignedOut` (session/new refuses with -32000),
  `Fake.SignInResult`, `Fake.SignIn` (a scripted LSP sign-in server over
  framed pipes, same signature as `ai.BeginSignIn`), `FakeUserCode`.
- **`tui/chatsignin.go`** (new): pane state (`needAuth`, `signState`,
  `signIn`, `signSeq`, code/URL), the ⎆ chip row drawn after the transcript
  (never archived), copy code / open page / cancel chips while waiting,
  status line, `beginSignIn` and `openURL` vars for tests. Success reconnects
  via `ensureChat`; the pending question goes on `EventReady`.
- **`tui/chat.go`**: EventExit / EventTurnDone set `needAuth`; "⟲ new to try
  again" is replaced by the chip when sign-in is offered; `chatSchema` now
  queues a question whose handshake was refused while its schema lookup ran
  (before, it was dropped); transcript menu gains "Sign in to Copilot…";
  `switchAgent` and `close` end a sign-in.
- **`tui/app.go`**: routes `chatSignInCodeMsg` / `chatSignInDoneMsg`.
- **README**: the AI assistant section describes in-pane sign-in.

## Verification

- `go test -race ./...` passes, with CATS_* stripped from the environment.
  New tests: `ai/signin_test.go` (device flow, config requests, already
  signed in, no subscription, Close during Wait, framing cases, ErrAuthRequired),
  and four TUI tests (full flow incl. refused question sent after sign-in,
  no subscription, cancel with a late answer ignored, menu when signed in).
- Live against the real server: `DBC_AI_LIVE=1 go test -run 'LiveSignIn|LiveHandshake' ./ai`
  → signed in: `AlreadySignedIn` for the user's account. With
  `XDG_CONFIG_HOME` pointed at an empty scratch dir: the ACP handshake refuses
  with `Authentication required (code -32000)` and `signIn` returns a real
  device code + github.com/login/device. Scratch dir deleted afterwards.
- Not verified: entering a code end to end (it would mint a real token).
  `DBC_AI_LIVE_SIGNIN=1` on `TestLiveSignInStatus` does it from a signed-out
  setup.

## Next

Closed: N-028. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
