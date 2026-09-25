package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// Copilot sign-in from inside dbc: GitHub's device flow, driven through the
// language server's own auth methods, so a user with no other Copilot editor
// is not sent off to install one just to sign in.
//
// WHY LSP MODE AND NOT ACP's authenticate. In ACP mode the server does
// advertise an auth method ("github_oauth"), but its authenticate runs an
// OAuth CODE flow: the server opens a browser itself and waits for a
// localhost callback. That needs a browser on the machine dbc runs on, and a
// terminal database client is often run over SSH, where there is none. The
// DEVICE flow shows a short code the user types at github.com/login/device
// on any machine — but the server exposes it only in LSP mode, as the custom
// methods below. It is the path ced has used since July 2026.
//
//	BeginSignIn ─spawn (--stdio)─► initialize ─► initialized ─► signIn
//	     │                                                        │
//	     │  Status.SignedIn() ◄──── {status: "AlreadySignedIn"} ◄─┤
//	     ▼                                                        │
//	SignIn{UserCode, VerificationURI} ◄── {status: "PromptUserDeviceFlow",
//	     │                                 userCode, verificationUri, command}
//	     ▼
//	Wait ─► workspace/executeCommand(command) — blocks until the user
//	        finishes in the browser (≤ 15 min) ─► AuthStatus; process closed
//
// The server writes the credential to its own store — the one ACP mode reads
// — so the next chat handshake finds it. dbc never sees the token.
//
// This process is separate from any chat: a failed chat handshake has
// already closed its agent, and an ACP connection cannot be switched to LSP.
// It lives only for the sign-in and is closed by Wait or Close.

// Sign-in timeouts, each explained by what it waits on.
const (
	// signInCodeTimeout covers signIn, which asks GitHub for a device code
	// over the network.
	signInCodeTimeout = 30 * time.Second
	// signInTimeout covers the wait for the user to enter the code. GitHub's
	// device codes live about 15 minutes; matching that means dbc never
	// gives up on a code the user could still redeem.
	signInTimeout = 15 * time.Minute
)

// fallbackVerifyURL is where device codes are redeemed when the server's
// response leaves verificationUri out (older builds did).
const fallbackVerifyURL = "https://github.com/login/device"

// AuthStatus is the server's account state: its status word and, when
// signed in, the GitHub login.
type AuthStatus struct {
	Status string `json:"status"` // "OK", "AlreadySignedIn", "NotSignedIn", "NotAuthorized", …
	User   string `json:"user"`
}

// SignedIn reports whether the status means a usable account. Anything
// unrecognised counts as signed out, which fails safe: the user just signs
// in again. MaybeOK (the server could not reach GitHub to confirm a stored
// token) counts as in, as the server itself treats it.
func (s AuthStatus) SignedIn() bool {
	switch strings.ToLower(s.Status) {
	case "ok", "alreadysignedin", "maybeok":
		return true
	}
	return false
}

// NoSubscription reports the one failure worth its own words: the GitHub
// sign-in worked, but that account has no Copilot access, so signing in
// again will not help.
func (s AuthStatus) NoSubscription() bool { return s.Status == "NotAuthorized" }

// signInCommand is the LSP Command object signIn hands back; Wait echoes it
// through workspace/executeCommand. Arguments stay raw — their shape is the
// server's business.
type signInCommand struct {
	Command   string            `json:"command"`
	Arguments []json.RawMessage `json:"arguments"`
}

// SignIn is a device-flow sign-in in progress.
//
// When the account was already signed in, Status says so, UserCode is empty,
// and the server has already been closed; Wait then just returns Status.
// Otherwise the UI shows UserCode and VerificationURI and calls Wait.
type SignIn struct {
	UserCode        string
	VerificationURI string
	Status          AuthStatus

	conn *rpcConn
	cmd  signInCommand
}

// BeginSignIn starts the agent's language server in LSP mode and asks it for
// a device code. It blocks for the handshake and the code request — up to
// the initTimeout, since a cold start can sit on a Keychain prompt — so a UI
// calls it off its event loop.
func BeginSignIn(a Agent, dir string) (*SignIn, error) {
	if a.SignInArgs == nil {
		return nil, fmt.Errorf("dbc cannot sign in to %s — %s", a.Name, a.Auth)
	}
	if !a.Installed() {
		return nil, fmt.Errorf("%s is not installed (%s not on PATH) — install it with: %s",
			a.Name, a.Binary, a.Install)
	}
	conn, err := spawn(dir, a.Binary, a.SignInArgs, true, nil, answerLSPRequest, nil)
	if err != nil {
		return nil, err
	}
	return beginSignIn(conn, dir)
}

// BeginSignInPipes is BeginSignIn over an existing connection to a language
// server — r is what the server writes, w what it reads — as StartPipes is
// for a chat. It is how tests drive the flow against a scripted server (see
// package aitest). Closing the SignIn closes w.
func BeginSignInPipes(dir string, r io.Reader, w io.Writer) (*SignIn, error) {
	return beginSignIn(newLSPConn(r, w, nil, answerLSPRequest, nil), dir)
}

// beginSignIn runs the LSP handshake and signIn on conn. conn is closed on
// every path that does not hand it to the caller.
func beginSignIn(conn *rpcConn, dir string) (*SignIn, error) {
	var res struct {
		AuthStatus
		UserCode        string        `json:"userCode"`
		VerificationURI string        `json:"verificationUri"`
		Command         signInCommand `json:"command"`
	}
	err := lspHandshake(conn, dir)
	if err == nil {
		err = conn.call("signIn", map[string]any{}, &res, signInCodeTimeout)
	}
	if err != nil {
		conn.close()
		return nil, err
	}
	if res.AuthStatus.SignedIn() {
		conn.close()
		return &SignIn{Status: res.AuthStatus}, nil
	}
	if res.UserCode == "" {
		conn.close()
		return nil, errors.New("the language server returned no device code")
	}
	s := &SignIn{UserCode: res.UserCode, VerificationURI: res.VerificationURI,
		Status: res.AuthStatus, conn: conn, cmd: res.Command}
	if s.VerificationURI == "" {
		s.VerificationURI = fallbackVerifyURL
	}
	return s, nil
}

// lspHandshake is the server's required opening: initialize, initialized,
// and a settings push. The initializationOptions are NOT decoration — the
// server refuses clients that do not name an editor and plugin.
func lspHandshake(conn *rpcConn, dir string) error {
	root := (&url.URL{Scheme: "file", Path: filepath.ToSlash(dir)}).String()
	info := map[string]any{"name": "dbc", "version": buildVersion()}
	params := map[string]any{
		// The server watches this pid and exits with it, so a dbc that dies
		// mid-sign-in does not leave the server waiting out its 15 minutes.
		"processId":        os.Getpid(),
		"rootUri":          root,
		"capabilities":     map[string]any{"workspace": map[string]any{"workspaceFolders": true}},
		"workspaceFolders": []any{map[string]any{"uri": root, "name": filepath.Base(dir)}},
		"initializationOptions": map[string]any{
			"editorInfo":       info,
			"editorPluginInfo": map[string]any{"name": "dbc-assistant", "version": info["version"]},
		},
	}
	if err := conn.call("initialize", params, nil, initTimeout); err != nil {
		return err
	}
	if err := conn.notify("initialized", map[string]any{}); err != nil {
		return err
	}
	// The server expects settings after initialized; empty means all
	// defaults. A lost notification costs nothing sign-in uses.
	_ = conn.notify("workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{}})
	return nil
}

// Wait finishes the device flow: it blocks until the user has entered the
// code in the browser, or the code expires, then closes the server. The
// status says which ending it was — SignedIn, NoSubscription, or neither.
func (s *SignIn) Wait() (AuthStatus, error) {
	if s.conn == nil {
		return s.Status, nil
	}
	defer s.conn.close()
	var st AuthStatus
	args := s.cmd.Arguments
	if args == nil {
		args = []json.RawMessage{} // the spec's arguments is an array, never null
	}
	err := s.conn.call("workspace/executeCommand",
		map[string]any{"command": s.cmd.Command, "arguments": args}, &st, signInTimeout)
	if errors.Is(err, errConnClosed) {
		// Close from the UI (the user gave up) or the server died; either
		// way the flow did not complete, and the plain words say so.
		return AuthStatus{}, errors.New("sign-in was stopped before it finished")
	}
	return st, err
}

// Close abandons the sign-in and ends the server. Safe to call more than
// once, before or during Wait (which then returns an error).
func (s *SignIn) Close() {
	if s.conn != nil {
		s.conn.close()
	}
}

// answerLSPRequest answers the server's requests to the client with the
// emptiest legal payload, as an editor that supports nothing would:
// workspace/configuration gets one empty object per item it asked for (the
// server waits on it), and everything else — capability registration,
// progress tokens, message prompts — accepts null. An error would be just
// as honest, but a server can treat one as fatal where null is routine.
func answerLSPRequest(method string, params json.RawMessage) (any, error) {
	if method != "workspace/configuration" {
		return nil, nil
	}
	var p struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(params, &p)
	empties := make([]any, len(p.Items))
	for i := range empties {
		empties[i] = map[string]any{}
	}
	return empties, nil
}

// buildVersion is dbc's module version as the Go toolchain stamped it
// ("(devel)" for a local build), for the editorInfo the server requires.
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}
