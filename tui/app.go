package tui

import (
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/theme"
	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/workspace"
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
// message. The database side of that — the active connection, the run slot,
// the pinned session, the last statement, error, result and plan — is not
// the Model's at all but the workspace's (ws), which the browser UI shares
// and which guards itself with its own mutexes: a run's Job lands its
// outcome there from the command goroutine, and Update draws the event.
//
// It is a pointer receiver throughout: the model is large, and every Update
// returning a copy of it would copy the editor buffer and the result on
// every mouse motion.
type Model struct {
	cfg *config.Config
	mgr *db.Manager
	st  styles

	// saved is the saved-connections file (connections.toml) the
	// connection form adds to, edits and removes from (connform.go);
	// memory-only under Options.NoPersist.
	saved *config.SavedStore

	w, h int
	lay  layout

	focus  focusID
	editor *editor
	grid   *grid
	planv  *planView // the results pane's Plan tab
	// logp is the log on screen: the shown connection's, one of logs
	// (logs.go), keyed by connection ("" for none: the holding log);
	// logFor is whose it is. syncLog keeps logp in step with the tab.
	logp   *logPane
	logs   map[string]*logPane
	logFor string
	conns  *list
	tables *list
	chat   *chatPane

	// Overlays, drawn on top of everything. At most one menu and one modal;
	// a menu may sit over a modal (a right-click inside it), never the
	// reverse.
	menu  *menu
	modal modal
	// compl is the editor's completion popup (complete.go), nil when
	// closed; complSt its bookkeeping while the schema loads.
	compl   *complPopup
	complSt complState

	// ws is the database side: the active connection and its catalog, the
	// run slot (one run at a time), the pinned session, connects, history,
	// and the last statement, error, result and plan. The TUI reads it for
	// drawing and calls it to start work; see package workspace.
	ws *workspace.Workspace

	// tabs are the query tabs (tabs.go); tabs[curTab] is the one on screen,
	// whose workspace and widgets are the fields above (m.ws, m.editor, …).
	// nextTabKey numbers new tabs for tagging their events.
	tabs       []*queryTab
	curTab     int
	nextTabKey int
	// savedLayoutTabs / savedActiveTab are the tabs the last run saved,
	// held from restoreLayout until New can rebuild them (restoreTabs).
	savedLayoutTabs []userdata.LayoutTab
	savedActiveTab  int
	// connTestSeq numbers connection-form tests across forms (connform.go)
	connTestSeq int

	resTab  resultsTab // which tab the results pane shows: the grid or the plan
	resZoom bool       // the results pane has the whole centre column (z)

	// The results pane per connection (resulttabs.go). planFor is the
	// connection m.planv and m.resTab belong to; plans parks the other
	// connections' plan views. gridConn and gridFor are the connection
	// and result-tab ID m.grid shows (gridFor 0: no tab), and rgrids parks
	// the grids of the other result tabs, by connection then ID. All four
	// are the query tab's, swapped with it in park / load.
	planFor  string
	plans    map[string]connPane
	gridConn string
	gridFor  int
	rgrids   map[string]map[int]*grid

	// sizes the user can change by dragging a pane border
	sideW  int
	chatW  int
	logH   int
	edFrac float64 // the editor's share of the centre column above the log

	// sideHidden folds the sidebar away (^B, or the ‹ / › tabs), as dbc
	// web's Ctrl+B does: the work column takes its width. It is separate
	// from the narrow-terminal rule (sidebarWidth), which hides the sidebar
	// without the user asking and brings it back on a wider window.
	sideHidden bool
	// layoutFile is where the pane sizes and the fold persist ("" under
	// Options.NoPersist); see restoreLayout.
	layoutFile string

	drag  dragState
	click clickState
	hover hoverState

	status string // left half of the status bar

	// schemaPicks is the schema pick last made on each connection (derived
	// ones included): a switch back to a database, or the next start of
	// dbc, reopens the schema the user was in. See navigator.go.
	schemaPicks map[string]workspace.SchemaPick
	// picksFile is where schemaPicks persist ("" under Options.NoPersist:
	// they then last only for the run).
	picksFile string
	// schemaLoading names the schema pick in flight ("" when none), for the
	// schema row's "loading…" until its tables land.
	schemaLoading string

	// consoleDir is where the per-database SQL consoles live ("" under
	// Options.NoPersist: the editor then keeps one buffer that is never
	// swapped or saved). console is the file of the one in the editor now,
	// "" before any; consoleDB and consoleName say which it is, and
	// consoleText is its text as last loaded or saved, so an unchanged
	// console is not written back. lastConsole is the console last open on
	// each database this run, which a switch back reopens. consoleViews
	// parks each console left this run (by file) with its caret, scroll
	// and undo, for the switch back. See console.go.
	consoleDir   string
	console      string
	consoleDB    userdata.ConsoleDB
	consoleName  string
	consoleText  string
	lastConsole  map[userdata.ConsoleDB]string
	consoleViews map[string]editorView

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
	// NoPersist keeps the history, consoles and conversations off disk —
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
		planv:  newPlanView(),
		conns:  newList(),
		tables: newList(),
		sideW:  26,
		logH:   7,
		edFrac: 0.35,
		send:   func(tea.Msg) {},

		schemaPicks: map[string]workspace.SchemaPick{},
	}
	m.chat = newChatPane()
	m.editor.sqlMode = true
	m.editor.placeholder = "Type SQL here, then Ctrl+R (or ▶ Run) to run it…"
	m.aiAgent, _ = ai.AgentByID(cfg.AIAgent)

	hist := userdata.LoadHistory("")
	m.saved = config.OpenSaved("")
	if !opt.NoPersist {
		m.saved = config.OpenSaved(config.SavedFile())
		hist = userdata.LoadHistory(userdata.HistoryFile())
		m.chat.dir = userdata.ChatsDir()
		m.loadPicks(userdata.PicksFile())
		m.layoutFile = userdata.LayoutFile()
		m.restoreLayout(userdata.LoadLayout(m.layoutFile))
	}
	// the first query tab is the workspace and widgets built here; see
	// newWorkspace for the sink and the catalog choice
	m.initTabs()
	m.ws = m.newWorkspace(m.active().key, hist)
	m.refreshConns()

	// The editor opens on the console of the connection about to be
	// connected (console.go), so the first connect — landing on that same
	// database — has nothing to swap.
	saved := ""
	if !opt.NoPersist {
		m.consoleDir = userdata.ConsolesDir()
		saved = m.openConsole(m.ws.Active(), userdata.BufferFile())
	}
	switch {
	case strings.TrimSpace(saved) != "":
		// the scratchpad from last time beats the demo sample
		m.editor.SetText(saved)
	case cfg.Demo:
		m.editor.SetText("SELECT id, name, breed, age, adopted FROM cats ORDER BY age")
	}
	m.restoreTabs(m.savedLayoutTabs, m.savedActiveTab)

	m.startupLog()
	m.setStatus("ready")
	return m
}

// loadPicks makes path where schema picks persist, and takes the picks saved
// there as the ones last made (see schemaPicks).
func (m *Model) loadPicks(path string) {
	m.picksFile = path
	for conn, p := range userdata.LoadPicks(path) {
		m.schemaPicks[conn] = workspace.SchemaPick{Name: p.Name, All: p.All}
	}
}

// startupLog writes what a new user needs to know once: where the config
// came from, the demo connections, and the keys.
func (m *Model) startupLog() {
	if m.cfg.Demo {
		names := make([]string, 0, len(m.cfg.Connections))
		for _, c := range m.cfg.Connections {
			n := c.Name
			if c.Name == m.ws.Active() {
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
	m.log(logMuted, "keys: ^R run · ^⇧R/⌥R run all · ^X explain · ⌥X explain analyze · ^K stop · ^A assistant · ^E export · ^P history · ^O scripts · "+
		"^Space suggest · F12/⇧F12/F2 definition/uses/rename · ⌥T/⌥W/⌥1…9 tabs · ⌥N/⌥C new/next console · ^T tables · ^L conns · ^B sidebar · F1 keys · x/r disconnect/refresh · a/e add/edit connection · d/s database/schema · Tab focus · y/Y/c copy · t transpose · Enter inspect · -/+ hide/show column · { } P S x result tabs · ^Q quit")
	m.log(logMuted, "mouse: click to focus · drag to select · right-click for menus · "+
		"drag borders to resize · hold Shift (⌥ on macOS) to select terminal text")
	for _, w := range m.cfg.Warnings {
		m.log(logWarn, w)
	}
}

// Init starts the first connect, which also fills the tables sidebar.
func (m *Model) Init() tea.Cmd {
	// a restored tab connects to the connection it was saved on (lazy, see
	// restoreTabs); otherwise the workspace's default
	conn := m.ws.Active()
	if t := m.active(); t.lazy != "" {
		conn, t.lazy = t.lazy, ""
	}
	return tea.Batch(m.connectCmd(conn), m.catsInit())
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
	// the MySQL driver's own log lines would draw over the screen; they
	// go to the log pane instead (see db.SetDriverLog)
	db.SetDriverLog(lineWriter(func(line string) { m.send(driverLogMsg(line)) }))
	defer db.SetDriverLog(nil)
	_, err := p.Run()

	m.shutdown()
	m.saveLayout()
	m.forEachTab(func() {
		if werr := m.saveConsole(); werr != nil {
			fmt.Fprintf(os.Stderr, "could not save the editor buffer: %v\n", werr)
		}
	})
	return err
}

// driverLogMsg is one line a database driver logged (db.SetDriverLog).
type driverLogMsg string

// lineWriter is an io.Writer that hands each complete line written to it
// to emit, without its newline. The driver's logger writes one whole line
// per call; a write without a trailing newline is taken as a line too,
// rather than held back for a newline that may never come.
type lineWriter func(string)

func (f lineWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			f(line)
		}
	}
	return len(p), nil
}

// shutdown releases everything the session holds, in the order that keeps
// each step from blocking on the next: stop the run, hand the pane back to
// cats, save and stop the assistant, then release the pinned connection. A
// connect still dialing is abandoned with the run.
func (m *Model) shutdown() {
	for _, ws := range m.tabWorkspaces() {
		ws.Stop() // the run and any connect still dialing, in every tab
	}
	m.catsClose()
	m.chatSave() // before close: quitting must not discard the conversation
	m.chat.close()
	for _, ws := range m.tabWorkspaces() {
		ws.Close() // releases each tab's pinned connection
	}
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
		m.compl = nil
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
	case tabMsg:
		// a workspace's event, tagged with the tab that started it: drawn
		// now if that tab is on screen, kept for its activation if not
		return m.routeTab(msg)

	// the workspace's events: a Job's outcome, already landed, or a
	// script's mid-run output from the sink
	case *workspace.Connected:
		return m.connected(msg)
	case *workspace.SchemaLoaded:
		return m.schemaLoaded(msg)
	case *workspace.RowCounts:
		return m.rowCountsLanded(msg)
	case *workspace.SessionReleased:
		return m.sessionReleased(msg)
	case *workspace.RunDone:
		return m.runDone(msg)
	case *workspace.ExplainDone:
		return m.explainDone(msg)
	case *workspace.ScriptShow:
		// already in the result set of the connection the script started
		// on; drawn only while that is the one on screen
		if msg.Conn == m.ws.Active() {
			m.showResult(msg.Result)
		}
		return nil
	case *workspace.ScriptPrint:
		m.logTo(msg.Conn, logInfo, msg.Text)
		return nil
	case complLoadedMsg:
		return m.complLoaded(msg)
	case tickMsg:
		return m.tick(msg)
	case clipDoneMsg:
		return m.clipDone(msg)
	case driverLogMsg:
		m.log(logMuted, string(msg))
		return nil
	case erdMsg:
		return m.erdDone(msg)
	case chatEventMsg:
		return m.chatEvent(msg)
	case chatModelSetMsg:
		return m.chatModelSet(msg)
	case chatSchemaMsg:
		return m.chatSchema(msg)
	case chatSignInCodeMsg:
		return m.chatSignInCode(msg)
	case chatSignInDoneMsg:
		return m.chatSignInDone(msg)
	case catsMsg:
		return m.catsHandle(msg)
	case connTestMsg:
		return m.connTestDone(msg)
	case scriptEditedMsg:
		return m.scriptEdited(msg)
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
		md := m.modal
		cmd, closed := md.key(m, k)
		// only the modal that closed is dropped: one that hands over to
		// another (the scripts browser's rename prompt reopening the
		// browser) has already put its successor in m.modal
		if closed && m.modal == md {
			m.modal = nil
		}
		return cmd
	}

	// the completion popup sits on the editor: its keys come before the
	// app's chords (Tab picks rather than moving focus), the rest go on
	if m.complLive() != nil {
		if cmd, used := m.complKey(k); used {
			return cmd
		}
	}

	switch s {
	case "ctrl+space":
		if m.focus == focusEditor {
			return m.openCompletion(true)
		}
	case "ctrl+r":
		return m.runQuery()
	case "ctrl+shift+r", "alt+r":
		// Ctrl+Shift+R arrives as itself only where the terminal speaks the
		// kitty keyboard protocol; elsewhere it is plain Ctrl+R, so Alt+R is
		// the door that works everywhere Option/Alt sends Meta.
		return m.runAll()
	case "ctrl+x":
		return m.explainQuery(false)
	case "alt+x", "ctrl+shift+x":
		// as with run-all: Ctrl+Shift+X only where the terminal speaks the
		// kitty protocol, Alt+X wherever Option/Alt sends Meta
		return m.explainQuery(true)
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
		m.openScripts("")
		return nil
	case "ctrl+p":
		m.openHistory()
		return nil
	case "ctrl+t":
		return m.listTables()
	case "ctrl+l":
		m.sideHidden = false // the list asked for must be on screen
		m.focus = focusConns
		return nil
	case "ctrl+b":
		m.toggleSidebar()
		return nil
	case "f1":
		m.openHelp()
		return nil
	case "ctrl+a":
		return m.toggleChatFocus()
	case "ctrl+g":
		return m.openCatsAgents()
	case "alt+t":
		return m.newTab()
	case "alt+w":
		return m.closeTab(m.curTab, false)
	case "alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6", "alt+7", "alt+8", "alt+9":
		if i := int(s[len(s)-1] - '1'); i < len(m.tabs) {
			return m.activate(i)
		}
		return nil
	case "alt+n":
		return m.newConsole()
	case "alt+c":
		return m.nextConsole()
	case "tab":
		m.cycleFocus(1)
		return nil
	case "shift+tab":
		m.cycleFocus(-1)
		return nil
	}

	// ? is a typed character in the editor and the assistant's input, so
	// it opens the keys only where nothing takes text (F1 works anywhere)
	if s == "?" && m.focus != focusEditor && m.focus != focusChat {
		m.openHelp()
		return nil
	}

	switch m.focus {
	case focusEditor:
		if cmd, used := m.symbolKey(k); used { // F12, Shift+F12, F2, Esc (symbol.go)
			return cmd
		}
		ver := m.editor.version
		m.editor.HandleKey(k)
		m.drag.follow = true
		return m.complAfterKey(k, ver)
	case focusGrid:
		if k.String() == "z" {
			m.resZoom = !m.resZoom
			return nil
		}
		if m.resTab == tabPlan && m.planv.plan != nil {
			return m.planKey(k)
		}
		return m.gridKey(k)
	case focusConns:
		switch k.String() {
		case "x":
			// below the list's cursor row, where a confirm menu stays in view
			return m.disconnect(m.conns.view.X+2, m.conns.view.Y+m.conns.cur-m.conns.top+1)
		case "r":
			// the sidebar's connection, not the row under the cursor: as
			// x, it acts on what the tab is on (Workspace.Refresh)
			return m.refreshCatalog()
		}
		if m.connKey(k) { // a/+ add, e edit (connform.go)
			return nil
		}
		return m.listKey(m.conns, k, m.connPicked)
	case focusTables:
		// c ("Show columns"), e (diagram it), d (database), s (schema) and
		// # (row counts) are the tables list's letter keys: the list's own
		// keys are movement and Enter, so they shadow nothing
		switch k.String() {
		case "#":
			return m.toggleRowCounts()
		case "c":
			return m.tableColumns()
		case "e":
			return m.tableDiagram()
		case "d":
			m.openDatabasePicker()
			return nil
		case "s":
			m.openSchemaPicker()
			return nil
		}
		return m.listKey(m.tables, k, m.tablePicked)
	case focusLog:
		return m.logKeyPress(k)
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
		return m.copyRow(copyText)
	case "t":
		return m.transposeGrid()
	case "c", "menu":
		x, y := m.grid.cursorScreen()
		m.openGridMenu(x, y+1) // just below the cell, so it stays visible
		return nil
	case "enter":
		m.openInspect()
		return nil
	case "[":
		return m.stepScriptResult(-1)
	case "]":
		return m.stepScriptResult(1)
	case "{", "}", "P", "r", "S", "s", "x":
		cmd, _ := m.resultTabKey(k)
		return cmd
	case "p":
		if m.planv.plan == nil {
			m.log(logWarn, "no plan yet — Ctrl+X explains the statement under the caret")
			return nil
		}
		m.resTab = tabPlan
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
		m.compl = nil // its word and range are not the buffer's any more
	}
	return nil
}

// interrupt is Ctrl+C: stop what is running — a query, or the assistant's
// answer — and quit only when nothing is. psql muscle memory expects Ctrl+C
// to cancel, and losing the session to it would be a nasty surprise.
func (m *Model) interrupt() tea.Cmd {
	if m.ws.Busy() {
		return m.cancelRun()
	}
	if m.cancelConnect() {
		return nil // a connect in flight is "something running": stop it, don't quit
	}
	if m.chat.busy() {
		m.chatStop()
		return nil
	}
	// a query left running in another tab is something running too: Ctrl+C
	// is the gentle key, so it says where rather than quitting over it
	// (Ctrl+Q still quits, stopping every tab's run)
	for i, t := range m.tabs {
		if i != m.curTab && t.ws != nil && t.ws.Busy() {
			m.logf(logWarn, "%s is still running a query — ⌥%d goes there; ^Q quits anyway", t.title, i+1)
			return nil
		}
	}
	return m.quitCmd()
}

// quitCmd cancels anything in flight and ends the program.
func (m *Model) quitCmd() tea.Cmd {
	for _, ws := range m.tabWorkspaces() {
		ws.Stop()
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

// log appends a line to the log of the connection on screen (logs.go).
func (m *Model) log(kind logKind, text string) {
	m.syncLog()
	m.logp.add(kind, text)
}

// logf is log with formatting.
func (m *Model) logf(kind logKind, format string, args ...any) {
	m.log(kind, fmt.Sprintf(format, args...))
}
