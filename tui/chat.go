package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/db"
)

// The assistant pane: a conversation with an ACP agent (Copilot by default)
// about the query in the editor and the result in the grid.
//
//	┌ ✦ Copilot · Claude Sonnet 5 ▾ ────────── ⟲ new  ✕ ┐  ← header chips
//	│ ❯ why is this slow?                                │
//	│ The planner scans cats because …                   │  ← transcript
//	│ ┌ sql ──────────────────────── ⤓ insert  ⧉ copy ┐  │    (wheel scrolls)
//	│ │ CREATE INDEX ON cats (breed);                  │  │
//	│ ⧉ copy reply                                       │
//	│ [✓] with: query, 10 of 25 rows                     │  ← context chip
//	│ ask about the query or result…              ⏎ send │  ← composer
//	└────────────────────────────────────────────────────┘
//
// THE AGENT STARTS LAZILY, on first open: most sessions never use it, and
// spawning a Node process at every dbc launch would cost those users memory
// and a second of CPU for nothing.
//
// CONTEXT IS PER TURN and visible. The chip above the composer says what the
// next question will carry (schema, query, error, rows — per package ai's
// data rule) and clicking it turns context off; each sent question is
// followed in the transcript by the line saying what actually went.
//
// SCHEMA IS LOOKED UP AT SEND TIME. Which tables are involved is known as
// the user types — a word match of the query and the composer against the
// catalog the sidebar already holds, cheap enough to redo every frame — so
// the chip can say "schema of cats" before anything is sent. Their COLUMNS
// cost a database round trip, which a draw cannot make, so submitting runs
// one catalog query first and sends when it answers:
//
//	enter ─► chatSubmit ── tables? ──no──────────────────► finishSubmit ─► agent
//	                          │yes                              ▲
//	                          └─► schemaCmd (pool, ≤3s) ─► chatSchemaMsg
//
// The turn counts as in flight from the enter, so a second question cannot
// overtake the first while its lookup runs. The lookup is not cached:
// columns change under ALTER TABLE, and one catalog query per question is
// milliseconds against the seconds the model takes to answer.
//
// SQL IN ANSWERS IS ACTIONABLE. Every fenced code block gets ⤓ insert (drop
// it into the editor at the caret) and ⧉ copy. There is no "run" button on
// purpose: a statement the model wrote should pass through the editor, where
// the user reads it, before it goes anywhere near the database.

// chatState is where the agent connection is.
type chatState int

const (
	chatIdle     chatState = iota // not started
	chatStarting                  // process up, handshake in flight
	chatReady
	chatDead // failed or exited; ⟲ new retries
)

type chatRole int

const (
	roleUser chatRole = iota
	roleAgent
	roleTool
	roleNote
	roleInfo
	roleErr
)

type chatMsg struct {
	role chatRole
	text string
}

// chatPane is the assistant's state.
type chatPane struct {
	open    bool
	c       *ai.Chat
	gen     int // bumped per connection; events from an older one are dropped
	state   chatState
	agent   ai.Agent
	models  []ai.Model
	modelID string

	msgs      []chatMsg
	streaming bool   // an answer is in flight
	first     bool   // the next turn is the conversation's first
	pending   string // a prompt written before the handshake finished
	attach    bool   // send context with the next question

	input *editor

	// transcript scroll, in visual rows
	top    int
	follow bool
	total  int

	// geometry of the last draw, for hit-testing
	modelChip, newBtn, closeBtn, stopBtn, attachChip, sendBtn Rect
	transcript, inputR                                        Rect
	targets                                                   []chatTarget
	hover                                                     int // index into targets, or -1
}

// chatTarget is a clickable spot in the transcript.
type chatTarget struct {
	r    Rect
	kind targetKind
	text string
}

type targetKind int

const (
	targetInsert targetKind = iota
	targetCopyCode
	targetCopyReply
)

// chatEventMsg carries one ai.Event to Update, tagged with the connection
// generation it came from.
type chatEventMsg struct {
	gen int
	ev  ai.Event
}

// chatSchemaMsg delivers the columns looked up for a question, to be sent
// with it. ctx is the context as it stood at the enter, so an editor change
// during the lookup does not change what the question was about.
type chatSchemaMsg struct {
	gen      int
	question string
	ctx      ai.Context
	cols     [][]db.Column // parallel to ctx.Tables; nil on error
	err      error
}

// maxSchemaTables caps how many tables' columns go with one question. A
// query joining more than this is rare; a question whose words happen to
// name a dozen tables is not asking about all of them.
const maxSchemaTables = 8

// schemaLookupTimeout bounds the catalog query at send time. Past it the
// question goes without columns rather than waiting on a busy database.
const schemaLookupTimeout = 3 * time.Second

// chatModelSetMsg reports a model switch.
type chatModelSetMsg struct {
	gen int
	id  string
	err error
}

func newChatPane() *chatPane {
	p := &chatPane{input: newEditor(false), follow: true, first: true, attach: true, hover: -1}
	p.input.placeholder = "ask about the query or result…"
	return p
}

func (p *chatPane) busy() bool { return p.streaming }

func (p *chatPane) close() {
	if p.c != nil {
		p.c.Close()
	}
}

// startChat is ai.Start, as a var so tests hand in a scripted agent instead
// of spawning the real one — which would need it installed and signed in,
// and would spend the developer's Copilot requests on every test run.
var startChat = ai.Start

// waitChat returns a command that delivers the chat's next event, or nothing
// once the chat is closed.
func waitChat(c *ai.Chat, gen int) tea.Cmd {
	return func() tea.Msg {
		select {
		case e := <-c.Events():
			return chatEventMsg{gen: gen, ev: e}
		case <-c.Done():
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// Opening, starting, closing
// ---------------------------------------------------------------------------

// toggleChat opens or closes the pane (the ✦ toolbar button). Closing only
// hides it: the conversation and the agent stay, so reopening continues.
func (m *Model) toggleChat() tea.Cmd {
	if m.chat.open {
		m.chat.open = false
		if m.focus == focusChat {
			m.focus = focusEditor
		}
		return nil
	}
	m.chat.open = true
	m.focus = focusChat
	return m.ensureChat()
}

// toggleChatFocus is Ctrl+A: open the assistant and put the keyboard in it,
// or, from inside it, go back to the editor.
func (m *Model) toggleChatFocus() tea.Cmd {
	if m.chat.open && m.focus == focusChat {
		m.focus = focusEditor
		return nil
	}
	m.chat.open = true
	m.focus = focusChat
	return m.ensureChat()
}

// ensureChat starts the agent if it is not running.
func (m *Model) ensureChat() tea.Cmd {
	p := m.chat
	if p.c != nil && (p.state == chatStarting || p.state == chatReady) {
		return nil
	}
	if p.agent.ID == "" {
		p.agent = m.aiAgent
		if _, ok := ai.AgentByID(m.cfg.AIAgent); !ok && m.cfg.AIAgent != "" {
			p.add(roleInfo, fmt.Sprintf("unknown ai_agent %q — using %s", m.cfg.AIAgent, p.agent.Name))
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		dir = os.TempDir()
	}
	p.gen++
	p.state = chatStarting
	p.c = startChat(p.agent, ai.Options{Dir: dir, Model: m.cfg.AIModel})
	return waitChat(p.c, p.gen)
}

// newChat ends the conversation and starts a fresh one — which also resets
// the agent's own memory of it, since that lives in the agent's session.
func (m *Model) newChat() tea.Cmd {
	p := m.chat
	p.close()
	p.c, p.state, p.streaming, p.pending = nil, chatIdle, false, ""
	p.msgs, p.first, p.top, p.follow = nil, true, 0, true
	return m.ensureChat()
}

// switchAgent starts a new conversation with a different backend.
func (m *Model) switchAgent(a ai.Agent) tea.Cmd {
	m.chat.agent = a
	return m.newChat()
}

// chatStop asks the agent to stop the answer in flight.
func (m *Model) chatStop() {
	if m.chat.c != nil && m.chat.c.Cancel() {
		m.chat.add(roleInfo, "— stopping…")
	}
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// chatEvent lands one event from the agent and re-arms the wait.
func (m *Model) chatEvent(msg chatEventMsg) tea.Cmd {
	p := m.chat
	if msg.gen != p.gen || p.c == nil {
		return nil
	}
	e := msg.ev
	switch e.Kind {
	case ai.EventReady:
		p.state, p.models, p.modelID = chatReady, e.Models, e.ModelID
		if p.pending != "" {
			prompt := p.pending
			p.pending = ""
			m.chatSend(prompt)
		}
	case ai.EventText:
		p.appendAgent(e.Text)
	case ai.EventTool:
		p.add(roleTool, "⚙ "+e.Text)
	case ai.EventNote:
		p.add(roleNote, e.Text)
	case ai.EventTurnDone:
		// streaming=false is also what makes the next chunk start a new
		// message rather than extend this answer
		p.streaming = false
		switch {
		case e.Err != nil:
			p.add(roleErr, e.Err.Error())
		case e.StopReason == "cancelled":
			p.add(roleInfo, "— stopped")
		}
		m.catsAfterTransition()
	case ai.EventExit:
		p.streaming = false
		p.state = chatDead
		if e.Err != nil {
			p.add(roleErr, e.Err.Error())
			p.add(roleInfo, "click ⟲ new to try again")
		}
		m.catsAfterTransition()
		return nil // nothing more will come from this connection
	}
	return waitChat(p.c, p.gen)
}

func (m *Model) chatModelSet(msg chatModelSetMsg) tea.Cmd {
	p := m.chat
	if msg.gen != p.gen {
		return nil
	}
	if msg.err != nil {
		p.add(roleErr, "could not switch model: "+msg.err.Error())
		return nil
	}
	p.modelID = msg.id
	p.add(roleInfo, "model: "+p.modelName())
	return nil
}

func (p *chatPane) add(role chatRole, text string) {
	p.msgs = append(p.msgs, chatMsg{role: role, text: text})
}

// appendAgent adds a streamed chunk to the answer being written, starting
// one if the last message is not an open answer.
func (p *chatPane) appendAgent(text string) {
	if n := len(p.msgs); n > 0 && p.msgs[n-1].role == roleAgent && p.streaming {
		p.msgs[n-1].text += text
		return
	}
	p.add(roleAgent, text)
}

func (p *chatPane) modelName() string {
	for _, md := range p.models {
		if md.ID == p.modelID {
			return md.Name
		}
	}
	return p.modelID
}

// ---------------------------------------------------------------------------
// Asking
// ---------------------------------------------------------------------------

// chatContext gathers what the next question may carry — see package ai for
// what actually goes, which is decided by the connection's ai_rows.
//
// Which statement is "this query": the one under the editor's caret, since
// that is what the user is looking at. When it is the statement that last
// ran, its result (or error) comes along; when the editor is empty, the last
// run's statement stands in.
//
// question is the question being (or about to be) asked; its words, like
// the statement's, pick which tables' schema goes along. The Tables come
// back named but without columns — see chatSubmit for the lookup — and refs
// is the same tables as the catalog knows them, for that lookup.
func (m *Model) chatContext(question string) (ctx ai.Context, refs []db.TableRef) {
	cc, _ := m.cfg.ConnByName(m.active)
	ctx = ai.Context{Conn: m.active, Driver: cc.Driver, SendRows: cc.AIRows, MaxRows: m.cfg.AIContextRows}
	cur := ""
	if stmts, _ := m.stmtsToRun(); len(stmts) > 0 {
		cur = stmts[len(stmts)-1]
	}
	if cur == "" {
		cur = m.lastStmt
	}
	ctx.Query = cur
	if m.tableIdx != nil {
		refs = m.tableIdx.Mentioned(cur, question)
		refs = refs[:min(len(refs), maxSchemaTables)]
		for _, r := range refs {
			ctx.Tables = append(ctx.Tables, ai.Table{Name: m.tableIdx.Display(r), View: r.View})
		}
	}
	if cur == "" || cur != m.lastStmt {
		return ctx, refs
	}
	if m.lastErr != "" {
		ctx.Err = m.lastErr
		return ctx, refs
	}
	if r := m.lastRes; r != nil && !r.IsExec {
		ctx.Columns, ctx.Rows, ctx.Truncated = r.Columns, r.Rows, r.Truncated
		// Columns hidden in the grid are left out, as copies and exports
		// leave them out — see package ai's HIDDEN COLUMNS for why. The
		// guard makes sure hidden is indexed by this result's columns; the
		// two are set together today, so it is belt and braces.
		//
		// A header sort goes too: rows are sent in the grid's order and the
		// model is told so. Only the prefix of the order that could be sent
		// is copied — this runs every frame for the chip — and it IS copied,
		// because applySort rewrites order in place and a submit's context
		// waits for its schema lookup before it is built into a prompt.
		if g := m.grid; g.res == r {
			ctx.Hidden = g.HiddenCols()
			if g.sortCol >= 0 {
				ctx.Order = slices.Clone(g.order[:min(len(g.order), max(ctx.MaxRows, 0))])
				ctx.SortedBy, ctx.SortDesc = r.Columns[g.sortCol], g.sortDesc
			}
		}
	}
	return ctx, refs
}

// askAbout opens the assistant with a question drafted in the composer —
// not sent, so the user can add to it first.
func (m *Model) askAbout(question string) tea.Cmd {
	cmd := m.toggleChatFocus()
	if m.focus != focusChat { // it was already focused and toggled away
		m.focus = focusChat
	}
	m.chat.input.SetText(question)
	m.chat.input.move(pos{0, len([]rune(question))}, false)
	return cmd
}

// chatSubmit sends what is in the composer.
func (m *Model) chatSubmit() tea.Cmd {
	p := m.chat
	q := strings.TrimSpace(p.input.Text())
	if q == "" {
		return nil
	}
	if p.streaming {
		m.log(logWarn, "the assistant is still answering — Ctrl+K stops it")
		return nil
	}
	p.input.SetText("")
	p.add(roleUser, q)
	p.follow = true
	var ctx ai.Context
	var refs []db.TableRef
	if p.attach {
		ctx, refs = m.chatContext(q)
	}

	// ensureChat may start the agent, which bumps the generation; the
	// lookup is tagged with the generation it will be sent on
	cmd := m.ensureChat()
	if len(refs) == 0 {
		m.finishSubmit(q, ctx)
		return cmd
	}
	// in flight from here, lookup included — see the diagram at the top
	p.streaming = true
	return tea.Batch(cmd, m.schemaCmd(p.gen, q, ctx, refs))
}

// schemaCmd looks up the columns of the tables a question involves.
func (m *Model) schemaCmd(gen int, question string, ctx ai.Context, refs []db.TableRef) tea.Cmd {
	mgr, conn := m.mgr, m.active
	return func() tea.Msg {
		c, cancel := context.WithTimeout(context.Background(), schemaLookupTimeout)
		defer cancel()
		cols, err := mgr.Columns(c, conn, refs)
		return chatSchemaMsg{gen: gen, question: question, ctx: ctx, cols: cols, err: err}
	}
}

// chatSchema lands a lookup and sends the question it was for.
func (m *Model) chatSchema(msg chatSchemaMsg) tea.Cmd {
	p := m.chat
	if msg.gen != p.gen {
		return nil // ⟲ new or an agent switch since: that conversation is gone
	}
	ctx := msg.ctx
	if msg.err != nil {
		// The question still goes, without schema, and the transcript says
		// why rather than letting the model's guessed columns look like
		// dbc's facts.
		p.add(roleInfo, "schema lookup failed, sending without it: "+msg.err.Error())
		ctx.Tables = nil
	} else {
		// Keep only what the catalog could describe, so the note lists
		// exactly the tables whose columns went.
		var tables []ai.Table
		for i, t := range ctx.Tables {
			if i < len(msg.cols) && len(msg.cols[i]) > 0 {
				for _, c := range msg.cols[i] {
					t.Columns = append(t.Columns, ai.Column{Name: c.Name, Type: c.Type})
				}
				tables = append(tables, t)
			}
		}
		ctx.Tables = tables
	}
	if p.state == chatDead {
		// the agent exited while the lookup ran; its exit is already in the
		// transcript, and ⟲ new starts over
		p.streaming = false
		return nil
	}
	m.finishSubmit(msg.question, ctx)
	return nil
}

// finishSubmit builds the prompt and hands it to the agent, or queues it
// until the handshake completes.
func (m *Model) finishSubmit(question string, ctx ai.Context) {
	p := m.chat
	prompt := ai.Build(question, ctx, p.first)
	p.first = false
	p.add(roleNote, "▤ "+prompt.Note)
	p.follow = true
	if p.state != chatReady {
		p.pending = prompt.Text
		p.streaming = true
		return
	}
	m.chatSend(prompt.Text)
}

// chatSend hands a prompt to the agent.
func (m *Model) chatSend(text string) {
	p := m.chat
	if err := p.c.Send(text); err != nil {
		p.streaming = false
		if errors.Is(err, ai.ErrBusy) {
			p.add(roleInfo, "still answering the last question")
			return
		}
		p.add(roleErr, err.Error())
		return
	}
	p.streaming = true
	m.catsAfterTransition()
}

// ---------------------------------------------------------------------------
// Keys and mouse
// ---------------------------------------------------------------------------

func (m *Model) chatKey(k tea.KeyPressMsg) tea.Cmd {
	p := m.chat
	switch k.String() {
	case "enter":
		return m.chatSubmit()
	case "esc":
		m.focus = focusEditor
		return nil
	case "pgup":
		p.scroll(-max(p.transcript.H-1, 1))
		return nil
	case "pgdown":
		p.scroll(max(p.transcript.H-1, 1))
		return nil
	}
	p.input.HandleKey(k)
	return nil
}

func (p *chatPane) scroll(n int) {
	maxTop := max(p.total-p.transcript.H, 0)
	if p.follow {
		p.top = maxTop
	}
	p.top = max(0, min(p.top+n, maxTop))
	p.follow = p.top >= maxTop
}

func (m *Model) chatClick(x, y, n int, shift bool) tea.Cmd {
	p := m.chat
	switch {
	case p.closeBtn.Contains(x, y):
		return m.toggleChat()
	case p.newBtn.Contains(x, y):
		return m.newChat()
	case p.stopBtn.Contains(x, y):
		m.chatStop()
		return nil
	case p.modelChip.Contains(x, y):
		m.openModelMenu(p.modelChip.X, p.modelChip.Y+1)
		return nil
	case p.attachChip.Contains(x, y):
		p.attach = !p.attach
		return nil
	case p.sendBtn.Contains(x, y):
		return m.chatSubmit()
	case p.inputR.Contains(x, y):
		p.input.Click(x, y, n, shift)
		m.drag.kind = dragChatInput
		return nil
	}
	for _, t := range p.targets {
		if t.r.Contains(x, y) {
			return m.chatTargetPress(t)
		}
	}
	return nil
}

func (m *Model) chatTargetPress(t chatTarget) tea.Cmd {
	switch t.kind {
	case targetInsert:
		code := strings.TrimRight(t.text, "\n")
		m.editor.Insert(code)
		m.focus = focusEditor
		m.drag.follow = true
		m.logf(logOk, "inserted the assistant's SQL at the caret — %s", preview(code))
		return nil
	case targetCopyCode:
		return m.copyString(t.text, "the code block")
	case targetCopyReply:
		return m.copyString(t.text, "the reply")
	}
	return nil
}

func (m *Model) chatHover(x, y int) {
	p := m.chat
	p.hover = -1
	for i, t := range p.targets {
		if t.r.Contains(x, y) {
			p.hover = i
		}
	}
}

// openModelMenu lists the agent's models and the other installed agents.
func (m *Model) openModelMenu(x, y int) {
	p := m.chat
	var items []menuItem
	if len(p.models) > 0 {
		items = append(items, heading("model"))
		for _, md := range p.models {
			id := md.ID
			label := "  " + md.Name
			if id == p.modelID {
				label = "● " + md.Name
			}
			items = append(items, menuItem{label: label, key: md.Usage, act: func(m *Model) tea.Cmd {
				c, gen := m.chat.c, m.chat.gen
				return func() tea.Msg { return chatModelSetMsg{gen: gen, id: id, err: c.SetModel(id)} }
			}})
		}
	}
	items = append(items, heading("assistant"))
	for _, a := range ai.Agents() {
		a := a
		label := "  " + a.Name
		if a.ID == p.agent.ID {
			label = "● " + a.Name
		}
		why := ""
		if !a.Installed() {
			why = a.Name + " is not installed — " + a.Install
		}
		items = append(items, menuItem{label: label, why: why, key: a.Binary,
			act: func(m *Model) tea.Cmd { return m.switchAgent(a) }})
	}
	m.openMenu(x, y, items)
}

// openChatMenu is the transcript's context menu.
func (m *Model) openChatMenu(x, y int) {
	p := m.chat
	last := ""
	for i := len(p.msgs) - 1; i >= 0; i-- {
		if p.msgs[i].role == roleAgent {
			last = p.msgs[i].text
			break
		}
	}
	noReply := ""
	if last == "" {
		noReply = "no reply to copy yet"
	}
	m.openMenu(x, y, []menuItem{
		{label: "Copy last reply", why: noReply, act: func(m *Model) tea.Cmd { return m.copyString(last, "the reply") }},
		{label: "Copy conversation", act: func(m *Model) tea.Cmd { return m.copyString(p.transcriptText(), "the conversation") }},
		heading(""),
		{label: "⟲ New conversation", act: func(m *Model) tea.Cmd { return m.newChat() }},
		{label: "Model and assistant…", act: func(m *Model) tea.Cmd { m.openModelMenu(x, y); return nil }},
	})
}

// transcriptText is the conversation as plain text.
func (p *chatPane) transcriptText() string {
	var b strings.Builder
	for _, msg := range p.msgs {
		switch msg.role {
		case roleUser:
			b.WriteString("> " + msg.text + "\n\n")
		case roleAgent:
			b.WriteString(msg.text + "\n\n")
		default:
			b.WriteString("  " + msg.text + "\n")
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// chatRow is one visual row of the transcript.
type chatRow struct {
	segs []seg
	fill Style // background across the row (code blocks)
	hasF bool
	tgts []rowTarget
}

type seg struct {
	text string
	st   Style
}

// rowTarget is a clickable span within a row, in columns.
type rowTarget struct {
	x0, x1 int
	kind   targetKind
	text   string
}

// drawChat draws the pane and returns the composer's caret when visible.
func (m *Model) drawChat(c *Canvas, r Rect) *caret {
	p := m.chat
	st := m.st
	focused := m.focus == focusChat
	s := c.Sub(r)
	s.Fill(st.base)
	border, ts := st.border, st.title
	if focused {
		border, ts = st.borderFocus, st.titleFocus
	}
	s.Box(border, "", ts)
	in := c.Sub(r.Inset(1))
	W := in.W()

	// header: agent · model ▾ …… ⟲ new ✕
	name := p.agent.Name
	if name == "" {
		name = m.aiAgent.Name
	}
	title := "✦ " + name
	if p.modelID != "" {
		title += " · " + p.modelName()
	}
	title += " ▾"
	p.modelChip = chip(s, 2, 0, " "+truncate(title, W-14)+" ", onBg(ts, st.base))
	p.closeBtn = chip(s, r.W-5, 0, " ✕ ", st.err.Bold())
	p.newBtn = chip(s, r.W-12, 0, " ⟲ new ", st.muted)

	// status line
	status, sst := "", st.muted
	p.stopBtn = Rect{}
	switch {
	case p.state == chatStarting:
		status = "connecting to " + name + "…"
	case p.streaming:
		status, sst = "thinking…", st.warn
	case p.state == chatDead:
		status, sst = "not connected", st.err
	case p.state == chatIdle:
		status = "starts on your first question"
	}
	if status != "" {
		in.Put(0, 0, status, sst.Italic())
		if p.streaming {
			p.stopBtn = chip(in, W-9, 0, " ■ stop ", st.buttonStop)
		}
	}

	// composer, sized to its content (1–6 rows)
	inH := max(1, min(len(p.input.lines), 6))
	composerY := in.H() - inH

	// context chip. It wraps onto a second row rather than truncating: the
	// end of the note is where "(1 column hidden)" and the ai_rows hint
	// sit, and a chip that cuts off what is left out of the context would
	// defeat the chip. Two rows at most, the second truncated, so a long
	// schema list cannot eat the transcript; the continuation is indented
	// under the note, clear of the checkbox.
	note := "context off — question only"
	if p.attach {
		ctx, _ := m.chatContext(p.input.Text())
		note = strings.TrimPrefix(ai.Build("", ctx, false).Note, "sent: ")
		note = "with: " + note
	}
	box := "[✓] "
	if !p.attach {
		box = "[ ] "
	}
	chipLines := wrap(note, W-width(box))
	if len(chipLines) > 2 {
		chipLines = []string{chipLines[0], strings.Join(chipLines[1:], "")}
	}
	chipY := composerY - len(chipLines)
	p.transcript = Rect{in.Rect().X, in.Rect().Y + 1, W, max(chipY-1, 0)}
	p.attachChip = Rect{}
	for i, line := range chipLines {
		lead := box
		if i > 0 {
			lead = strings.Repeat(" ", width(box))
		}
		r := chip(in, 0, chipY+i, truncate(lead+strings.TrimRight(line, " "), W), st.muted)
		if i == 0 {
			p.attachChip = r
		} else { // the click target covers every row the chip spans
			p.attachChip.W = max(p.attachChip.W, r.W)
			p.attachChip.H++
		}
	}

	inputS := in.Sub(Rect{0, composerY, W - 8, inH})
	p.inputR = inputS.Rect()
	cx, cy, ok := p.input.Draw(inputS, st, st.raised, [2]int{}, true)
	p.sendBtn = chip(in, W-7, composerY+inH-1, " ⏎ send", pick(p.input.Empty(), st.button.Dim(), st.buttonHot))

	// transcript
	rows := p.layoutRows(st, W-1)
	p.total = len(rows)
	th := p.transcript.H
	maxTop := max(len(rows)-th, 0)
	if p.follow {
		p.top = maxTop
	}
	p.top = min(p.top, maxTop)
	p.targets = p.targets[:0]
	ts2 := c.Sub(p.transcript)
	if len(p.msgs) == 0 {
		hint := []string{
			"Ask about the query in the editor or the result in the grid.",
			"",
			"The query, any error, and the columns of the tables it or your question " +
				"names go with each question. Result rows go only " +
				"on connections with ai_rows = true (up to ai_context_rows of them), " +
				"in the grid's sort order and without its hidden columns.",
			"",
			"SQL in answers gets ⤓ insert, which puts it in the editor.",
			"",
			"Enter sends · Alt+Enter adds a line · Ctrl+K stops an answer · " +
				"Esc returns to the editor",
		}
		y := 1
		for _, h := range hint {
			for _, line := range wrap(h, W-3) {
				ts2.Put(1, y, line, st.muted)
				y++
			}
		}
	}
	for y := 0; y < th && p.top+y < len(rows); y++ {
		row := rows[p.top+y]
		if row.hasF {
			ts2.Sub(Rect{0, y, W - 1, 1}).Fill(row.fill)
		}
		x := 0
		for _, sg := range row.segs {
			x = ts2.Put(x, y, sg.text, sg.st)
		}
		for _, t := range row.tgts {
			abs := Rect{p.transcript.X + t.x0, p.transcript.Y + y, t.x1 - t.x0, 1}
			idx := len(p.targets)
			p.targets = append(p.targets, chatTarget{r: abs, kind: t.kind, text: t.text})
			if idx == p.hover {
				c.Sub(abs).Restyle(func(s Style) Style { return st.buttonHover })
			}
		}
	}
	if len(rows) > th {
		drawVBar(c.Sub(Rect{p.transcript.X + W - 1, p.transcript.Y, 1, th}), st, p.top, th, len(rows))
	}

	if ok {
		return &caret{cx, cy}
	}
	return nil
}

// layoutRows turns the transcript into visual rows for a width.
func (p *chatPane) layoutRows(st styles, w int) []chatRow {
	var rows []chatRow
	blank := func() { rows = append(rows, chatRow{}) }
	for i, msg := range p.msgs {
		switch msg.role {
		case roleUser:
			if i > 0 {
				blank()
			}
			for j, line := range wrap(msg.text, w-2) {
				pre := "  "
				if j == 0 {
					pre = "❯ "
				}
				rows = append(rows, chatRow{segs: []seg{{pre, st.chatUser}, {line, st.chatUser}}})
			}
		case roleAgent:
			rows = append(rows, p.agentRows(st, msg.text, w)...)
			last := i == len(p.msgs)-1
			if !(last && p.streaming) {
				rows = append(rows, chatRow{
					segs: []seg{{"⧉ copy reply", st.muted}},
					tgts: []rowTarget{{0, 12, targetCopyReply, msg.text}},
				})
			}
		case roleTool, roleNote:
			for _, line := range wrap(msg.text, w) {
				rows = append(rows, chatRow{segs: []seg{{line, st.chatTool}}})
			}
		case roleInfo:
			for _, line := range wrap(msg.text, w) {
				rows = append(rows, chatRow{segs: []seg{{line, st.muted.Italic()}}})
			}
		case roleErr:
			for _, line := range wrap(msg.text, w) {
				rows = append(rows, chatRow{segs: []seg{{line, st.err}}})
			}
		}
	}
	return rows
}

// agentRows renders an answer: paragraphs wrapped, `inline code` and
// **bold** styled, headings in the accent, and fenced code blocks on their
// own surface with ⤓ insert and ⧉ copy chips in their header row.
func (p *chatPane) agentRows(st styles, text string, w int) []chatRow {
	var rows []chatRow
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fence, ok := strings.CutPrefix(strings.TrimSpace(line), "```"); ok {
			lang := strings.TrimSpace(fence)
			var code []string
			j := i + 1
			for ; j < len(lines); j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
					break
				}
				code = append(code, lines[j])
			}
			i = j
			body := strings.Join(code, "\n")
			rows = append(rows, codeHeader(st, lang, body, w))
			for _, cl := range code {
				for _, part := range wrap(cl, w-2) {
					rows = append(rows, chatRow{segs: []seg{{" " + part, st.chatCode}}, fill: st.chatCode, hasF: true})
				}
			}
			continue
		}
		style := st.chatAgent
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			line = strings.TrimLeft(trimmed, "# ")
			style = st.accent.Bold()
		}
		if trimmed == "" {
			rows = append(rows, chatRow{})
			continue
		}
		inCode, inBold := false, false
		for _, part := range wrap(line, w) {
			var segs []seg
			segs, inCode, inBold = inline(part, style, st, inCode, inBold)
			rows = append(rows, chatRow{segs: segs})
		}
	}
	return rows
}

// codeHeader is a code block's title row: the language, then the chips.
func codeHeader(st styles, lang, body string, w int) chatRow {
	label := lang
	if label == "" {
		label = "code"
	}
	head := chatRow{fill: st.chatCode.WithFg(st.muted.Fg), hasF: true}
	head.segs = append(head.segs, seg{" " + label + " ", st.chatCode.WithFg(st.accent.Fg).Bold()})
	copyX := w - 8
	insX := copyX - 11
	if isSQLLang(lang) && insX > width(label)+3 {
		head.segs = append(head.segs, seg{strings.Repeat(" ", insX-width(label)-2), st.chatCode})
		head.segs = append(head.segs, seg{" ⤓ insert ", st.button})
		head.segs = append(head.segs, seg{" ", st.chatCode})
		head.tgts = append(head.tgts, rowTarget{insX, insX + 10, targetInsert, body})
	} else {
		head.segs = append(head.segs, seg{strings.Repeat(" ", max(copyX-width(label)-2, 1)), st.chatCode})
	}
	head.segs = append(head.segs, seg{" ⧉ copy ", st.button})
	head.tgts = append(head.tgts, rowTarget{copyX, copyX + 8, targetCopyCode, body})
	return head
}

// isSQLLang reports whether a fence's language tag means SQL. Untagged
// blocks count: models often omit the tag, and in a SQL assistant an
// untagged block is overwhelmingly SQL.
func isSQLLang(lang string) bool {
	switch strings.ToLower(lang) {
	case "", "sql", "postgres", "postgresql", "psql", "mysql", "sqlite", "pgsql", "plpgsql":
		return true
	}
	return false
}

// inline splits one wrapped line into styled segments for `code` and
// **bold**, carrying open spans across wrapped lines of the same paragraph.
func inline(line string, base Style, st styles, inCode, inBold bool) ([]seg, bool, bool) {
	var segs []seg
	var cur strings.Builder
	style := func() Style {
		s := base
		if inCode {
			s = st.chatCode.WithFg(st.warn.Fg)
		}
		if inBold {
			s = s.Bold()
		}
		return s
	}
	flush := func() {
		if cur.Len() > 0 {
			segs = append(segs, seg{cur.String(), style()})
			cur.Reset()
		}
	}
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '`':
			flush()
			inCode = !inCode
		case !inCode && strings.HasPrefix(line[i:], "**"):
			flush()
			inBold = !inBold
			i++
		default:
			cur.WriteByte(line[i])
		}
	}
	flush()
	return segs, inCode, inBold
}
