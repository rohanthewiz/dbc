// The control-socket client: one connection per call, one newline-framed
// JSON request out, one response back, close. That is the whole transport —
// cats' control API is deliberately connectionless per command, so there is
// no session to keep alive, nothing to reconnect, and a dead server costs a
// failed dial rather than a wedged client. (The one exception, the event
// stream, upgrades a connection instead of closing it and lives in
// events.go.)
//
// Every wrapper is a thin shell over Call: build the params struct, name the
// verb, decode Data into the result. The value of the wrappers is that the
// verb names and wire shapes are spelled out ONCE here — mirrored from cats'
// internal/app/command_vocab.go — instead of at each call site in the app.
//
// Errors are plain: a transport failure, or the server's own message when it
// answers ok:false. Callers at Tier 0 never reach this code at all; callers
// at Tier 1 treat any error as "fall back to the Tier-0 path", which is why
// nothing here is typed beyond error.

package cats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Control-API verb names (cats internal/app/command_vocab.go §7 table).
// Only the ones dbc uses are mirrored; the table is much larger.
//
// The deliberate omissions are worth naming, because they are the growth
// path rather than an oversight: capture + pane.wait_for_output would let dbc
// read a sibling agent's answer back, and pane.split would let it open a
// result in a second dbc. Neither has a consumer yet, and a mirror struct
// with no caller is a wire contract nobody is checking.
const (
	MethodPing       = "ping"
	MethodPaneList   = "pane.list"
	MethodPaneFocus  = "pane.focus"
	MethodPaneSendIn = "pane.send_input"
	MethodChatSend   = "chat.send"
	MethodConfigGet  = "config.get"
	MethodSubscribe  = "events.subscribe"

	defaultCallTimeout = 3 * time.Second
)

// request is the outbound envelope. ID is echoed back; dbc sends one request
// per connection, so the id is only ever a label in a server-side log.
type request struct {
	ID     string `json:"id,omitempty"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// response is the inbound envelope. Data is left raw so each wrapper decodes
// its own result shape (or ignores it, for the verbs that return nothing).
type response struct {
	ID    string          `json:"id,omitempty"`
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Client is a control socket address plus the timeout its calls run under.
// It holds no connection and no lock: every call dials fresh, so a Client is
// safe to share across goroutines and cheap to keep on the App forever.
type Client struct {
	Socket string

	// Timeout bounds one whole call (dial + write + read). Zero means
	// defaultCallTimeout. It is a field rather than a constant so a caller
	// with a different budget — a startup probe that must give up fast —
	// can hand out a copy rather than mutate the shared client.
	Timeout time.Duration
}

// NewClient returns a client for the given control socket path. It performs
// no IO — an unreachable socket is discovered by the first call, which is
// also the only place a caller can do anything about it.
func NewClient(socket string) *Client { return &Client{Socket: socket} }

// Call runs one control command. out may be nil for verbs whose reply
// carries no data; when non-nil it is json.Unmarshal'd from the response's
// data field.
func (c *Client) Call(method string, params, out any) error {
	if c == nil || c.Socket == "" {
		return errors.New("cats: no control socket")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	conn, err := dial(c.Socket, timeout)
	if err != nil {
		return fmt.Errorf("cats: dial %s: %w", c.Socket, err)
	}
	defer conn.Close()

	line, err := json.Marshal(request{ID: "dbc", Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("cats: encode %s: %w", method, err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("cats: send %s: %w", method, err)
	}

	var resp response
	// A response can be longer than bufio's default 4KB line (a large
	// config.get, a session full of panes), so decode a stream rather than
	// reading a line: the JSON decoder stops at the closing brace, and the
	// newline framing is just what lets the SERVER find the end of our
	// request.
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return fmt.Errorf("cats: read %s: %w", method, err)
	}
	if !resp.OK {
		if resp.Error == "" {
			return fmt.Errorf("cats: %s failed", method)
		}
		return fmt.Errorf("cats: %s: %s", method, resp.Error)
	}
	if out == nil || len(resp.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return fmt.Errorf("cats: decode %s result: %w", method, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Wire shapes — minimal mirrors of cats' internal/app/command_vocab.go.
// Fields dbc neither sends nor reads are omitted on purpose: a mirror that
// tried to be complete would be a second copy of a file we do not own.
// -----------------------------------------------------------------------------

// Pong is the ping reply: how the server identifies itself.
type Pong struct {
	Protocol int    `json:"protocol"`
	Service  string `json:"service"`
}

// PaneInfo is one row of pane.list. Pane is the internal id every other
// command addresses a pane by; Handle is the public "w1:p3" label the pane
// environment carries — the pair is what makes ResolvePane possible, and what
// lets dbc recognize (and exclude) its own pane in a picker.
//
// The agent fields are cats' arbitrated identity for the pane, the same one
// its sidebar shows. cats flattens them into this object rather than nesting
// a meta struct, so the mirror inlines them too.
type PaneInfo struct {
	Pane       uint32 `json:"pane"`
	Handle     string `json:"handle,omitempty"`
	Name       string `json:"name,omitempty"`
	Focused    bool   `json:"focused"`
	Visible    bool   `json:"visible"`
	Agent      string `json:"agent,omitempty"`
	AgentState string `json:"agent_state,omitempty"`
	Title      string `json:"title,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
}

// PaneListResult is pane.list's payload.
type PaneListResult struct {
	Panes []PaneInfo `json:"panes"`
}

// ConfigTheme is the appearance section of config.get: the active theme name
// and its resolved color map, which is what a host-matching dbc palette is
// built from.
type ConfigTheme struct {
	Name   string            `json:"name,omitempty"`
	Colors map[string]string `json:"colors,omitempty"`
	Font   string            `json:"font,omitempty"`
}

// ConfigGetResult is config.get's payload, trimmed to the appearance fields.
// Theme is the EFFECTIVE appearance (the user's per-key overrides already
// folded in), which is the one a host-matching client should follow.
type ConfigGetResult struct {
	Path  string      `json:"path"`
	Theme ConfigTheme `json:"theme"`
}

// -----------------------------------------------------------------------------
// Typed verbs
// -----------------------------------------------------------------------------

// Ping asks the server to identify itself. It is the Tier-1 gate (see
// Caps.Probe) and the cheapest possible liveness check.
func (c *Client) Ping() (Pong, error) {
	var p Pong
	// The probe budget, not the call budget: this runs during startup
	// detection, where the whole point is to give up quickly. A copy rather
	// than a mutation, because the Client is shared.
	pc := *c
	if pc.Timeout <= 0 {
		pc.Timeout = ProbeTimeout
	}
	err := pc.Call(MethodPing, nil, &p)
	return p, err
}

// PaneList returns every pane in the session, with the agent/title/cwd
// metadata cats' own sidebar shows.
func (c *Client) PaneList() ([]PaneInfo, error) {
	var r PaneListResult
	if err := c.Call(MethodPaneList, nil, &r); err != nil {
		return nil, err
	}
	return r.Panes, nil
}

// ResolvePane turns the public pane handle from the environment ("w1:p3")
// into the internal id control commands take. The "p_<n>" form is decoded
// locally with no round trip; anything else costs one pane.list.
//
// Its real job is answering "which pane am I?", which nothing in the pane
// environment states directly and which a picker needs in order to leave dbc
// itself off the list of panes to talk to.
func (c *Client) ResolvePane(handle string) (uint32, error) {
	if id, ok := ParsePaneHandle(handle); ok {
		return id, nil
	}
	panes, err := c.PaneList()
	if err != nil {
		return 0, err
	}
	for _, p := range panes {
		if p.Handle == handle {
			return p.Pane, nil
		}
	}
	return 0, fmt.Errorf("cats: pane %q not found", handle)
}

// PaneFocus focuses a pane within its tab — the follow-through for "I sent
// that query next door", so the user lands where the text went.
func (c *Client) PaneFocus(pane uint32) error {
	return c.Call(MethodPaneFocus, struct {
		Pane uint32 `json:"pane"`
	}{pane}, nil)
}

// PaneSendInput injects text into a pane's PTY as though typed there.
// submit false stages the text for the user to read and press Enter on
// themselves — which is the setting dbc uses for "send this statement to the
// agent pane", because putting a query in an agent's mouth and running it is
// a different act from handing it one to look at.
func (c *Client) PaneSendInput(pane uint32, text string, submit bool) error {
	return c.Call(MethodPaneSendIn, struct {
		Pane   uint32 `json:"pane"`
		Text   string `json:"text,omitempty"`
		Submit bool   `json:"submit,omitempty"`
	}{pane, text, submit}, nil)
}

// ChatSend posts one user turn to cats' own chat panel — the "ask cats about
// this query" path, which reaches the host's agent rather than a sibling pane.
func (c *Client) ChatSend(text string) error {
	return c.Call(MethodChatSend, struct {
		Text string `json:"text"`
	}{text}, nil)
}

// ConfigGet reads the host's configuration; dbc uses the theme section.
func (c *Client) ConfigGet() (ConfigGetResult, error) {
	var r ConfigGetResult
	err := c.Call(MethodConfigGet, nil, &r)
	return r, err
}
