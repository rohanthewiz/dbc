package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/cats"
	"github.com/rohanthewiz/dbc/theme"
)

// The cats host integration, ported from the former tview UI (its
// cats_glue.go, catstheme.go, catsagents.go, metakeys.go and hostident.go,
// removed with it). The behavior and the reasons for it are unchanged; what
// changed is the plumbing:
//
//	tview                               Bubble Tea
//	a.catsPost(func(){…}) closure  →    a catsMsg delivered through m.send
//	QueueUpdateDraw                →    Update, which redraws after every message
//	tview SetTitle (OSC 2)         →    tea.View.WindowTitle
//	/dev/tty write for OSC 7       →    tea.Raw, through Bubble Tea's own output
//
// THE THREADING RULE carries over exactly: a goroutine started here touches
// only the values it was handed, and reaches the model only by sending a
// message. Everything that reads or writes Model fields happens in Update.
//
// TIER 0 IS THE ZERO VALUE. catsState's zero value is "not in cats, nothing
// connected", which is also what every failure path produces. Outside cats
// this whole file costs a few getenv calls at startup.
//
// WHAT THE HOOK REPORTER IS FOR: turning a working→idle transition into cats'
// "finished" badge, toast and phone push, so a four-minute query (or a long
// assistant answer) that ends while you are in another window reaches you.

type catsState struct {
	caps     cats.Caps
	client   *cats.Client
	reporter *cats.Reporter
	stream   *cats.Stream

	self   uint32
	selfOK bool

	panes   []cats.PaneInfo
	panesAt time.Time

	lastState, lastStatus string
}

// catsMsg is everything a cats goroutine reports back, one kind at a time.
type catsMsg struct {
	kind   catsMsgKind
	caps   cats.Caps
	self   uint32
	selfOK bool
	event  cats.Event
	up     bool
	panes  []cats.PaneInfo
	text   string // a log line to write (sent/failed)
	err    error
}

type catsMsgKind int

const (
	catsReady catsMsgKind = iota
	catsEvent
	catsLink
	catsPanes
	catsSent
)

// catsPanePollMin rate-limits pane.list refreshes.
const catsPanePollMin = 2 * time.Second

// catsInit detects the host, arms the reporter, and — inside cats — probes
// the control socket off the event loop.
func (m *Model) catsInit() tea.Cmd {
	env := cats.DetectEnv()
	m.cats.caps = env
	m.cats.reporter = cats.NewReporter(env.HookSocket, env.PaneHandle)
	m.catsReportNow() // claims the pane in cats' sidebar

	var cmds []tea.Cmd
	// OSC 7: where we are, so a new tab or split opens in the same place.
	// Tier 0 — any terminal that understands it benefits.
	if cwd, err := os.Getwd(); err == nil {
		cmds = append(cmds, tea.Raw(osc7CwdSeq(hostname(), cwd)))
	}
	if env.InCats && env.ControlSocket != "" {
		cmds = append(cmds, func() tea.Msg {
			caps := env.Probe()
			msg := catsMsg{kind: catsReady, caps: caps}
			if caps.Tier1() {
				if id, err := cats.NewClient(caps.ControlSocket).ResolvePane(caps.PaneHandle); err == nil {
					msg.self, msg.selfOK = id, true
				}
			}
			return msg
		})
	}
	return tea.Batch(cmds...)
}

// catsHandle lands one cats message.
func (m *Model) catsHandle(msg catsMsg) tea.Cmd {
	switch msg.kind {
	case catsReady:
		m.cats.caps, m.cats.self, m.cats.selfOK = msg.caps, msg.self, msg.selfOK
		if !msg.caps.Tier1() {
			if msg.caps.InCats && msg.caps.Reason != "" {
				m.logf(logMuted, "cats: %s — running standalone", msg.caps.Reason)
			}
			return nil
		}
		m.cats.client = cats.NewClient(msg.caps.ControlSocket)
		m.logf(logOk, "cats: connected — pane %s, host %s", msg.caps.PaneHandle, msg.caps.Service)
		m.log(logMuted, "^G hands the current statement to an agent in another pane")
		m.catsSubscribe()
		return m.catsPollPanes(true)
	case catsEvent:
		return m.catsFrame(msg.event)
	case catsLink:
		m.cats.caps.Control = msg.up
	case catsPanes:
		m.cats.panes = msg.panes
	case catsSent:
		if msg.err != nil {
			m.logf(logErr, "%s: %s", msg.text, msg.err)
		} else {
			m.log(logOk, msg.text)
		}
	}
	return nil
}

func (m *Model) catsTier1() bool { return m.cats.client != nil && m.cats.caps.Tier1() }

// catsSubscribe opens the event stream. The filter names events, never a
// pane: theme_changed is session-scoped and emitted against pane 0.
func (m *Model) catsSubscribe() {
	if m.cats.stream != nil || !m.catsTier1() {
		return
	}
	send := m.send
	m.cats.stream = cats.Subscribe(m.cats.caps.ControlSocket,
		cats.SubscribeFilter{Events: []string{
			cats.EventThemeChanged, cats.EventPaneAgent, cats.EventPaneNotify,
			cats.EventPaneAdded, cats.EventPaneRemoved, cats.EventFocusChanged,
		}},
		func(ev cats.Event) { send(catsMsg{kind: catsEvent, event: ev}) },
		func(up bool, _ error) { send(catsMsg{kind: catsLink, up: up}) })
}

// catsFrame dispatches one event. Unknown names are ignored: the vocabulary
// grows on the host's schedule.
func (m *Model) catsFrame(ev cats.Event) tea.Cmd {
	switch ev.Name {
	case cats.EventThemeChanged:
		var t cats.ThemeChangedEvent
		if json.Unmarshal(ev.Data, &t) != nil {
			return nil
		}
		// cats rebroadcasts its theme after any config change; a palette
		// that did not change costs nothing
		if p, ok := theme.FromHost(t.Colors); ok && p != m.st.pal {
			m.st = newStyles(p)
			name := t.Name
			if name == "" {
				name = "host"
			}
			m.logf(logOk, "theme synced to %s", name)
		}
	case cats.EventPaneAgent, cats.EventPaneNotify, cats.EventPaneAdded,
		cats.EventPaneRemoved, cats.EventFocusChanged:
		return m.catsPollPanes(false)
	}
	return nil
}

// catsPollPanes refreshes the cached pane list, rate-limited.
func (m *Model) catsPollPanes(force bool) tea.Cmd {
	if !m.catsTier1() || (!force && time.Since(m.cats.panesAt) < catsPanePollMin) {
		return nil
	}
	m.cats.panesAt = time.Now()
	client := m.cats.client
	return func() tea.Msg {
		panes, err := client.PaneList()
		if err != nil {
			return nil // a stale list beats none
		}
		return catsMsg{kind: catsPanes, panes: panes}
	}
}

// catsSelfState maps dbc's state onto the hook vocabulary. A running query
// is work; so is an assistant answer in flight, because that too is
// something the user may walk away from and want to hear the end of.
func (m *Model) catsSelfState() (state, status string) {
	switch {
	case m.ws.Busy():
		return cats.StateWorking, m.ws.RunTag()
	case m.chat.streaming:
		return cats.StateWorking, "assistant answering"
	}
	return cats.StateIdle, ""
}

// catsReportNow publishes the state if it changed — every report is a
// potential toast, and a channel that fires for nothing gets muted.
func (m *Model) catsReportNow() {
	if m.cats.reporter == nil {
		return
	}
	state, status := m.catsSelfState()
	if state == m.cats.lastState && status == m.cats.lastStatus {
		return
	}
	m.cats.lastState, m.cats.lastStatus = state, status
	m.cats.reporter.ReportState(state, status)
}

// catsAfterTransition is the one call every run-state change makes. (The
// window title follows by itself: View recomputes it every frame.)
func (m *Model) catsAfterTransition() { m.catsReportNow() }

// catsClose hands the pane back: the stream stops first, so no callback is
// still posting, then the pane is released.
func (m *Model) catsClose() {
	m.cats.stream.Close() // nil-safe
	m.cats.stream = nil
	m.cats.reporter.Release() // nil-safe
}

// ---------------------------------------------------------------------------
// Theme at startup
// ---------------------------------------------------------------------------

// startupPalette holds the host palette fetched before the first frame.
var startupPalette *theme.Palette

// catsThemeAtStartup fetches the host's palette synchronously, before the
// first frame, so launching inside cats does not flash dbc's own colors and
// then repaint. Bounded by the probe timeout, and only ever inside cats.
func catsThemeAtStartup() {
	env := cats.DetectEnv()
	if !env.InCats || env.ControlSocket == "" {
		return
	}
	client := &cats.Client{Socket: env.ControlSocket, Timeout: cats.ProbeTimeout}
	res, err := client.ConfigGet()
	if err != nil {
		return
	}
	if p, ok := theme.FromHost(res.Theme.Colors); ok {
		startupPalette = &p
	}
}

func catsStartupPalette() (theme.Palette, bool) {
	if startupPalette == nil {
		return theme.Palette{}, false
	}
	return *startupPalette, true
}

// ---------------------------------------------------------------------------
// ^G — hand the statement to an agent in another pane
// ---------------------------------------------------------------------------

// catsAgentRank orders the picker: a blocked agent is the one worth
// interrupting.
func catsAgentRank(state string) int {
	switch state {
	case cats.StateBlocked:
		return 3
	case cats.StateWorking:
		return 2
	case cats.StateIdle:
		return 1
	}
	return 0
}

// catsAgentPanes is the cached list minus our own pane, most-blocked first.
func (m *Model) catsAgentPanes() []cats.PaneInfo {
	var out []cats.PaneInfo
	for _, p := range m.cats.panes {
		if p.Agent == "" || m.catsIsSelf(p) {
			continue
		}
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			ri, rj := catsAgentRank(out[j].AgentState), catsAgentRank(out[j-1].AgentState)
			if ri < rj || (ri == rj && out[j].Pane > out[j-1].Pane) {
				break
			}
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (m *Model) catsIsSelf(p cats.PaneInfo) bool {
	if m.cats.selfOK && p.Pane == m.cats.self {
		return true
	}
	return p.Handle != "" && p.Handle == m.cats.caps.PaneHandle
}

// catsQuestion is what ^G sends: the statement Ctrl+R would run, with its
// connection and, when it failed, the error.
func (m *Model) catsQuestion() string {
	stmts, _ := m.stmtsToRun()
	if len(stmts) == 0 {
		return ""
	}
	sql := strings.Join(stmts, ";\n")
	var b strings.Builder
	if cc, ok := m.cfg.ConnByName(m.ws.Active()); ok {
		fmt.Fprintf(&b, "In dbc, on %s (%s):\n\n", cc.Name, cc.Driver)
	} else {
		b.WriteString("In dbc:\n\n")
	}
	b.WriteString("```sql\n" + sql + "\n```\n")
	if m.ws.LastErr() != "" && sql == m.ws.LastStmt() {
		b.WriteString("\nIt failed with: " + m.ws.LastErr() + "\n")
	}
	return b.String()
}

// openCatsAgents is Ctrl+G. The text is STAGED in the agent's prompt, never
// submitted: putting a question in an agent's mouth is one thing, running it
// is another.
func (m *Model) openCatsAgents() tea.Cmd {
	if !m.catsTier1() {
		m.log(logWarn, "handing a statement to another pane's agent needs cats — "+
			"use ✦ Assistant (Ctrl+A) to ask here instead")
		return nil
	}
	q := m.catsQuestion()
	if q == "" {
		m.log(logWarn, "nothing to ask about — put the caret in a statement first")
		return nil
	}
	items := []menuItem{heading("hand this statement to")}
	for _, p := range m.catsAgentPanes() {
		p := p
		where := p.Handle
		if where == "" {
			where = fmt.Sprintf("pane %d", p.Pane)
		}
		state := p.AgentState
		if state == "" {
			state = cats.StateUnknown
		}
		items = append(items, menuItem{label: p.Agent, key: state + " · " + where,
			act: func(m *Model) tea.Cmd { return m.catsSendToPane(p, q) }})
	}
	items = append(items, menuItem{label: "cats chat", key: "the host's panel",
		act: func(m *Model) tea.Cmd { return m.catsSendToChat(q) }})
	m.openMenu(m.w/2-20, m.h/3, items)
	return m.catsPollPanes(false) // refresh for the next open
}

// catsSendToPane stages the question in an agent's pane and focuses it.
func (m *Model) catsSendToPane(p cats.PaneInfo, question string) tea.Cmd {
	client, name := m.cats.client, p.Agent
	return func() tea.Msg {
		err := client.PaneSendInput(p.Pane, question, false)
		if err == nil {
			err = client.PaneFocus(p.Pane)
		}
		if err != nil {
			return catsMsg{kind: catsSent, text: "could not reach " + name, err: err}
		}
		return catsMsg{kind: catsSent, text: "sent to " + name + " — it is waiting there unsent, press Enter to ask"}
	}
}

// catsSendToChat posts the question to cats' own chat panel, which submits.
func (m *Model) catsSendToChat(question string) tea.Cmd {
	client := m.cats.client
	return func() tea.Msg {
		if err := client.ChatSend(question); err != nil {
			return catsMsg{kind: catsSent, text: "could not reach cats chat", err: err}
		}
		return catsMsg{kind: catsSent, text: "asked cats chat"}
	}
}

// ---------------------------------------------------------------------------
// ⌘ accelerators
// ---------------------------------------------------------------------------

// metaAccels is the ⌘ table. Every row is a second door onto a verb that has
// a Ctrl chord (the twin), so nothing is ⌘-only; the chords are ones cats
// forwards to panes (its CMD_TO_PANE allowlist).
var metaAccels = []struct {
	key  rune
	twin string
	fire func(*Model) tea.Cmd
}{
	{'e', "ctrl+e", func(m *Model) tea.Cmd { m.openExport(); return nil }},
	{'p', "ctrl+p", func(m *Model) tea.Cmd { m.openHistory(); return nil }},
	{'g', "ctrl+g", func(m *Model) tea.Cmd { return m.openCatsAgents() }},
}

// metaAccel answers a ⌘ chord. It only ACTS on the main screen; over a modal
// it is swallowed, so an unclaimed chord never types its letter into an open
// field. Unarmed (⌥ may be sending Meta here), the key is left alone.
func (m *Model) metaAccel(k tea.KeyPressMsg) (tea.Cmd, bool) {
	if k.Mod&tea.ModSuper == 0 || !m.metaAccelArmed() {
		return nil, false
	}
	r := unicode.ToLower(k.Code)
	shift := k.Mod&tea.ModShift != 0 || unicode.IsUpper(k.Code)
	if m.modal == nil && m.menu == nil && !shift {
		for _, a := range metaAccels {
			if a.key == r {
				return a.fire(m), true
			}
		}
	}
	return nil, true
}

// metaAccelArmed reports whether ⌘ can be trusted to mean ⌘: inside cats, or
// in a terminal that speaks the kitty protocol and has no Option-as-Meta
// setting to confuse it with (see ui/metakeys.go for the full reasoning).
func (m *Model) metaAccelArmed() bool {
	if m.catsTier1() {
		return true
	}
	term, prog := os.Getenv("TERM"), os.Getenv("TERM_PROGRAM")
	switch {
	case term == "xterm-kitty" || os.Getenv("KITTY_WINDOW_ID") != "":
		return true
	case term == "xterm-ghostty" || strings.EqualFold(prog, "ghostty") || os.Getenv("GHOSTTY_RESOURCES_DIR") != "":
		return true
	case strings.EqualFold(prog, "WezTerm") || os.Getenv("WEZTERM_PANE") != "":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Host identity: OSC 7
// ---------------------------------------------------------------------------

// osc7CwdSeq reports the working directory as a file URL.
func osc7CwdSeq(host, dir string) string {
	return fmt.Sprintf("\x1b]7;file://%s%s\x07", host, fileURLPath(dir))
}

// fileURLPath percent-encodes a path for a file:// URL, including any control
// byte that would otherwise cut the sequence short.
func fileURLPath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '/', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// hostname is the URL's authority; unknown is the empty authority, which a
// file URL reads as "this machine".
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return titleSafe(h)
}

// titleSafe strips control characters from text bound for a terminal escape:
// connection names and script names are user-controlled, and an embedded ESC
// or BEL would end the sequence early.
func titleSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}
