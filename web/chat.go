package web

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/workspace"
)

// The assistant pane's server half: one conversation with an ACP agent per
// browser tab, as the TUI has one per window.
//
// WHAT IS SHARED WITH THE TUI, AND WHAT IS NOT. The rules that decide what
// reaches a hosted model are shared, so the two UIs cannot send different
// things: which statement "this query" is, when its result or error goes,
// the data rule (rows only with ai_rows), hidden columns left out, rows in
// the grid's order (workspace.ChatContext); the prompt and its "sent: …"
// line (ai.Build); the schema lookup and what a failed one sends
// (workspace.LookupColumns, AttachColumns); the archive (userdata). What is
// NOT shared is the state machine around them — starting the agent, the
// transcript, sign-in — because the TUI's runs on Bubble Tea's event loop
// and this one on goroutines, the same split as tui/run.go and hub.go.
//
// THE TRANSCRIPT LIVES HERE, not in the page. A reload reattaches to the
// same tab (see hub.go) and must find the conversation it left, with the
// answer still streaming if one was; and saving to the archive — after
// every answer, as the TUI does — needs the transcript where the file is.
// The page mirrors it from events:
//
//	chat.msg   {i, role, text}   message i was added
//	chat.text  {i, text}         a chunk was appended to message i
//	chat.state {…}               the header, stop and sign-in chips changed
//	chat.reset {}                the transcript was replaced: fetch it again
//
// Messages are numbered, so a page that sees an index it does not expect
// (it missed events while its stream was down) fetches the whole transcript
// rather than drawing a wrong one. Every event is sent while holding the
// assistant's lock, so they reach the stream in the order they happened.
//
// A turn's life, the TUI's (tui/chat.go) step for step:
//
//	POST …/chat/ask ── ChatContext (the page's editor and grid view)
//	      │ tables mentioned? ─no──────────────────────────► finish ─► Send
//	      │ yes: streaming from here, so no second question overtakes it
//	      └─► go LookupColumns (pool, ≤3s) ─► schemaLanded ─► finish ─► Send
//	pump goroutine: Events() ─► onEvent ─► chat.text … EventTurnDone ─► save
//
// THE AGENT STARTS LAZILY, when the pane is first opened or a question is
// asked: most sessions never use it, and a Node process per browser tab
// would cost memory for nothing.

// Chat states, as the page sees them.
const (
	chatIdle     = "idle"     // not started
	chatStarting = "starting" // process up, handshake in flight
	chatReady    = "ready"
	chatDead     = "dead" // failed or exited; ⟲ new retries
)

// Sign-in states.
const (
	signNone     = ""
	signStarting = "starting" // asking the server for a device code
	signWaiting  = "waiting"  // the user has the code; waiting on GitHub
)

// chatLine is one transcript message. Role is the archive's word for it
// (userdata: user, agent, tool, note, error, info), so saving is a copy.
type chatLine struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// assistant is one tab's conversation.
type assistant struct {
	srv *Server
	t   *tab

	mu        sync.Mutex
	c         *ai.Chat
	gen       int // bumped per agent connection; events from an older one are dropped
	state     string
	agent     ai.Agent
	models    []ai.Model
	modelID   string
	msgs      []chatLine
	streaming bool   // an answer is in flight (a schema lookup included)
	first     bool   // the next turn is the conversation's first
	pending   string // a prompt written before the handshake finished
	needAuth  bool   // the agent refused for lack of sign-in

	signState string
	signIn    *ai.SignIn
	signSeq   int // bumped per attempt and on its end; a stale answer is dropped
	signCode  string
	signURL   string

	archiveID    string
	archiveStart time.Time
	saveErr      string // the last save failure logged, so it is logged once
}

func newAssistant(s *Server, t *tab) *assistant {
	return &assistant{srv: s, t: t, state: chatIdle, first: true}
}

// ---------------------------------------------------------------------------
// What the page sees
// ---------------------------------------------------------------------------

type agentInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Binary    string `json:"binary"`
	Installed bool   `json:"installed"`
	Install   string `json:"install,omitempty"`
	CanSignIn bool   `json:"canSignIn"`
	Auth      string `json:"auth,omitempty"`
}

func infoOf(a ai.Agent) agentInfo {
	return agentInfo{ID: a.ID, Name: a.Name, Binary: a.Binary, Installed: a.Installed(),
		Install: a.Install, CanSignIn: a.CanSignIn(), Auth: a.Auth}
}

type modelInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Usage string `json:"usage,omitempty"`
}

// chatView is the pane's chrome: header, stop, the sign-in chips.
type chatView struct {
	State     string      `json:"state"`
	Agent     agentInfo   `json:"agent"`
	Model     string      `json:"model"`
	ModelID   string      `json:"modelId"`
	Models    []modelInfo `json:"models"`
	Streaming bool        `json:"streaming"`
	// SignIn is "offer" (the ⎆ chip belongs under the transcript),
	// "starting", "waiting" (Code and URL are set), or "".
	SignIn    string `json:"signIn"`
	Code      string `json:"code,omitempty"`
	URL       string `json:"url,omitempty"`
	ArchiveID string `json:"archiveId,omitempty"` // the live conversation's file, once saved
}

func (a *assistant) viewLocked() chatView {
	v := chatView{State: a.state, Agent: infoOf(a.agentLocked()), Model: a.modelNameLocked(),
		ModelID: a.modelID, Models: []modelInfo{}, Streaming: a.streaming,
		SignIn: a.signState, Code: a.signCode, URL: a.signURL, ArchiveID: a.archiveID}
	for _, m := range a.models {
		v.Models = append(v.Models, modelInfo{ID: m.ID, Name: m.Name, Usage: m.Usage})
	}
	if a.offerSignInLocked() {
		v.SignIn = "offer"
	}
	return v
}

// chatSnapshot is GET …/chat: everything the pane draws.
type chatSnapshot struct {
	View   chatView    `json:"view"`
	Msgs   []chatLine  `json:"msgs"`
	Agents []agentInfo `json:"agents"`
}

func (a *assistant) snapshot() chatSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := chatSnapshot{View: a.viewLocked(), Msgs: slices.Clone(a.msgs), Agents: []agentInfo{}}
	if s.Msgs == nil {
		s.Msgs = []chatLine{}
	}
	for _, ag := range ai.Agents() {
		s.Agents = append(s.Agents, infoOf(ag))
	}
	return s
}

func (a *assistant) sendStateLocked() { a.t.send("chat.state", a.viewLocked()) }

func (a *assistant) addLocked(role, text string) {
	a.msgs = append(a.msgs, chatLine{Role: role, Text: text})
	a.t.send("chat.msg", map[string]any{"i": len(a.msgs) - 1, "role": role, "text": text})
}

// appendAgentLocked adds a streamed chunk to the answer being written,
// starting one if the last message is not an open answer.
func (a *assistant) appendAgentLocked(text string) {
	if n := len(a.msgs); n > 0 && a.msgs[n-1].Role == "agent" && a.streaming {
		a.msgs[n-1].Text += text
		a.t.send("chat.text", map[string]any{"i": n - 1, "text": text})
		return
	}
	a.addLocked("agent", text)
}

// agentLocked is the backend this conversation runs, or the configured one
// before it has started.
func (a *assistant) agentLocked() ai.Agent {
	if a.agent.ID != "" {
		return a.agent
	}
	ag, _ := ai.AgentByID(a.srv.cfg.AIAgent)
	return ag
}

func (a *assistant) modelNameLocked() string {
	for _, m := range a.models {
		if m.ID == a.modelID {
			return m.Name
		}
	}
	return a.modelID
}

// ---------------------------------------------------------------------------
// Starting and the event pump
// ---------------------------------------------------------------------------

// open is the pane opening: the agent starts if it is not running.
func (a *assistant) open() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ensureLocked()
	a.sendStateLocked()
}

// ensureLocked starts the agent unless one is starting or running.
func (a *assistant) ensureLocked() {
	if a.c != nil && (a.state == chatStarting || a.state == chatReady) {
		return
	}
	if a.agent.ID == "" {
		a.agent = a.agentLocked()
		if _, ok := ai.AgentByID(a.srv.cfg.AIAgent); !ok && a.srv.cfg.AIAgent != "" {
			a.addLocked("info", fmt.Sprintf("unknown ai_agent %q — using %s", a.srv.cfg.AIAgent, a.agent.Name))
		}
	}
	a.gen++
	a.state = chatStarting
	a.needAuth = false // a refusal of this connection sets it again
	a.c = a.srv.opt.StartChat(a.agent, ai.Options{Dir: agentDir(), Model: a.srv.cfg.AIModel})
	go a.pump(a.c, a.gen)
}

// agentDir is the agent's working directory: dbc's own, as in the TUI.
func agentDir() string {
	if dir, err := os.Getwd(); err == nil {
		return dir
	}
	return os.TempDir()
}

// pump delivers one connection's events until it exits or is closed.
func (a *assistant) pump(c *ai.Chat, gen int) {
	for {
		select {
		case e := <-c.Events():
			a.onEvent(gen, e)
			if e.Kind == ai.EventExit {
				return
			}
		case <-c.Done():
			return
		}
	}
}

// onEvent lands one event from the agent — tui.(*Model).chatEvent's rules.
func (a *assistant) onEvent(gen int, e ai.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen || a.c == nil {
		return // an earlier connection's straggler
	}
	switch e.Kind {
	case ai.EventReady:
		a.state, a.models, a.modelID = chatReady, e.Models, e.ModelID
		a.needAuth = false
		if a.pending != "" {
			prompt := a.pending
			a.pending = ""
			a.sendLocked(prompt)
		}
	case ai.EventText:
		a.appendAgentLocked(e.Text)
		return // a chunk changes no chrome; no state event per chunk
	case ai.EventTool:
		a.addLocked("tool", "⚙ "+e.Text)
	case ai.EventNote:
		a.addLocked("note", e.Text)
	case ai.EventTurnDone:
		// streaming=false is also what makes the next chunk start a new
		// message rather than extend this answer
		a.streaming = false
		switch {
		case e.Err != nil:
			a.addLocked("error", e.Err.Error())
			// a credential revoked mid-conversation refuses the turn, not
			// the handshake; the ⎆ chip is offered the same way
			a.needAuth = errors.Is(e.Err, ai.ErrAuthRequired)
		case e.StopReason == "cancelled":
			a.addLocked("info", "— stopped")
		}
		a.saveLocked()
	case ai.EventExit:
		a.streaming = false
		a.state = chatDead
		if e.Err != nil {
			a.addLocked("error", e.Err.Error())
			a.needAuth = errors.Is(e.Err, ai.ErrAuthRequired)
			// When dbc can sign in, the ⎆ chip is the next step and a
			// question already asked waits for it; ⟲ new would only be
			// refused again.
			switch {
			case !a.offerSignInLocked():
				a.addLocked("info", "click ⟲ new to try again")
			case a.pending != "":
				a.addLocked("info", "your question goes out once you are signed in")
			}
		}
		a.saveLocked() // an answer cut off by the exit is still worth keeping
	}
	a.sendStateLocked()
}

// ---------------------------------------------------------------------------
// Asking
// ---------------------------------------------------------------------------

// errStillAnswering is the refusal for a second question mid-answer: the
// TUI's words, a 409 like a busy run.
func errStillAnswering() error {
	return conflict("the assistant is still answering — Ctrl+K (or ■ stop) stops it")
}

// ask starts a turn. ctx and refs are the context gathered for it (empty
// when the user turned context off).
func (a *assistant) ask(q string, ctx ai.Context, refs []db.TableRef) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.streaming {
		return errStillAnswering()
	}
	a.addLocked("user", q)
	// ensureLocked may start the agent, which bumps the generation; the
	// lookup is tagged with the generation it will be sent on
	a.ensureLocked()
	if len(refs) == 0 {
		a.finishLocked(q, ctx)
		return nil
	}
	a.streaming = true // in flight from here, lookup included
	a.sendStateLocked()
	gen, ws := a.gen, a.t.ws
	go func() {
		cols, err := ws.LookupColumns(refs)
		a.schemaLanded(gen, q, ctx, cols, err)
	}()
	return nil
}

// schemaLanded sends the question a schema lookup was for.
func (a *assistant) schemaLanded(gen int, q string, ctx ai.Context, cols [][]db.Column, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen {
		return // ⟲ new or an agent switch since: that conversation is gone
	}
	ctx, failed := workspace.AttachColumns(ctx, cols, err)
	if failed != "" {
		a.addLocked("info", failed)
	}
	if a.state == chatDead {
		// the agent exited while the lookup ran; its exit is already in
		// the transcript, and ⟲ new starts over
		a.streaming = false
		if a.offerSignInLocked() {
			// ...unless it was refused for lack of sign-in: then the
			// question waits for the sign-in (finishLocked queues it)
			a.finishLocked(q, ctx)
			a.streaming = false
			a.addLocked("info", "your question goes out once you are signed in")
		}
		a.sendStateLocked()
		return
	}
	a.finishLocked(q, ctx)
}

// finishLocked builds the prompt and hands it to the agent, or queues it
// until the handshake completes.
func (a *assistant) finishLocked(q string, ctx ai.Context) {
	prompt := ai.Build(q, ctx, a.first)
	a.first = false
	a.addLocked("note", "▤ "+prompt.Note)
	if a.state != chatReady {
		a.pending = prompt.Text
		a.streaming = true
		a.sendStateLocked()
		return
	}
	a.sendLocked(prompt.Text)
}

// sendLocked hands a prompt to the agent.
func (a *assistant) sendLocked(text string) {
	if err := a.c.Send(text); err != nil {
		a.streaming = false
		if errors.Is(err, ai.ErrBusy) {
			a.addLocked("info", "still answering the last question")
		} else {
			a.addLocked("error", err.Error())
		}
	} else {
		a.streaming = true
	}
	a.sendStateLocked()
}

// stop asks the agent to stop the answer in flight.
func (a *assistant) stop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.c != nil && a.c.Cancel() {
		a.addLocked("info", "— stopping…")
		return true
	}
	return false
}

// setModel switches the model. It blocks for the agent's round trip; the
// handler that calls it is a request of its own, so nothing else waits.
func (a *assistant) setModel(id string) error {
	a.mu.Lock()
	c, gen := a.c, a.gen
	known := slices.ContainsFunc(a.models, func(m ai.Model) bool { return m.ID == id })
	a.mu.Unlock()
	if c == nil || !known {
		return badRequest("the assistant does not offer model %q", id)
	}
	err := c.SetModel(id)
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen {
		return nil // the conversation was replaced meanwhile
	}
	if err != nil {
		a.addLocked("error", "could not switch model: "+err.Error())
	} else {
		a.modelID = id
		a.addLocked("info", "model: "+a.modelNameLocked())
	}
	a.sendStateLocked()
	return nil
}

// ---------------------------------------------------------------------------
// New, switch, the archive
// ---------------------------------------------------------------------------

// newChat saves the conversation and starts a fresh one — which also
// resets the agent's memory of it, since that lives in its session.
// Reports whether the old one was saved (for the log line).
func (a *assistant) newChat() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	saved := a.saveLocked()
	a.resetLocked()
	return saved
}

// switchAgent starts a new conversation with a different backend.
func (a *assistant) switchAgent(ag ai.Agent) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ag.ID != a.agentLocked().ID {
		a.endSignInLocked() // a sign-in belongs to the agent being left
	}
	saved := a.saveLocked()
	a.agent = ag
	a.resetLocked()
	return saved
}

// resetLocked clears the transcript and starts a fresh agent session,
// WITHOUT saving; callers save first when they mean to. Forgetting the
// archive id is what keeps a later save from writing over the old file.
//
// A sign-in in flight survives it: the flow runs its own process, and a
// user who clicks ⟲ new while typing the code still wants to be signed in.
func (a *assistant) resetLocked() {
	if a.c != nil {
		a.c.Close()
	}
	a.c, a.state, a.streaming, a.pending = nil, chatIdle, false, ""
	a.msgs, a.first, a.models, a.modelID = nil, true, nil, ""
	a.archiveID, a.archiveStart = "", time.Time{}
	a.ensureLocked()
	a.t.send("chat.reset", nil)
	a.sendStateLocked()
}

// worthSavingLocked: a transcript with no question in it — the pane opened,
// nothing asked — would only fill the recent list with rows nobody could
// tell apart.
func (a *assistant) worthSavingLocked() bool {
	return slices.ContainsFunc(a.msgs, func(m chatLine) bool { return m.Role == "user" })
}

// saveLocked writes the live conversation to the archive, under the same
// id all conversation long. A failure is logged once, not after every
// answer. Reports whether the conversation is now on disk.
func (a *assistant) saveLocked() bool {
	dir := a.srv.opt.ChatsDir
	if dir == "" || !a.worthSavingLocked() {
		return false
	}
	now := time.Now()
	if a.archiveID == "" {
		a.archiveID, a.archiveStart = userdata.NewChatID(now), now
	}
	msgs := make([]userdata.ChatMsg, len(a.msgs))
	for i, m := range a.msgs {
		msgs[i] = userdata.ChatMsg{Role: m.Role, Text: m.Text}
	}
	err := userdata.SaveChat(dir, userdata.Chat{
		ID: a.archiveID, Title: userdata.ChatTitle(msgs),
		Agent: a.agentLocked().Name, Model: a.modelNameLocked(), Conn: a.t.ws.Active(),
		Started: a.archiveStart, Updated: now, Msgs: msgs,
	})
	switch {
	case err == nil:
		a.saveErr = ""
		return true
	case err.Error() != a.saveErr:
		a.saveErr = err.Error()
		a.t.send("log", logLine{Level: "warn", Text: "could not save the assistant conversation: " + err.Error()})
	}
	return false
}

// openSaved puts the live conversation away (saved) and loads a saved one
// as a transcript, in a fresh agent session — continuing to talk to a
// session that remembers the conversation being replaced is the confusing
// failure, and an invisible one.
func (a *assistant) openSaved(id string) error {
	dir := a.srv.opt.ChatsDir
	if dir == "" {
		return badRequest("conversations are not kept in this dbc web (no chats directory)")
	}
	c, err := userdata.LoadChat(dir, id)
	if err != nil {
		return badRequest("could not open that conversation: %v", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saveLocked()
	a.resetLocked()
	for _, m := range c.Msgs {
		a.msgs = append(a.msgs, chatLine{Role: m.Role, Text: m.Text})
	}
	a.archiveID, a.archiveStart = c.ID, c.Started
	if a.archiveStart.IsZero() {
		a.archiveStart = c.Updated
	}
	a.msgs = append(a.msgs, chatLine{Role: "info", Text: c.RestoreNote()})
	// one reset for the whole transcript rather than an event per message
	a.t.send("chat.reset", nil)
	a.sendStateLocked()
	return nil
}

// deleteLive deletes the conversation on screen: its file, if saved, then
// the transcript, cleared WITHOUT the save newChat would do — that save is
// exactly what would write the file back. A file that will not delete
// leaves the pane untouched.
func (a *assistant) deleteLive() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.msgs) == 0 && a.archiveID == "" {
		return badRequest("no conversation to delete")
	}
	if a.archiveID != "" && a.srv.opt.ChatsDir != "" {
		if err := userdata.RemoveChat(a.srv.opt.ChatsDir, a.archiveID); err != nil {
			return badRequest("could not delete the conversation: %v", err)
		}
	}
	a.resetLocked()
	return nil
}

// liveID is the live conversation's archive id ("" before its first save).
func (a *assistant) liveID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.archiveID
}

// close ends the conversation for good — the tab is forgotten, or dbc web
// is stopping — saving it first, as quitting the TUI does.
func (a *assistant) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saveLocked()
	if a.c != nil {
		a.c.Close()
	}
	a.endSignInLocked()
}

// ---------------------------------------------------------------------------
// Sign-in (tui/chatsignin.go's flow; ai/signin.go for why the device flow)
// ---------------------------------------------------------------------------

// offerSignInLocked: the agent refused for lack of sign-in, dbc can sign
// in to it, and no sign-in is under way.
func (a *assistant) offerSignInLocked() bool {
	return a.needAuth && a.signState == signNone && a.agentLocked().CanSignIn()
}

// startSignIn begins GitHub's device flow. The code goes in the transcript
// — the wait can take minutes and the user will look back for it — and in
// the state, for the ⧉ copy code and ↗ open page chips. Unlike the TUI, the
// server neither copies it nor opens a browser: the page is the browser,
// and a clipboard write needs the user's own click to count.
func (a *assistant) startSignIn() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.signState != signNone {
		if a.signCode != "" {
			a.addLocked("info", "sign-in is waiting on GitHub — enter "+a.signCode+" at "+a.signURL)
		}
		return nil
	}
	ag := a.agentLocked()
	if !ag.CanSignIn() {
		return badRequest("dbc cannot sign in to %s — %s", ag.Name, ag.Auth)
	}
	a.signSeq++
	a.signState = signStarting
	a.addLocked("info", "starting GitHub sign-in…")
	a.sendStateLocked()
	seq, begin := a.signSeq, a.srv.opt.BeginSignIn
	go func() {
		s, err := begin(ag, agentDir())
		a.signInCode(seq, s, err)
	}()
	return nil
}

// signInCode lands the device code — or the news that none is needed.
func (a *assistant) signInCode(seq int, s *ai.SignIn, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq != a.signSeq || a.signState != signStarting {
		if s != nil {
			s.Close() // an attempt cancelled while it started
		}
		return
	}
	if err != nil {
		a.signState = signNone
		a.addLocked("error", "sign-in failed: "+err.Error())
		a.sendStateLocked()
		return
	}
	if s.UserCode == "" { // signed in all along — elsewhere, since the refusal
		a.signedInLocked(s.Status)
		return
	}
	a.signIn, a.signState = s, signWaiting
	a.signCode, a.signURL = s.UserCode, s.VerificationURI
	a.addLocked("info", "enter code "+a.signCode+" at "+a.signURL+
		" — ⧉ copy code and ↗ open page below. Waiting for GitHub…")
	a.sendStateLocked()
	go func() {
		st, err := s.Wait()
		a.signInDone(seq, st, err)
	}()
}

// signInDone lands the end of the device flow.
func (a *assistant) signInDone(seq int, st ai.AuthStatus, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq != a.signSeq || a.signState != signWaiting {
		return // cancelled; the cancel already said so
	}
	a.endSignInLocked()
	switch {
	case err != nil:
		a.addLocked("error", "sign-in did not complete: "+err.Error())
	case st.SignedIn():
		a.signedInLocked(st)
		return
	case st.NoSubscription():
		a.addLocked("error", "signed in to GitHub"+asUser(st.User)+", but that account has no Copilot subscription")
	default:
		a.addLocked("error", "sign-in did not complete ("+st.Status+")")
	}
	a.sendStateLocked()
}

// signedInLocked reports success and reconnects when the chat is down; a
// question refused along with the handshake is still pending and goes out
// on EventReady.
func (a *assistant) signedInLocked(st ai.AuthStatus) {
	a.endSignInLocked()
	a.needAuth = false
	if a.state == chatReady || a.state == chatStarting {
		a.addLocked("info", "signed in to GitHub"+asUser(st.User))
	} else {
		a.addLocked("info", "signed in to GitHub"+asUser(st.User)+" — connecting…")
		a.ensureLocked()
	}
	a.sendStateLocked()
}

// cancelSignIn abandons a sign-in (the ✕ cancel chip).
func (a *assistant) cancelSignIn() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.signState == signNone {
		return
	}
	a.endSignInLocked()
	a.addLocked("info", "sign-in cancelled")
	a.sendStateLocked()
}

// endSignInLocked ends any sign-in in flight and forgets it; bumping the
// seq is what makes a late answer from it land as a no-op.
func (a *assistant) endSignInLocked() {
	if a.signIn != nil {
		a.signIn.Close()
	}
	a.signSeq++
	a.signIn, a.signState, a.signCode, a.signURL = nil, signNone, "", ""
}

func asUser(user string) string {
	if user == "" {
		return ""
	}
	return " as " + user
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// chatReq is what the page sends with a question, and with a request for
// the context chip's forecast: the editor and the grid's view, from which
// workspace.ChatContext decides what goes.
type chatReq struct {
	Question string    `json:"question"`
	Attach   bool      `json:"attach"` // the context chip is on
	Editor   runReq    `json:"editor"`
	View     *chatGrid `json:"view"`
}

// chatGrid is the grid's view of the result it shows: the result's seq,
// its header sort, and the result columns hidden in it.
type chatGrid struct {
	Seq    int   `json:"seq"`
	Sort   int   `json:"sort"`
	Desc   bool  `json:"desc"`
	Hidden []int `json:"hidden"`
}

// chatContext gathers what a question may carry. The grid's view only
// counts when it describes the result the workspace holds (its seq is
// current): its hidden columns are indices into THAT result's columns.
//
// A stale view with hidden columns is refused rather than dropped: sending
// the question without them would send the very columns the user hid, and
// hiding is how a user keeps a column away from the model. The page, on
// the 409, reloads its grid and asks again.
func (s *Server) chatContext(t *tab, req chatReq) (ai.Context, []db.TableRef, error) {
	ed := workspace.Editor{Text: req.Editor.Buffer,
		Caret: byteOffset(req.Editor.Buffer, req.Editor.Caret), Selection: req.Editor.Selection}
	view := workspace.GridView{SortCol: -1}
	if g := req.View; g != nil && g.Seq > 0 {
		v := t.view(g.Sort, g.Desc)
		switch {
		case v != nil && v.seq == g.Seq:
			view = workspace.GridView{Result: v.res, Hidden: cleanHidden(g.Hidden, len(v.res.Columns)),
				SortCol: v.sortCol, SortDesc: v.desc, Order: v.order}
		case len(g.Hidden) > 0:
			return ai.Context{}, nil, conflict("the result changed while you were asking — the grid is reloading; ask again")
		}
	}
	ctx, refs := t.ws.ChatContext(req.Question, ed, view)
	return ctx, refs, nil
}

// cleanHidden makes a page's hidden-column list what ai.Context promises:
// in range, ascending, no repeats.
func cleanHidden(h []int, ncols int) []int {
	out := make([]int, 0, len(h))
	for _, c := range h {
		if c >= 0 && c < ncols {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// tabChat finds the tab and its assistant.
func (s *Server) tabChat(ctx rweb.Context) (*tab, *assistant, error) {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return nil, nil, err
	}
	return t, t.chat, nil
}

func (s *Server) handleChat(ctx rweb.Context) error {
	_, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, a.snapshot())
}

// handleChatOpen is the pane opening: the agent starts, lazily, now.
func (s *Server) handleChatOpen(ctx rweb.Context) error {
	_, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	a.open()
	return ok(ctx, a.snapshot())
}

// maxQuestion caps a question. A question is typed; a pasted dump is a
// mistake that would go to a hosted model and cost the user tokens.
const maxQuestion = 64 << 10

// handleChatAsk sends a question. It answers once the turn has started (or
// queued behind the handshake); the answer streams on the tab's stream.
func (s *Server) handleChatAsk(ctx rweb.Context) error {
	t, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req chatReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	q := strings.TrimSpace(req.Question)
	switch {
	case q == "":
		return fail(ctx, badRequest("ask something first"))
	case len(q) > maxQuestion:
		return fail(ctx, badRequest("that question is %d KB; the assistant takes up to %d KB", len(q)>>10, maxQuestion>>10))
	}
	var c ai.Context
	var refs []db.TableRef
	if req.Attach {
		if c, refs, err = s.chatContext(t, req); err != nil {
			return fail(ctx, err)
		}
	}
	if err = a.ask(q, c, refs); err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, nil)
}

// handleChatContext is the context chip's forecast: what the next question
// would carry, in the words the transcript uses once it is sent. The schema
// is named but not looked up — that costs a catalog query, paid only when
// the question goes.
func (s *Server) handleChatContext(ctx rweb.Context) error {
	t, _, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req chatReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	c, _, err := s.chatContext(t, req)
	if err != nil {
		return fail(ctx, err)
	}
	note := ai.Build("", c, false).Note
	if len(note) > len("sent: ") {
		note = note[len("sent: "):]
	}
	return ok(ctx, map[string]string{"note": note})
}

func (s *Server) handleChatStop(ctx rweb.Context) error {
	t, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	if !a.stop() {
		t.notes([]workspace.Note{{Level: workspace.Warn, Text: "the assistant is not answering"}})
	}
	return ok(ctx, nil)
}

func (s *Server) handleChatNew(ctx rweb.Context) error {
	t, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	if a.newChat() {
		t.send("log", logLine{Level: "info", Text: "saved the assistant conversation — reopen it from Recent conversations"})
	}
	return ok(ctx, nil)
}

type idReq struct {
	ID string `json:"id"`
}

func (s *Server) handleChatModel(ctx rweb.Context) error {
	_, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req idReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if err = a.setModel(req.ID); err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, nil)
}

// handleChatAgent switches to another backend: a new conversation.
func (s *Server) handleChatAgent(ctx rweb.Context) error {
	t, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req idReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	var ag ai.Agent
	for _, x := range ai.Agents() {
		if x.ID == req.ID {
			ag = x
		}
	}
	if ag.ID == "" {
		return fail(ctx, badRequest("unknown assistant %q", req.ID))
	}
	if !ag.Installed() {
		return fail(ctx, badRequest("%s is not installed — %s", ag.Name, ag.Install))
	}
	if a.switchAgent(ag) {
		t.send("log", logLine{Level: "info", Text: "saved the assistant conversation — reopen it from Recent conversations"})
	}
	return ok(ctx, nil)
}

func (s *Server) handleChatSignIn(ctx rweb.Context) error {
	_, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	if err = a.startSignIn(); err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, nil)
}

func (s *Server) handleChatSignInCancel(ctx rweb.Context) error {
	_, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	a.cancelSignIn()
	return ok(ctx, nil)
}

// handleChatLoad reopens a saved conversation in this tab's pane.
func (s *Server) handleChatLoad(ctx rweb.Context) error {
	_, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req idReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if err = a.openSaved(req.ID); err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, nil)
}

// handleChatDeleteLive is the transcript menu's "Delete this conversation".
func (s *Server) handleChatDeleteLive(ctx rweb.Context) error {
	t, a, err := s.tabChat(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	if err = a.deleteLive(); err != nil {
		return fail(ctx, err)
	}
	t.send("log", logLine{Level: "ok", Text: "deleted this conversation"})
	return ok(ctx, nil)
}

// savedChat is a row of the Recent conversations list.
type savedChat struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Agent   string    `json:"agent,omitempty"`
	Conn    string    `json:"conn,omitempty"`
	Updated time.Time `json:"updated"`
	Count   int       `json:"count"`
}

// handleChats lists the saved conversations — the TUI's archive, so a
// conversation had in either is offered in both. ?ws= leaves that tab's
// live conversation out: reopening what is already on screen would be a
// no-op at best, and at worst reload it from a copy one answer stale.
func (s *Server) handleChats(ctx rweb.Context) error {
	out := []savedChat{}
	dir := s.opt.ChatsDir
	if dir == "" {
		return ok(ctx, out)
	}
	metas, err := userdata.ListChats(dir)
	if err != nil {
		return fail(ctx, err)
	}
	live := ""
	if id := ctx.Request().QueryParam("ws"); id != "" {
		if t, err := s.hub.get(id); err == nil {
			live = t.chat.liveID()
		}
	}
	for _, m := range metas {
		if m.ID == live {
			continue
		}
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		out = append(out, savedChat{ID: m.ID, Title: title, Agent: m.Agent, Conn: m.Conn, Updated: m.Updated, Count: m.Count})
	}
	return ok(ctx, out)
}

// handleChatDelete removes a saved conversation for good. The page asks
// for it only from a named menu row — the one destructive gesture here is
// never a single stray click. A tab's live conversation is refused: its
// next save would write it straight back.
func (s *Server) handleChatDelete(ctx rweb.Context) error {
	dir := s.opt.ChatsDir
	id := ctx.Request().PathParam("id")
	if dir == "" {
		return fail(ctx, badRequest("conversations are not kept in this dbc web"))
	}
	if s.hub.liveChat(id) {
		return fail(ctx, badRequest("that conversation is open in an assistant pane — use Delete this conversation there"))
	}
	if err := userdata.RemoveChat(dir, id); err != nil {
		return fail(ctx, badRequest("could not delete the conversation: %v", err))
	}
	return ok(ctx, nil)
}
