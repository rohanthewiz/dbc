package tui

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/ai"
)

// Signing in to the assistant's agent from the pane, for an agent that
// CanSignIn (Copilot). Without it, a user with no other Copilot editor hits
// "Authentication required" and is told to go and sign in somewhere else.
//
// It is GitHub's device flow (see ai/signin.go for why that flow):
//
//	handshake fails ── ErrAuthRequired ──► ⎆ sign in chip under the error
//	    │ click (or the transcript menu's "Sign in to …")
//	    ▼
//	signInBeginCmd ── ai.BeginSignIn (≤2 min) ──► chatSignInCodeMsg
//	    │ already signed in? ─────────────────────► reconnect
//	    ▼
//	code in the transcript, copied, browser opened;
//	  ⧉ copy code · ↗ open page · ✕ cancel chips while waiting
//	    │
//	signInWaitCmd ── SignIn.Wait (≤15 min) ──► chatSignInDoneMsg
//	    │ signed in ──► reconnect: ensureChat, and a question queued before
//	    │               the refusal goes out on EventReady
//	    └ otherwise ──► say which ending, offer the chip again
//
// THE CODE GOES IN THE TRANSCRIPT, not a modal: the wait can take minutes
// and the user will look back for the code, possibly after switching away.
// It is also copied and the browser opened at once — the click on "sign in"
// is the consent to both, and both are conveniences: over SSH neither
// reaches the user, and the code and URL on screen are enough.
//
// The flow runs its own short-lived process, independent of the chat's
// connection, so ⟲ new while waiting does not lose it; switching to another
// agent, or quitting, does end it.

// signInState is where a sign-in is.
type signInState int

const (
	signInNone     signInState = iota
	signInStarting             // asking the server for a code
	signInWaiting              // the user has the code; waiting on GitHub
)

// chatSignInCodeMsg lands ai.BeginSignIn's answer. seq ties it to the
// attempt that asked, so a cancelled attempt's late answer is dropped.
type chatSignInCodeMsg struct {
	seq int
	s   *ai.SignIn
	err error
}

// chatSignInDoneMsg lands the end of the device flow.
type chatSignInDoneMsg struct {
	seq int
	st  ai.AuthStatus
	err error
}

// beginSignIn is ai.BeginSignIn, as a var so tests run the flow against a
// scripted server — the real one would pop a browser and wait on GitHub.
var beginSignIn = ai.BeginSignIn

// openURL opens a page in the user's browser, best effort: a headless or
// remote host just means the user opens the URL on screen by hand. A var so
// tests never launch a browser.
var openURL = func(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	// Start and reap on a goroutine; the opener's output would draw over
	// the TUI, so it goes nowhere.
	go func() { _ = cmd.Run() }()
}

// offerSignIn reports whether the ⎆ sign in chip belongs under the
// transcript: the agent refused for lack of sign-in — at the handshake, or
// on a turn after the credential was revoked — dbc can sign in to it, and no
// sign-in is under way.
func (p *chatPane) offerSignIn() bool {
	return p.needAuth && p.signState == signInNone && p.agent.CanSignIn()
}

// chatSignIn starts a sign-in (the ⎆ chip, or the transcript menu).
func (m *Model) chatSignIn() tea.Cmd {
	p := m.chat
	if p.signState != signInNone {
		if p.signCode != "" {
			m.logf(logInfo, "sign-in is waiting on GitHub — enter %s at %s", p.signCode, p.signURL)
		}
		return nil
	}
	a := p.agent
	if a.ID == "" {
		a = m.aiAgent
	}
	if !a.CanSignIn() {
		m.logf(logWarn, "dbc cannot sign in to %s — %s", a.Name, a.Auth)
		return nil
	}
	dir, err := os.Getwd()
	if err != nil {
		dir = os.TempDir()
	}
	p.signSeq++
	p.signState = signInStarting
	p.add(roleInfo, "starting GitHub sign-in…")
	p.follow = true
	seq := p.signSeq
	return func() tea.Msg {
		s, err := beginSignIn(a, dir)
		return chatSignInCodeMsg{seq: seq, s: s, err: err}
	}
}

// chatSignInCode lands the device code — or the news that no code is
// needed — and starts the wait.
func (m *Model) chatSignInCode(msg chatSignInCodeMsg) tea.Cmd {
	p := m.chat
	if msg.seq != p.signSeq || p.signState != signInStarting {
		if msg.s != nil {
			msg.s.Close() // an attempt cancelled while it started
		}
		return nil
	}
	if msg.err != nil {
		p.signState = signInNone
		p.add(roleErr, "sign-in failed: "+msg.err.Error())
		return nil
	}
	if msg.s.UserCode == "" { // signed in all along — elsewhere, since the refusal
		return m.chatSignedIn(msg.s.Status)
	}
	p.signIn, p.signState = msg.s, signInWaiting
	p.signCode, p.signURL = msg.s.UserCode, msg.s.VerificationURI
	p.add(roleInfo, "enter code "+p.signCode+" at "+p.signURL+
		" — it is on the clipboard, and your browser should open there. Waiting for GitHub…")
	p.follow = true
	openURL(p.signURL)
	s, seq := msg.s, p.signSeq
	return tea.Batch(
		m.copyString(p.signCode, "the sign-in code"),
		func() tea.Msg {
			st, err := s.Wait()
			return chatSignInDoneMsg{seq: seq, st: st, err: err}
		})
}

// chatSignInDone lands the end of the device flow.
func (m *Model) chatSignInDone(msg chatSignInDoneMsg) tea.Cmd {
	p := m.chat
	if msg.seq != p.signSeq || p.signState != signInWaiting {
		return nil // cancelled; the cancel already said so
	}
	p.endSignIn()
	switch {
	case msg.err != nil:
		p.add(roleErr, "sign-in did not complete: "+msg.err.Error())
	case msg.st.SignedIn():
		return m.chatSignedIn(msg.st)
	case msg.st.NoSubscription():
		p.add(roleErr, "signed in to GitHub"+asUser(msg.st.User)+
			", but that account has no Copilot subscription")
	default:
		p.add(roleErr, "sign-in did not complete ("+msg.st.Status+")")
	}
	return nil
}

// chatSignedIn reports success and reconnects when the chat is down. A
// question refused along with the handshake is still pending, and goes out
// on EventReady.
func (m *Model) chatSignedIn(st ai.AuthStatus) tea.Cmd {
	p := m.chat
	p.endSignIn()
	p.needAuth = false
	if p.state == chatReady || p.state == chatStarting {
		p.add(roleInfo, "signed in to GitHub"+asUser(st.User))
		return nil
	}
	p.add(roleInfo, "signed in to GitHub"+asUser(st.User)+" — connecting…")
	p.follow = true
	return m.ensureChat()
}

// chatSignInCancel abandons a sign-in (the ✕ cancel chip).
func (m *Model) chatSignInCancel() {
	p := m.chat
	if p.signState == signInNone {
		return
	}
	p.endSignIn()
	p.add(roleInfo, "sign-in cancelled")
}

// endSignIn ends any sign-in in flight and forgets it. Bumping the seq is
// what makes a late answer from it land as a no-op.
func (p *chatPane) endSignIn() {
	if p.signIn != nil {
		p.signIn.Close()
	}
	p.signSeq++
	p.signIn, p.signState, p.signCode, p.signURL = nil, signInNone, "", ""
}

// signInRows is the transient row under the transcript: the ⎆ chip, or the
// chips for the code being waited on. It is drawn, not stored in msgs, so it
// is never archived and disappears the moment it no longer applies.
func (p *chatPane) signInRows(st styles) []chatRow {
	var r chatRow
	x := 0
	chipAt := func(label string, style Style, kind targetKind, text string) {
		label = " " + label + " "
		r.segs = append(r.segs, seg{label, style}, seg{" ", st.base})
		r.tgts = append(r.tgts, rowTarget{x, x + width(label), kind, text})
		x += width(label) + 1
	}
	switch {
	case p.offerSignIn():
		chipAt("⎆ sign in to "+p.agent.Name, st.buttonHot, targetSignIn, "")
	case p.signState == signInWaiting:
		chipAt("⧉ copy code", st.button, targetCopySignInCode, p.signCode)
		chipAt("↗ open page", st.button, targetOpenURL, p.signURL)
		chipAt("✕ cancel", st.button, targetSignInCancel, "")
	default:
		return nil
	}
	return []chatRow{{}, r}
}

// asUser is " as <login>", or nothing when the server named no login.
func asUser(user string) string {
	if user == "" {
		return ""
	}
	return " as " + user
}

// isAuthRefusal reports whether an agent error was a refusal for lack of
// sign-in.
func isAuthRefusal(err error) bool { return errors.Is(err, ai.ErrAuthRequired) }

// signInMenuLabel names the transcript menu's sign-in item for an agent.
func signInMenuLabel(a ai.Agent) string {
	return "Sign in to " + strings.TrimSpace(a.Name) + "…"
}
