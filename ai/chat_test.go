package ai

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgent is an ACP agent on the far side of two in-memory pipes. It
// answers the handshake, streams a scripted answer to each prompt, and
// records what the client sent, so tests can check wire shapes as well as
// the events the Chat produces. The script is chosen by words in the prompt:
//
//	"PERMISSION" → ask the client for permission mid-turn, then finish
//	"SLOW"       → stream nothing until session/cancel arrives
//	anything else → two text chunks, a tool call, end_turn
type fakeAgent struct {
	t   *testing.T
	in  *bufio.Reader  // what the client wrote
	out *io.PipeWriter // to the client

	mu        sync.Mutex
	calls     []message // every request and notification received
	permReply chan json.RawMessage
	cancel    chan struct{}
	writeMu   sync.Mutex
}

// startFake builds a Chat wired to a fresh fake agent.
func startFake(t *testing.T, opt Options) (*Chat, *fakeAgent) {
	t.Helper()
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	fa := &fakeAgent{t: t, in: bufio.NewReader(c2aR), out: a2cW,
		permReply: make(chan json.RawMessage, 1), cancel: make(chan struct{}, 1)}
	go fa.serve()
	dial := func(onNotify func(string, json.RawMessage),
		onRequest func(string, json.RawMessage) (any, error),
		onExit func(error)) (*rpcConn, error) {
		return newRPCConn(a2cR, c2aW, onNotify, onRequest, onExit), nil
	}
	c := startWith(Agents()[0], opt, dial)
	t.Cleanup(c.Close)
	return c, fa
}

func (fa *fakeAgent) write(m any) {
	b, _ := json.Marshal(m)
	fa.writeMu.Lock()
	defer fa.writeMu.Unlock()
	_, _ = fa.out.Write(append(b, '\n'))
}

func (fa *fakeAgent) reply(id *int64, result any) {
	fa.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (fa *fakeAgent) update(kind string, extra map[string]any) {
	u := map[string]any{"sessionUpdate": kind}
	maps.Copy(u, extra)
	fa.write(map[string]any{"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": "s1", "update": u}})
}

func (fa *fakeAgent) chunk(text string) {
	fa.update("agent_message_chunk", map[string]any{"content": map[string]any{"type": "text", "text": text}})
}

// serve is the agent's read loop. Prompt turns run on their own goroutine,
// as in a real agent, so a cancel can arrive while one is in flight.
func (fa *fakeAgent) serve() {
	defer fa.out.Close()
	for {
		m, err := readLine(fa.in)
		if err != nil {
			return
		}
		fa.mu.Lock()
		fa.calls = append(fa.calls, *m)
		fa.mu.Unlock()

		switch {
		case m.Method == "initialize":
			fa.reply(m.ID, map[string]any{"protocolVersion": 1})
		case m.Method == "session/new":
			fa.reply(m.ID, map[string]any{
				"sessionId": "s1",
				"models": map[string]any{
					"currentModelId": "gpt-4.1",
					"availableModels": []any{
						map[string]any{"modelId": "gpt-4.1", "name": "GPT-4.1", "_meta": map[string]any{"copilotUsage": "0x"}},
						map[string]any{"modelId": "claude-sonnet", "name": "Claude Sonnet", "_meta": map[string]any{"copilotUsage": "1x"}},
					},
				},
			})
		case m.Method == "session/set_model":
			fa.reply(m.ID, map[string]any{})
		case m.Method == "session/cancel":
			fa.cancel <- struct{}{}
		case m.Method == "session/prompt":
			go fa.turn(m)
		case m.ID != nil && m.Method == "":
			// the client's answer to our permission request
			fa.permReply <- m.Result
		}
	}
}

func (fa *fakeAgent) turn(m *message) {
	var p struct {
		Prompt []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(m.Params, &p)
	text := ""
	if len(p.Prompt) > 0 {
		text = p.Prompt[0].Text
	}
	switch {
	case strings.Contains(text, "SLOW"):
		<-fa.cancel
		fa.reply(m.ID, map[string]any{"stopReason": "cancelled"})
	case strings.Contains(text, "PERMISSION"):
		id := int64(900)
		fa.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission",
			"params": map[string]any{
				"sessionId": "s1",
				"toolCall":  map[string]any{"title": "Run rm -rf /tmp/x", "kind": "execute"},
				"options": []any{
					map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
					map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"},
				},
			}})
		<-fa.permReply // must be answered before the turn can end
		fa.chunk("done")
		fa.reply(m.ID, map[string]any{"stopReason": "end_turn"})
	default:
		fa.chunk("Hello ")
		// a straggler from some other session must be ignored
		fa.write(map[string]any{"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]any{"sessionId": "old", "update": map[string]any{
				"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "STALE"}}}})
		fa.update("agent_thought_chunk", map[string]any{"content": map[string]any{"type": "text", "text": "hmm"}})
		fa.update("tool_call", map[string]any{"title": "Read schema"})
		fa.chunk("world")
		fa.reply(m.ID, map[string]any{"stopReason": "end_turn"})
	}
}

// received returns the calls with the given method.
func (fa *fakeAgent) received(method string) []message {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	var out []message
	for _, m := range fa.calls {
		if m.Method == method {
			out = append(out, m)
		}
	}
	return out
}

// next waits for the chat's next event.
func next(t *testing.T, c *Chat) Event {
	t.Helper()
	select {
	case e := <-c.Events():
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s")
		return Event{}
	}
}

// until reads events up to and including the first of the given kind.
func until(t *testing.T, c *Chat, kind EventKind) []Event {
	t.Helper()
	var evs []Event
	for {
		e := next(t, c)
		evs = append(evs, e)
		if e.Kind == kind {
			return evs
		}
		if e.Kind == EventExit {
			t.Fatalf("connection ended early: %v", e.Err)
		}
	}
}

func TestHandshakeReportsTheRosterAndAppliesThePreferredModel(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/work", Model: "claude-sonnet"})
	e := next(t, c)
	if e.Kind != EventReady {
		t.Fatalf("first event = %+v, want Ready", e)
	}
	if len(e.Models) != 2 || e.Models[1] != (Model{ID: "claude-sonnet", Name: "Claude Sonnet", Usage: "1x"}) {
		t.Errorf("models = %+v", e.Models)
	}
	if e.ModelID != "claude-sonnet" {
		t.Errorf("preferred model not applied: %q", e.ModelID)
	}

	// the wire: no fs capability, an absolute cwd, an empty (not null)
	// mcpServers, and set_model aimed at the new session
	var init struct {
		ClientCapabilities struct {
			FS map[string]bool `json:"fs"`
		} `json:"clientCapabilities"`
	}
	_ = json.Unmarshal(fa.received("initialize")[0].Params, &init)
	if init.ClientCapabilities.FS["readTextFile"] || init.ClientCapabilities.FS["writeTextFile"] {
		t.Errorf("dbc must not offer filesystem access: %+v", init.ClientCapabilities.FS)
	}
	if got := string(fa.received("session/new")[0].Params); got != `{"cwd":"/work","mcpServers":[]}` {
		t.Errorf("session/new params = %s", got)
	}
	if got := string(fa.received("session/set_model")[0].Params); got != `{"modelId":"claude-sonnet","sessionId":"s1"}` {
		t.Errorf("set_model params = %s", got)
	}
}

// A saved model the roster no longer offers is skipped, never an error.
func TestStalePreferredModelKeepsTheDefault(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w", Model: "retired-model"})
	if e := next(t, c); e.Kind != EventReady || e.ModelID != "gpt-4.1" {
		t.Fatalf("event = %+v", e)
	}
	if n := len(fa.received("session/set_model")); n != 0 {
		t.Errorf("set_model should not be sent for an unknown id, got %d", n)
	}
}

func TestSendStreamsTheAnswerInOrder(t *testing.T) {
	// before the handshake, a send is refused rather than lost
	if err := (&Chat{}).Send("too early"); !errors.Is(err, ErrNotReady) {
		t.Errorf("Send before ready = %v, want ErrNotReady", err)
	}

	c, fa := startFake(t, Options{Dir: "/w"})
	until(t, c, EventReady)

	if err := c.Send("explain this"); err != nil {
		t.Fatal(err)
	}
	if !c.Busy() {
		t.Error("Busy should be true while the answer streams")
	}
	if err := c.Send("again"); !errors.Is(err, ErrBusy) {
		t.Errorf("a second Send mid-turn = %v, want ErrBusy", err)
	}
	evs := until(t, c, EventTurnDone)

	var text strings.Builder
	var tools []string
	for _, e := range evs {
		switch e.Kind {
		case EventText:
			text.WriteString(e.Text)
		case EventTool:
			tools = append(tools, e.Text)
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("answer = %q (thoughts and stale sessions must be dropped)", text.String())
	}
	if len(tools) != 1 || tools[0] != "Read schema" {
		t.Errorf("tools = %q", tools)
	}
	done := evs[len(evs)-1]
	if done.StopReason != "end_turn" || done.Err != nil {
		t.Errorf("turn done = %+v", done)
	}
	if c.Busy() {
		t.Error("Busy should clear when the turn ends")
	}

	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type, Text string
		} `json:"prompt"`
	}
	calls := fa.received("session/prompt")
	_ = json.Unmarshal(calls[len(calls)-1].Params, &p)
	if p.SessionID != "s1" || len(p.Prompt) != 1 || p.Prompt[0].Type != "text" || p.Prompt[0].Text != "explain this" {
		t.Errorf("prompt params = %+v", p)
	}
}

func TestCancelStopsTheTurnOnce(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w"})
	until(t, c, EventReady)
	if c.Cancel() {
		t.Error("nothing is running, so there is nothing to cancel")
	}
	if err := c.Send("SLOW please"); err != nil {
		t.Fatal(err)
	}
	if !c.Cancel() {
		t.Fatal("Cancel should send while a turn runs")
	}
	if c.Cancel() {
		t.Error("a second Cancel in one turn should do nothing")
	}
	evs := until(t, c, EventTurnDone)
	if got := evs[len(evs)-1].StopReason; got != "cancelled" {
		t.Errorf("stop reason = %q", got)
	}
	if n := len(fa.received("session/cancel")); n != 1 {
		t.Errorf("session/cancel sent %d times", n)
	}
}

// The assistant may talk but not act: a permission request is declined with
// the agent's own reject option, and the transcript says so.
func TestPermissionRequestsAreDeclined(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w"})
	until(t, c, EventReady)
	if err := c.Send("PERMISSION test"); err != nil {
		t.Fatal(err)
	}
	evs := until(t, c, EventTurnDone)

	var note string
	for _, e := range evs {
		if e.Kind == EventNote {
			note = e.Text
		}
	}
	if !strings.Contains(note, "declined") || !strings.Contains(note, "Run rm -rf /tmp/x") {
		t.Errorf("note = %q", note)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	var last message
	for _, m := range fa.calls {
		if m.ID != nil && *m.ID == 900 {
			last = m
		}
	}
	if got := string(last.Result); got != `{"outcome":{"optionId":"no","outcome":"selected"}}` {
		t.Errorf("permission answer = %s", got)
	}
}

func TestHandleRequestFallsBackToCancelledWithoutARejectOption(t *testing.T) {
	c := &Chat{events: make(chan Event, 4), done: make(chan struct{})}
	res, err := c.handleRequest("session/request_permission",
		json.RawMessage(`{"toolCall":{"title":"x"},"options":[{"optionId":"a","kind":"allow_once"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if string(b) != `{"outcome":{"outcome":"cancelled"}}` {
		t.Errorf("answer = %s", b)
	}
	if _, err := c.handleRequest("fs/write_text_file", nil); err == nil {
		t.Error("filesystem requests must be refused")
	}
}

// The agent dying mid-conversation is reported once, with a reason.
func TestAgentExitIsReported(t *testing.T) {
	c, fa := startFake(t, Options{Dir: "/w"})
	until(t, c, EventReady)
	fa.out.Close() // the process went away
	e := next(t, c)
	if e.Kind != EventExit || e.Err == nil {
		t.Fatalf("event = %+v, want Exit with a reason", e)
	}
	if c.Ready() {
		t.Error("a dead chat must not report ready")
	}
	if err := c.Send("hello?"); !errors.Is(err, ErrNotReady) {
		t.Errorf("Send after exit = %v", err)
	}
}

// A deliberate Close is not an error, and delivers nothing afterwards.
func TestCloseIsQuiet(t *testing.T) {
	c, _ := startFake(t, Options{Dir: "/w"})
	until(t, c, EventReady)
	c.Close()
	c.Close() // idempotent
	select {
	case e := <-c.Events():
		if e.Kind == EventExit && e.Err != nil {
			t.Errorf("a deliberate close reported an error: %v", e.Err)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

// A missing binary is an Exit that says how to install it — the chat pane is
// the one place a missing integration is worth explaining.
func TestMissingBinaryExplainsTheInstall(t *testing.T) {
	prev := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookPath = prev })

	c := Start(Agents()[0], Options{Dir: "/w"})
	t.Cleanup(c.Close)
	e := next(t, c)
	if e.Kind != EventExit || e.Err == nil ||
		!strings.Contains(e.Err.Error(), "npm install -g @github/copilot-language-server") {
		t.Errorf("event = %+v", e)
	}
}

func TestExplainAddsTheSignInHint(t *testing.T) {
	c := &Chat{agent: Agents()[0]}
	if got := c.explain(errors.New("session/new: Not authorized (code -32000)")).Error(); !strings.Contains(got, "sign in") {
		t.Errorf("auth error without a hint: %q", got)
	}
	if got := c.explain(errors.New("boom")).Error(); got != "boom" {
		t.Errorf("unrelated errors pass through: %q", got)
	}
}

func TestAgentByID(t *testing.T) {
	if a, ok := AgentByID(""); !ok || a.ID != CopilotID {
		t.Errorf("empty = %v %v", a.ID, ok)
	}
	if a, ok := AgentByID("claude"); !ok || a.Binary != "claude-code-acp" {
		t.Errorf("claude = %+v %v", a, ok)
	}
	if a, ok := AgentByID("clippy"); ok || a.ID != CopilotID {
		t.Errorf("an unknown id falls back to the default and says so: %v %v", a.ID, ok)
	}
}
