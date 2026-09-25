package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/theme"
	"github.com/rohanthewiz/dbc/userdata"
)

// Model is the whole application state, and the Bubble Tea model.
//
// ONE MODEL, PLAIN WIDGETS. As in cats-todo, there are no nested tea.Models:
// the editor, grid, lists and panes are ordinary structs the Model owns and
// calls. Messages are routed here, in one place, which is what lets a mouse
// click decide between "focus this pane", "start dragging a splitter" and
// "dismiss a menu" before any widget sees it.
//
// THE THREADING RULE. Only Update mutates the Model. Work that blocks — a
// query, a connect, a clipboard write, the AI agent — runs as a tea.Cmd (a
// function Bubble Tea calls on its own goroutine) and reports back with a
// message. The one exception is the database session, which a running
// command uses while Update may be starting the next run's bookkeeping; it
// has its own mutex (sessMu), exactly as the former tview UI's did.
//
// It is a pointer receiver throughout: the model is large, and every Update
// returning a copy of it would copy the editor buffer and the result on
// every mouse motion.
type Model struct {
	cfg *config.Config
	mgr *db.Manager
	st  styles

	w, h int
	lay  layout

	focus  focusID
	editor *editor
	grid   *grid
	logp   *logPane
	conns  *list
	tables *list
	chat   *chatPane

	// Overlays, drawn on top of everything. At most one menu and one modal;
	// a menu may sit over a modal (a right-click inside it), never the
	// reverse.
	menu  *menu
	modal modal

	active     string         // active connection
	tableRes   *model.Result  // the active connection's catalog, for the sidebar
	tableIdx   *db.TableIndex // the same catalog, indexed for the assistant's schema context
	lastRes    *model.Result  // the result in the grid
	lastStmt   string         // the statement the last run executed
	lastErr    string         // what it failed with, "" if it worked
	hist       *userdata.History
	histWarned bool

	// run state — one run at a time, as in the former tview UI
	busy    bool
	runGen  int // bumped per run; a late message from an older run is dropped
	runTag  string
	runAt   time.Time
	cancel  context.CancelFunc
	sessMu  sync.Mutex
	sess    *db.Session
	sessFor string

	// connect state. A connect runs as a command so the UI stays live while
	// it dials; these let Ctrl+K / Ctrl+C abandon it, and let a newer pick
	// supersede an older one. Touched only on the Update goroutine.
	connGen    int                // bumped per connect; a connectMsg from an older one is dropped
	connCancel context.CancelFunc // cancels the connect in flight; nil when none is
	connName   string             // what it is connecting to, for the log

	// sizes the user can change by dragging a pane border
	sideW  int
	chatW  int
	logH   int
	edFrac float64 // the editor's share of the centre column above the log

	drag  dragState
	click clickState
	hover hoverState

	status string // left half of the status bar

	// send delivers a message to the running program from outside a Cmd —
	// the script engine's show/print callbacks, which fire mid-run. Run
	// installs Program.Send; tests install a recorder.
	send func(tea.Msg)

	// cats is everything about the host this may be running inside.
	cats catsState

	aiAgent ai.Agent // the configured assistant backend
	quit    bool
}

// focusID is which pane has the keyboard.
type focusID int

const (
	focusEditor focusID = iota
	focusGrid
	focusConns
	focusTables
	focusLog
	focusChat
)

// Tab cycles through the panes in this order; the chat joins only while its
// pane is open.
var focusOrder = []focusID{focusEditor, focusGrid, focusConns, focusTables, focusLog, focusChat}

// Options configure New beyond what the config holds.
type Options struct {
	// NoPersist keeps the history, buffer and conversations off disk —
	// tests, which must not read or clobber the developer's real
	// ~/.config/dbc.
	NoPersist bool
}

// New builds the model. It does no IO beyond reading the history and saved
// buffer; connecting happens in Init's command.
func New(cfg *config.Config, mgr *db.Manager, opt Options) *Model {
	m := &Model{
		cfg: cfg, mgr: mgr,
		st:     newStyles(theme.Default()),
		editor: newEditor(false),
		grid:   newGrid(),
		logp:   newLogPane(),
		conns:  newList(),
		tables: newList(),
		sideW:  26,
		logH:   7,
		edFrac: 0.35,
		send:   func(tea.Msg) {},
	}
	m.chat = newChatPane()
	m.editor.sqlMode = true
	m.editor.placeholder = "Type SQL here, then Ctrl+R (or ▶ Run) to run it…"
	m.aiAgent, _ = ai.AgentByID(cfg.AIAgent)

	if opt.NoPersist {
		m.hist = userdata.LoadHistory("")
	} else {
		m.hist = userdata.LoadHistory(userdata.HistoryFile())
		m.chat.dir = userdata.ChatsDir()
	}

	if _, ok := cfg.ConnByName(cfg.DefaultConnection); ok {
		m.active = cfg.DefaultConnection
	} else if len(cfg.Connections) > 0 {
		m.active = cfg.Connections[0].Name
	}
	m.refreshConns()

	saved := ""
	if !opt.NoPersist {
		saved = userdata.LoadBuffer(userdata.BufferFile())
	}
	switch {
	case strings.TrimSpace(saved) != "":
		// the scratchpad from last time beats the demo sample
		m.editor.SetText(saved)
	case cfg.Demo:
		m.editor.SetText("SELECT id, name, breed, age, adopted FROM cats ORDER BY age")
	}

	m.startupLog()
	m.setStatus("ready")
	return m
}

// startupLog writes what a new user needs to know once: where the config
// came from, the demo connections, and the keys.
func (m *Model) startupLog() {
	if m.cfg.Demo {
		names := make([]string, 0, len(m.cfg.Connections))
		for _, c := range m.cfg.Connections {
			n := c.Name
			if c.Name == m.active {
				n += " (active)"
			}
			names = append(names, n)
		}
		m.logf(logInfo, "no config found — using the built-in demo connections: %s (see dbc.example.toml)",
			strings.Join(names, ", "))
		m.log(logAccent, "press Ctrl+R or click ▶ Run to run the query")
	} else if m.cfg.Path != "" {
		m.logf(logInfo, "loaded config from %s", m.cfg.Path)
	}
	m.log(logMuted, "keys: ^R run · ^K stop · ^A assistant · ^E export · ^P history · ^O scripts · "+
		"^T tables · ^L conns · Tab focus · y/Y/c copy · Enter inspect · -/+ hide/show column · ^Q quit")
	m.log(logMuted, "mouse: click to focus · drag to select · right-click for menus · "+
		"drag borders to resize · hold Shift (⌥ on macOS) to select terminal text")
	for _, w := range m.cfg.Warnings {
		m.log(logWarn, w)
	}
}

// Init starts the first connect, which also fills the tables sidebar.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.connectCmd(m.active), m.catsInit())
}

// Run builds the model and runs the program until the user quits.
func Run(cfg *config.Config, mgr *db.Manager) error {
	catsThemeAtStartup() // before New, so the first frame is already in the host's colors
	m := New(cfg, mgr, Options{})
	if p, ok := catsStartupPalette(); ok {
		m.st = newStyles(p)
	}
	p := tea.NewProgram(m)
	m.send = p.Send
	_, err := p.Run()

	m.shutdown()
	if werr := userdata.SaveBuffer(userdata.BufferFile(), m.editor.Text()); werr != nil {
		fmt.Fprintf(os.Stderr, "could not save the editor buffer: %v\n", werr)
	}
	return err
}

// shutdown releases everything the session holds, in the order that keeps
// each step from blocking on the next: stop the run, hand the pane back to
// cats, save and stop the assistant, then release the pinned connection. A
// connect still dialing is abandoned with the run.
func (m *Model) shutdown() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.connCancel != nil {
		m.connCancel()
	}
	m.catsClose()
	m.chatSave() // before close: quitting must not discard the conversation
	m.chat.close()
	m.dropSession()
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

// Update routes one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.route(msg)
	if m.quit {
		return m, tea.Quit
	}
	return m, cmd
}

func (m *Model) route(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.menu = nil // a menu placed for the old size may now be off screen
		return nil
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.MouseClickMsg:
		return m.mouseClick(msg)
	case tea.MouseReleaseMsg:
		return m.mouseRelease(msg)
	case tea.MouseMotionMsg:
		return m.mouseMotion(msg)
	case tea.MouseWheelMsg:
		return m.mouseWheel(msg)
	case tea.PasteMsg:
		return m.paste(msg.Content)
	case tea.BlurMsg:
		m.hover = hoverState{}
		return nil

	case connectMsg:
		return m.connected(msg)
	case sessionReleasedMsg:
		return m.sessionReleased(msg)
	case runDoneMsg:
		return m.runDone(msg)
	case tickMsg:
		return m.tick(msg)
	case scriptShowMsg:
		m.showResult(msg.res)
		return nil
	case scriptPrintMsg:
		m.log(logInfo, msg.text)
		return nil
	case clipDoneMsg:
		return m.clipDone(msg)
	case chatEventMsg:
		return m.chatEvent(msg)
	case chatModelSetMsg:
		return m.chatModelSet(msg)
	case chatSchemaMsg:
		return m.chatSchema(msg)
	case catsMsg:
		return m.catsHandle(msg)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------

// key routes a key press: overlays first, then the app's chords, then the
// focused pane.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()

	// A ⌘ chord from the cats host is answered on every path, modal or not.
	if cmd, ok := m.metaAccel(k); ok {
		return cmd
	}

	if m.menu != nil {
		return m.menuKey(k)
	}
	if m.modal != nil {
		switch s {
		case "ctrl+c":
			return m.interrupt()
		case "ctrl+q":
			return m.quitCmd()
		}
		cmd, closed := m.modal.key(m, k)
		if closed {
			m.modal = nil
		}
		return cmd
	}

	switch s {
	case "ctrl+r":
		return m.runQuery()
	case "ctrl+k":
		if m.focus == focusChat && m.chat.busy() {
			m.chatStop()
			return nil
		}
		return m.cancelRun()
	case "ctrl+c":
		return m.interrupt()
	case "ctrl+q":
		return m.quitCmd()
	case "ctrl+e":
		m.openExport()
		return nil
	case "ctrl+o":
		m.openScripts()
		return nil
	case "ctrl+p":
		m.openHistory()
		return nil
	case "ctrl+t":
		return m.listTables()
	case "ctrl+l":
		m.focus = focusConns
		return nil
	case "ctrl+a":
		return m.toggleChatFocus()
	case "ctrl+g":
		return m.openCatsAgents()
	case "tab":
		m.cycleFocus(1)
		return nil
	case "shift+tab":
		m.cycleFocus(-1)
		return nil
	}

	switch m.focus {
	case focusEditor:
		m.editor.HandleKey(k)
		m.drag.follow = true
	case focusGrid:
		return m.gridKey(k)
	case focusConns:
		return m.listKey(m.conns, k, m.connPicked)
	case focusTables:
		return m.listKey(m.tables, k, m.tablePicked)
	case focusLog:
		m.logp.key(k)
	case focusChat:
		return m.chatKey(k)
	}
	return nil
}

// gridKey handles the results grid's own keys: the copy keys, inspect, the
// context menu, and column width and visibility, then movement.
func (m *Model) gridKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "<":
		m.grid.Resize(m.grid.cur.col, -2)
		return nil
	case ">":
		m.grid.Resize(m.grid.cur.col, 2)
		return nil
	case "=":
		m.grid.Fit(m.grid.cur.col)
		return nil
	case "-":
		m.hideColumns()
		return nil
	case "+":
		m.showAllColumns()
		return nil
	case "y":
		return m.copyGrid(copyText, false)
	case "Y":
		r, what := m.grid.RowResult()
		return m.copyResult(r, what, copyText)
	case "c", "menu":
		x, y := m.grid.cursorScreen()
		m.openGridMenu(x, y+1) // just below the cell, so it stays visible
		return nil
	case "enter":
		m.openInspect()
		return nil
	}
	m.grid.HandleKey(k)
	return nil
}

// hideColumns hides the columns of the range selection, or the cursor's
// column, and says what it did — a column vanishing without a word would
// read as a rendering bug.
func (m *Model) hideColumns() {
	g := m.grid
	if g.Cols() == 0 {
		m.log(logWarn, noResult)
		return
	}
	_, c0, _, c1 := g.bounds()
	what := g.colName(c0)
	if c1 > c0 {
		what = plural(c1-c0+1, "column")
	}
	if g.Hide(c0, c1) == 0 {
		m.log(logWarn, "can't hide every column — show some first (+), or narrow the range")
		return
	}
	m.logf(logInfo, "hid %s — + or the right-click menu shows it again; copies leave hidden columns out", what)
}

// showAllColumns brings back every hidden column.
func (m *Model) showAllColumns() {
	if n := m.grid.ShowAll(); n > 0 {
		m.logf(logInfo, "showing %s again", plural(n, "hidden column"))
		return
	}
	m.log(logInfo, "no columns are hidden")
}

// cycleFocus moves the keyboard to the next pane in focusOrder, skipping the
// chat while its pane is closed and the sidebar while it is hidden.
func (m *Model) cycleFocus(d int) {
	cur := 0
	for i, f := range focusOrder {
		if f == m.focus {
			cur = i
		}
	}
	for range focusOrder {
		cur = (cur + d + len(focusOrder)) % len(focusOrder)
		f := focusOrder[cur]
		if f == focusChat && !m.chat.open {
			continue
		}
		if (f == focusConns || f == focusTables) && m.lay.conns.Empty() {
			continue
		}
		m.focus = f
		return
	}
}

// paste inserts pasted text into whatever text field has the keyboard.
func (m *Model) paste(s string) tea.Cmd {
	switch {
	case m.modal != nil:
		m.modal.paste(m, s)
	case m.focus == focusChat:
		m.chat.input.Insert(s)
	case m.focus == focusEditor:
		m.editor.Insert(s)
		m.drag.follow = true
	}
	return nil
}

// interrupt is Ctrl+C: stop what is running — a query, or the assistant's
// answer — and quit only when nothing is. psql muscle memory expects Ctrl+C
// to cancel, and losing the session to it would be a nasty surprise.
func (m *Model) interrupt() tea.Cmd {
	if m.busy {
		return m.cancelRun()
	}
	if m.cancelConnect() {
		return nil // a connect in flight is "something running": stop it, don't quit
	}
	if m.chat.busy() {
		m.chatStop()
		return nil
	}
	return m.quitCmd()
}

// quitCmd cancels anything in flight and ends the program.
func (m *Model) quitCmd() tea.Cmd {
	if m.cancel != nil {
		m.cancel()
	}
	m.quit = true
	return nil
}

// ---------------------------------------------------------------------------
// Status and log
// ---------------------------------------------------------------------------

// setStatus sets the left half of the status bar; the active connection is
// always drawn in front of it.
func (m *Model) setStatus(text string) { m.status = text }

// log appends a line to the log pane.
func (m *Model) log(kind logKind, text string) { m.logp.add(kind, text) }

// logf is log with formatting.
func (m *Model) logf(kind logKind, format string, args ...any) {
	m.logp.add(kind, fmt.Sprintf(format, args...))
}
