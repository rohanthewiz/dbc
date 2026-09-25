package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// A Chat is one conversation with one agent process.
//
// LIFECYCLE, as the ACP spec defines it and ced proved it:
//
//	Start ──spawn──► initialize ──► session/new ──► EventReady{models}
//	                                                  │
//	Send ──session/prompt (blocks for the whole turn)─┤◄─ session/update … EventText/EventTool
//	                                                  │
//	Cancel ──session/cancel (notification)────────────┤   the blocked prompt returns
//	                                                  ▼   stopReason "cancelled"
//	                                            EventTurnDone
//	Close ──► stdin closed, process reaped         (or EventExit if it died)
//
// Everything the agent says or does arrives as an Event on one channel, in
// order. The UI owns rendering and never touches the connection except
// through these methods.
//
// WHAT dbc LETS THE AGENT DO: NOTHING BUT TALK. The client advertises no
// filesystem capability, and every permission request — to run a command, to
// edit a file — is declined with the agent's own reject option and reported
// as an EventNote. A database client handing an agent a shell in the user's
// working directory would be a surprising thing for "explain this query" to
// do. The agent still has its own built-in knowledge and whatever context
// dbc sends; that is what a SQL assistant needs.

// Timeouts, each explained by what it waits on.
const (
	// initTimeout covers a cold start, which can block on a macOS Keychain
	// prompt the first time the agent reads its credential.
	initTimeout = 2 * time.Minute
	// sessionTimeout covers session/new and session/set_model: quick calls,
	// but the first one may be fetching the model roster over the network.
	sessionTimeout = 30 * time.Second
	// turnTimeout bounds one answer. A long streamed answer can take minutes;
	// past this the agent is wedged, not thinking.
	turnTimeout = 15 * time.Minute
)

// EventKind says what an Event carries.
type EventKind int

const (
	// EventReady: the handshake finished and Send may be called. Models and
	// ModelID describe the roster the agent offers.
	EventReady EventKind = iota
	// EventText: the next chunk of the agent's answer. Chunks of one answer
	// arrive in order; the UI appends them.
	EventText
	// EventTool: the agent started a tool call; Text is its title.
	EventTool
	// EventNote: something dbc did on the user's behalf that belongs in the
	// transcript, e.g. declining a permission request.
	EventNote
	// EventTurnDone: the answer to one Send is complete. StopReason is the
	// agent's ("end_turn", "cancelled", …); Err is set when the turn failed.
	EventTurnDone
	// EventExit: the connection is gone — the handshake failed or the process
	// died. Err says why (nil after a deliberate Close). No events follow.
	EventExit
)

// Event is one thing that happened in a conversation.
type Event struct {
	Kind       EventKind
	Text       string
	Models     []Model
	ModelID    string
	StopReason string
	Err        error
}

// Model is one entry of the agent's model roster.
type Model struct {
	ID    string
	Name  string
	Usage string // Copilot's premium-request multiplier ("1x", "0x"), if given
}

// Options configure a new Chat.
type Options struct {
	// Dir is the agent's working directory and the ACP session's cwd. It must
	// be absolute; the spec requires it.
	Dir string
	// Model is the preferred model id. Empty, or an id the roster no longer
	// offers, keeps the agent's default — a stale saved preference must never
	// break the handshake.
	Model string
}

// dialFunc opens the transport. Start uses the real process; tests hand in a
// connection over pipes to a fake agent.
type dialFunc func(onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, error),
	onExit func(error)) (*rpcConn, error)

// Chat is one live conversation. All methods are safe from any goroutine.
type Chat struct {
	agent  Agent
	events chan Event
	done   chan struct{} // closed by Close; stops event delivery
	once   sync.Once

	mu         sync.Mutex
	conn       *rpcConn
	sessionID  string
	ready      bool
	turn       bool // a session/prompt is in flight
	cancelSent bool // one session/cancel per turn is enough
}

// ErrNotReady is returned by Send before EventReady, and after EventExit.
var ErrNotReady = errors.New("the assistant is not connected yet")

// ErrBusy is returned by Send while an answer is still streaming.
var ErrBusy = errors.New("the assistant is still answering — stop it first")

// Start launches the agent and runs the handshake in the background. It
// returns at once; EventReady or EventExit reports how it went.
//
// A missing binary is reported as EventExit with an error that carries the
// install hint, rather than as a failure of Start itself, so the UI has one
// path for "the assistant is unavailable" however it came about.
func Start(agent Agent, opt Options) *Chat {
	dial := func(onNotify func(string, json.RawMessage),
		onRequest func(string, json.RawMessage) (any, error),
		onExit func(error)) (*rpcConn, error) {
		if !agent.Installed() {
			return nil, fmt.Errorf("%s is not installed (%s not on PATH) — install it with: %s",
				agent.Name, agent.Binary, agent.Install)
		}
		return startRPC(opt.Dir, agent.Binary, agent.Args, onNotify, onRequest, onExit)
	}
	return startWith(agent, opt, dial)
}

// StartPipes runs a conversation over an existing connection to an agent —
// r is what the agent writes, w is what it reads — instead of spawning a
// process. It serves an agent reached some other way (a socket, a
// container's stdio) and is how tests drive the Chat against a scripted one
// (see package aitest). Closing the Chat closes w.
func StartPipes(agent Agent, opt Options, r io.Reader, w io.Writer) *Chat {
	return startWith(agent, opt, func(onNotify func(string, json.RawMessage),
		onRequest func(string, json.RawMessage) (any, error),
		onExit func(error)) (*rpcConn, error) {
		return newRPCConn(r, w, onNotify, onRequest, onExit), nil
	})
}

// startWith is Start with the transport injected.
func startWith(agent Agent, opt Options, dial dialFunc) *Chat {
	c := &Chat{
		agent: agent,
		// Buffered so a burst of streamed chunks does not stall the agent's
		// read loop while the UI is mid-frame. Delivery still blocks when it
		// is full — dropping a chunk would corrupt the answer — until the UI
		// catches up or the Chat is closed.
		events: make(chan Event, 256),
		done:   make(chan struct{}),
	}
	go c.connect(opt, dial)
	return c
}

// Agent returns the backend this chat runs.
func (c *Chat) Agent() Agent { return c.agent }

// Events is the conversation's event stream. It is never closed: after
// EventExit nothing more is sent, and a reader that stops reading after Close
// leaks nothing, because delivery gives up once Close has run.
func (c *Chat) Events() <-chan Event { return c.events }

// Done is closed by Close. A reader waiting on Events selects on it too, so
// it stops waiting when the conversation it was reading is gone rather than
// blocking forever on a channel nothing will write to again.
func (c *Chat) Done() <-chan struct{} { return c.done }

// emit delivers an event unless the chat has been closed.
func (c *Chat) emit(e Event) {
	select {
	case c.events <- e:
	case <-c.done:
	}
}

// connect spawns the agent and runs initialize and session/new.
func (c *Chat) connect(opt Options, dial dialFunc) {
	onNotify := func(method string, params json.RawMessage) {
		if method == "session/update" {
			c.handleUpdate(params)
		}
	}
	onExit := func(err error) {
		c.mu.Lock()
		c.ready, c.turn = false, false
		c.mu.Unlock()
		if err == nil {
			select {
			case <-c.done:
				// a deliberate Close; the UI already knows
			default:
				err = fmt.Errorf("%s exited", c.agent.Name)
			}
		}
		c.emit(Event{Kind: EventExit, Err: err})
	}
	conn, err := dial(onNotify, c.handleRequest, onExit)
	if err != nil {
		c.emit(Event{Kind: EventExit, Err: err})
		return
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	// Close may have run while the process was starting; it could not reach
	// a connection that did not exist yet, so finish its job here.
	select {
	case <-c.done:
		conn.close()
		return
	default:
	}

	sess, err := handshake(conn, opt)
	if err != nil {
		conn.close()
		c.emit(Event{Kind: EventExit, Err: c.explain(err)})
		return
	}
	c.mu.Lock()
	c.sessionID, c.ready = sess.id, true
	c.mu.Unlock()
	c.emit(Event{Kind: EventReady, Models: sess.models, ModelID: sess.modelID})
}

// ErrAuthRequired matches (errors.Is) an EventExit or EventTurnDone error
// that the agent refused for lack of sign-in. A UI uses it to offer the
// agent's sign-in — for an agent that CanSignIn, one dbc runs itself.
var ErrAuthRequired = errors.New("sign-in required")

// authError is an agent error recognised as missing auth: it reads as the
// agent's words plus its sign-in hint, and matches both the original error
// and ErrAuthRequired.
type authError struct {
	err  error
	hint string
}

func (e *authError) Error() string   { return e.err.Error() + " — " + e.hint }
func (e *authError) Unwrap() []error { return []error{e.err, ErrAuthRequired} }

// explain adds the agent's sign-in hint to an error that looks like missing
// auth. The agents word it variously; these are the phrasings seen so far
// (Copilot's is ACP's "Authentication required", code -32000).
func (c *Chat) explain(err error) error {
	msg := strings.ToLower(err.Error())
	for _, k := range []string{"auth", "sign in", "signin", "not signed", "login", "unauthorized", "401"} {
		if strings.Contains(msg, k) {
			return &authError{err: err, hint: c.agent.Auth}
		}
	}
	return err
}

// session is what the handshake learned.
type session struct {
	id      string
	modelID string
	models  []Model
}

// handshake runs initialize and session/new, then applies the preferred
// model if the roster offers it.
func handshake(conn *rpcConn, opt Options) (session, error) {
	initParams := map[string]any{
		"protocolVersion": 1,
		// No fs capability: the agent is told up front it cannot read or
		// write files through dbc, so it answers in prose and SQL instead of
		// planning edits that would be refused.
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false},
		},
	}
	if err := conn.call("initialize", initParams, nil, initTimeout); err != nil {
		return session{}, err
	}

	var resp struct {
		SessionID string `json:"sessionId"`
		Models    struct {
			Available []struct {
				ModelID string `json:"modelId"`
				Name    string `json:"name"`
				Meta    struct {
					Usage string `json:"copilotUsage"`
				} `json:"_meta"`
			} `json:"availableModels"`
			CurrentModelID string `json:"currentModelId"`
		} `json:"models"`
	}
	// mcpServers is required and must be an array, never null.
	err := conn.call("session/new",
		map[string]any{"cwd": opt.Dir, "mcpServers": []any{}}, &resp, sessionTimeout)
	if err != nil {
		return session{}, err
	}
	if resp.SessionID == "" {
		return session{}, errors.New("the agent returned no session id")
	}
	s := session{id: resp.SessionID, modelID: resp.Models.CurrentModelID}
	for _, m := range resp.Models.Available {
		if m.ModelID != "" {
			s.models = append(s.models, Model{ID: m.ModelID, Name: m.Name, Usage: m.Meta.Usage})
		}
	}
	if opt.Model != "" && opt.Model != s.modelID && hasModel(s.models, opt.Model) {
		if err = conn.call("session/set_model",
			map[string]any{"sessionId": s.id, "modelId": opt.Model}, nil, sessionTimeout); err == nil {
			s.modelID = opt.Model
		}
	}
	return s, nil
}

func hasModel(ms []Model, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// handleUpdate turns one session/update notification into events. Answer
// text and tool calls are kept; the agent's thoughts and plans are dropped —
// a side pane has no room for its inner monologue, and the answer is what
// the user asked for.
func (c *Chat) handleUpdate(params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string          `json:"sessionUpdate"`
			Content json.RawMessage `json:"content"`
			Title   string          `json:"title"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	c.mu.Lock()
	current := p.SessionID == c.sessionID
	c.mu.Unlock()
	if !current {
		return // an earlier session's stragglers
	}
	switch p.Update.Kind {
	case "agent_message_chunk":
		if text := contentText(p.Update.Content); text != "" {
			c.emit(Event{Kind: EventText, Text: text})
		}
	case "tool_call":
		title := p.Update.Title
		if title == "" {
			title = "tool call"
		}
		c.emit(Event{Kind: EventTool, Text: title})
	}
}

// contentText extracts the text of an ACP ContentBlock; other block types
// (images, resources) yield "".
func contentText(raw json.RawMessage) string {
	var b struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &b) != nil || b.Type != "text" {
		return ""
	}
	return b.Text
}

// handleRequest answers the agent's requests to dbc. Permission requests are
// declined — see the note on Chat — and everything else (fs, terminal) is
// refused as unsupported, which is what the capabilities already promised.
func (c *Chat) handleRequest(method string, params json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return nil, fmt.Errorf("dbc does not support %s", method)
	}
	var p struct {
		ToolCall struct {
			Title string `json:"title"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(params, &p)
	what := p.ToolCall.Title
	if what == "" {
		what = "a tool"
	}
	c.emit(Event{Kind: EventNote, Text: "declined: " + what + " (dbc's assistant can only answer, not act)"})

	// Prefer the agent's own once-scoped reject, so nothing is remembered
	// against a later ask; fall back to the cancelled outcome when it offered
	// no reject at all.
	for _, kind := range []string{"reject_once", "reject_always"} {
		for _, o := range p.Options {
			if o.Kind == kind && o.OptionID != "" {
				return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}, nil
			}
		}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
}

// Send starts one turn with the given prompt text. It returns at once; the
// answer streams in as EventText and ends with EventTurnDone.
//
// The prompt goes as a single text block. ACP also has embedded-resource
// blocks for attachments, but dbc's context is small and structured (a query,
// maybe an error, a few rows) and reads best to a model as prose with fenced
// blocks — which every agent accepts, so there is one code path.
func (c *Chat) Send(text string) error {
	c.mu.Lock()
	if !c.ready || c.conn == nil {
		c.mu.Unlock()
		return ErrNotReady
	}
	if c.turn {
		c.mu.Unlock()
		return ErrBusy
	}
	c.turn, c.cancelSent = true, false
	conn, sid := c.conn, c.sessionID
	c.mu.Unlock()

	params := map[string]any{
		"sessionId": sid,
		"prompt":    []any{map[string]any{"type": "text", "text": text}},
	}
	go func() {
		var res struct {
			StopReason string `json:"stopReason"`
		}
		err := conn.call("session/prompt", params, &res, turnTimeout)
		c.mu.Lock()
		c.turn = false
		c.mu.Unlock()
		if err != nil {
			err = c.explain(err)
		}
		c.emit(Event{Kind: EventTurnDone, StopReason: res.StopReason, Err: err})
	}()
	return nil
}

// Busy reports whether an answer is streaming.
func (c *Chat) Busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turn
}

// Ready reports whether Send can be called.
func (c *Chat) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// Cancel asks the agent to stop the answer in flight. The turn then ends with
// EventTurnDone and stopReason "cancelled". A second Cancel in the same turn
// does nothing — the agent is already unwinding. Reports whether a cancel
// was sent.
func (c *Chat) Cancel() bool {
	c.mu.Lock()
	if !c.turn || c.cancelSent || c.conn == nil {
		c.mu.Unlock()
		return false
	}
	c.cancelSent = true
	conn, sid := c.conn, c.sessionID
	c.mu.Unlock()
	_ = conn.notify("session/cancel", map[string]any{"sessionId": sid})
	return true
}

// SetModel switches the session's model. It blocks for the round trip, so a
// UI calls it off its event loop.
func (c *Chat) SetModel(id string) error {
	c.mu.Lock()
	if !c.ready || c.conn == nil {
		c.mu.Unlock()
		return ErrNotReady
	}
	conn, sid := c.conn, c.sessionID
	c.mu.Unlock()
	return conn.call("session/set_model",
		map[string]any{"sessionId": sid, "modelId": id}, nil, sessionTimeout)
}

// Close ends the conversation and the agent process. Safe to call more than
// once, and before the handshake has finished.
func (c *Chat) Close() {
	c.once.Do(func() {
		close(c.done)
		c.mu.Lock()
		conn := c.conn
		c.ready = false
		c.mu.Unlock()
		if conn != nil {
			conn.close()
		}
	})
}
