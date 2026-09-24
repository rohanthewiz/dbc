// Package ui is the tview terminal interface: connections sidebar, SQL
// editor, results table, log pane, and status bar.
package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/serr"
)

// keyHints is kept tight: with the Stop button taking the right edge, a long
// hint line is the first thing an 80-column terminal truncates. Tab-to-cycle-
// focus gave up its slot to ^T — Tab is the guessable one of the two, and the
// lit border already says where the keys are going.
//
// A var rather than a const, rebuilt by setPalette: the accent it is written
// in can now arrive at runtime from the host's theme.
var keyHints = buildKeyHints()

func buildKeyHints() string {
	return tagAccent + "^R" + tagOff + " run " +
		tagAccent + "^K" + tagOff + " stop " +
		tagAccent + "^E" + tagOff + " export " +
		tagAccent + "^O" + tagOff + " scripts " +
		tagAccent + "^L" + tagOff + " conns " +
		tagAccent + "^T" + tagOff + " tables " +
		tagAccent + "^Q" + tagOff + " quit"
}

// stopBtnWidth is the width the Stop button takes in the status row while
// something is running. It is resized to zero the rest of the time.
const stopBtnWidth = 10

// logMaxLines bounds the log pane, so a chatty script in a long session
// cannot grow it without limit. Old lines fall off the top.
const logMaxLines = 2000

type App struct {
	cfg *config.Config
	mgr *db.Manager

	app       *tview.Application
	pages     *tview.Pages
	connList  *tview.List
	editor    *tview.TextArea
	table     *tview.Table
	logView   *tview.TextView
	status    *tview.TextView
	statusRow *tview.Flex
	stopBtn   *tview.Button

	// The layout containers, kept only so a theme change can repaint them:
	// they are what shows through between the panes.
	root     *tview.Flex
	mainRow  *tview.Flex
	rightCol *tview.Flex

	active  string // active connection name
	lastRes *model.Result

	hist       *history // statements run, for Ctrl+P recall
	histWarned bool     // a history write already failed and was reported

	// What the last run failed with, so "ask an agent about this" can carry
	// the error along with the query (ui/catsagents.go). Cleared by a run
	// that succeeds, because a stale error attached to a working query is
	// worse than none.
	lastRunErr string

	busy atomic.Bool // a query or script is running

	runMu  sync.Mutex // guards the run state below
	cancel context.CancelFunc
	runTag string        // what is running, for status and log text
	runAt  time.Time     // when the run started
	tick   chan struct{} // closed to stop the elapsed-time ticker

	// The editor's queries run on a session pinned to the active connection,
	// so BEGIN/COMMIT, SET, and temp tables carry across runs. Runs are
	// serialized by the busy slot, so sessMu is uncontended; it exists to make
	// the shutdown path safe.
	sessMu   sync.Mutex
	sess     *db.Session
	sessName string // connection the session is pinned to

	// connect state: lets Ctrl+K / Ctrl+C abandon a connect that is still
	// dialing, and lets a newer pick supersede an older one. Read and written
	// only on the UI goroutine (setActive runs from the connection list's
	// callback, the rest from key handlers and QueueUpdateDraw), so it needs
	// no lock; the dialing goroutine captures what it needs up front.
	connGen    int                // bumped per connect; an older connect's outcome is dropped
	connCancel context.CancelFunc // cancels the connect in flight; nil when none is
	connName   string             // what it is connecting to, for the log

	// Everything about the cats pane this may be running in. The zero value
	// is "no host", which is what any terminal that is not cats produces —
	// see ui/cats_glue.go.
	cats catsState

	// ttyWrite emits an escape sequence to the controlling terminal. Run
	// installs the real one; it stays nil under test, so the suite never
	// writes to the developer's terminal. See ui/hostident.go.
	ttyWrite  func(string) error
	identSent bool
	identKey  string // the (connection, run tag) the title was last built from

	// The screen dbc draws on, captured from tview's before-draw hook so the
	// clipboard fallback can reach the terminal directly. Nil until the first
	// draw, which is why clipWrite checks. See ui/clip.go.
	scr tcell.Screen
}

// Run builds and runs the TUI. It blocks until the user quits.
func Run(cfg *config.Config, mgr *db.Manager) error {
	a := &App{cfg: cfg, mgr: mgr, app: tview.NewApplication()}
	a.hist = loadHistory(historyFile())
	// Before build: tview primitives copy the theme when they are
	// constructed, so adopting the host's palette here is what makes the
	// first frame already correct instead of a repaint.
	catsThemeAtStartup()
	a.build()

	if _, ok := cfg.ConnByName(cfg.DefaultConnection); ok {
		a.active = cfg.DefaultConnection
	} else if len(cfg.Connections) > 0 {
		a.active = cfg.Connections[0].Name
	}
	a.refreshConnList()
	a.ttyWrite = ttyWriteReal // only the real app talks to /dev/tty
	a.catsInit()              // detect the cats host and claim the pane, if there is one

	if cfg.Demo {
		// name every demo that survived seeding, and mark the active one — the
		// point of shipping two is that the user knows the other is there
		names := make([]string, 0, len(cfg.Connections))
		for _, c := range cfg.Connections {
			n := tagAccent + c.Name + tagOff
			if c.Name == a.active {
				n += " (active)"
			}
			names = append(names, n)
		}
		a.logf("no config found — using the built-in demo connections: %s (see dbc.example.toml)",
			strings.Join(names, ", "))
		a.log("press " + tagWarn + "Ctrl+R" + tagOff + " to run the query")
	} else if cfg.Path != "" {
		a.logf("loaded config from %s", cfg.Path)
	}
	a.logKeys() // the status bar has room for seven of these, the log for all
	for _, w := range cfg.Warnings {
		a.logf(tagWarn+"%s", tview.Escape(w))
	}

	// the buffer from the last session beats the demo sample: a returning
	// user gets their scratchpad back
	if saved := loadBuffer(bufferFile()); strings.TrimSpace(saved) != "" {
		a.editor.SetText(saved, true)
	} else if cfg.Demo {
		a.editor.SetText("SELECT id, name, breed, age, adopted FROM cats ORDER BY age", true)
	}
	a.setStatusText(fmt.Sprintf(tagAccent+"%s"+tagOff+" │ ready", a.active))

	a.app.EnableMouse(true)
	err := a.app.Run()
	a.catsClose()   // hand the pane back before anything else can block
	a.dropSession() // release the pinned connection before the pools close
	if werr := saveBuffer(bufferFile(), a.editor.GetText()); werr != nil {
		fmt.Fprintf(os.Stderr, "could not save the editor buffer: %v\n", werr)
	}
	return err
}

func (a *App) build() {
	applyTheme()

	// a session-only history until Run loads the persisted one, so nothing
	// here has to guard against a nil one
	if a.hist == nil {
		a.hist = &history{}
	}

	a.connList = tview.NewList().ShowSecondaryText(true)
	a.connList.SetBorder(true).SetTitle(" Connections ")

	a.editor = tview.NewTextArea()
	a.editor.SetPlaceholder("Type SQL here, then Ctrl+R to run…")
	a.editor.SetBorder(true).SetTitle(" Query ")

	// cell selection rather than whole-row: it is what makes "copy this value"
	// possible, and it gives the arrow keys somewhere to go on a result too
	// wide for the pane
	a.table = tview.NewTable().SetFixed(1, 0).SetSelectable(true, true)
	a.table.SetBorder(true).SetTitle(" Results (y/Y copy) ")

	a.logView = tview.NewTextView().SetDynamicColors(true).SetScrollable(true).
		SetMaxLines(logMaxLines)
	a.logView.SetBorder(true).SetTitle(" Log ")

	a.status = tview.NewTextView().SetDynamicColors(true)

	// The Stop button lives at the right edge of the status bar. It is only
	// given width while a run is in flight, so it can neither be seen nor
	// clicked when there is nothing to stop.
	a.stopBtn = tview.NewButton("■ Stop").SetSelectedFunc(func() {
		a.cancelRun()
		a.app.SetFocus(a.editor)
	})

	a.statusRow = tview.NewFlex().
		AddItem(a.status, 0, 1, false).
		AddItem(a.stopBtn, 0, 0, false)

	a.restyle() // every color the widgets above wear
	a.setStatusText("ready")

	// The layout containers are kept because they show through wherever a
	// child does not cover them — a one-column gap beside a pane is still a
	// cell that has to be painted from the palette, and they take their
	// background from tview.Styles at construction like any other primitive.
	a.rightCol = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.editor, 0, 3, true).
		AddItem(a.table, 0, 7, false).
		AddItem(a.logView, 7, 0, false)

	a.mainRow = tview.NewFlex().
		AddItem(a.connList, 28, 0, false).
		AddItem(a.rightCol, 0, 1, true)

	a.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.mainRow, 0, 1, true).
		AddItem(a.statusRow, 1, 0, false)

	a.pages = tview.NewPages().AddPage("main", a.root, true, true)
	a.restyleLayout()

	// Catch tview's screen on the way past. The clipboard fallback needs a
	// screen handle to emit OSC 52 (ui/clip.go), and tview exposes one
	// nowhere else: creating the screen here instead and handing it over
	// would mean calling EnableMouse on a screen whose Init error SetScreen
	// silently swallows — a nil-pointer panic in place of the clean "cannot
	// open terminal" message Run returns on its own. Taking the screen from
	// the draw also keeps it CURRENT, since tview may replace it.
	a.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		a.scr = screen
		return false // draw normally
	})
	a.app.SetRoot(a.pages, true)

	a.app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		front, _ := a.pages.GetFrontPage()
		// First refusal, and on both paths: a ⌘ chord is a global vocabulary
		// that must be answered — or swallowed — whether the keyboard
		// belongs to the main page or to a modal. See ui/metakeys.go.
		if a.metaAccelFire(ev, front == "main") {
			return nil
		}
		if front != "main" {
			switch ev.Key() {
			case tcell.KeyEsc:
				a.pages.RemovePage(front)
				return nil
			case tcell.KeyCtrlC:
				// keep tview's default Ctrl+C from stopping the app without
				// canceling what is in flight
				a.interrupt()
				return nil
			case tcell.KeyCtrlQ:
				a.quit()
				return nil
			}
			return ev
		}
		switch ev.Key() {
		case tcell.KeyCtrlR:
			a.runQuery()
			return nil
		case tcell.KeyCtrlK:
			a.cancelRun()
			return nil
		case tcell.KeyCtrlE:
			a.showExportModal()
			return nil
		case tcell.KeyCtrlO:
			a.showScriptsModal()
			return nil
		case tcell.KeyCtrlT:
			a.listTables()
			return nil
		case tcell.KeyCtrlP:
			a.showHistoryModal()
			return nil
		case tcell.KeyCtrlG:
			a.showAgentModal()
			return nil
		case tcell.KeyCtrlL:
			a.app.SetFocus(a.connList)
			return nil
		case tcell.KeyTab:
			a.cycleFocus(1)
			return nil
		case tcell.KeyBacktab:
			a.cycleFocus(-1)
			return nil
		case tcell.KeyCtrlC:
			a.interrupt()
			return nil
		case tcell.KeyCtrlQ:
			a.quit()
			return nil
		case tcell.KeyRune:
			// only while the table holds the keys — in the editor a y is a y
			if a.table.HasFocus() {
				switch ev.Rune() {
				case 'y':
					a.copySelection(false)
					return nil
				case 'Y':
					a.copySelection(true)
					return nil
				}
			}
		}
		return ev
	})
}

// copySelection puts the results table's selection on the system clipboard:
// the cell under the cursor, or with wholeRow the entire row.
func (a *App) copySelection(wholeRow bool) {
	row, col := a.table.GetSelection()
	text, what, err := selectedText(a.lastRes, row, col, wholeRow)
	if err != nil {
		a.logf(tagWarn+"%s", tview.Escape(serr.StringFromErr(err)))
		return
	}
	dest, err := a.clipWrite(text)
	if err != nil {
		a.logf(tagErr+"copy failed: %s", tview.Escape(serr.StringFromErr(err)))
		return
	}
	a.logf(tagOk+"copied %s to %s — "+tagMuted+"%s", what, dest, tview.Escape(preview(text)))
}

// selectedText renders what a copy key should place on the clipboard. row and
// col are table coordinates, so row 0 is the header and the first data row is
// 1. A whole row is tab-separated, which pastes into a spreadsheet as cells
// and into a terminal as a readable line. what names the copy for the log.
func selectedText(r *model.Result, row, col int, wholeRow bool) (text, what string, err error) {
	if r == nil || len(r.Rows) == 0 {
		return "", "", serr.New("nothing to copy — run a query first")
	}
	ri := row - 1
	if ri < 0 || ri >= len(r.Rows) {
		return "", "", serr.New("no row selected")
	}
	cells := r.Rows[ri]
	if wholeRow {
		return strings.Join(cells, "\t"), fmt.Sprintf("row %d (%d columns)", ri+1, len(cells)), nil
	}
	if col < 0 || col >= len(cells) {
		return "", "", serr.New("no cell selected")
	}
	name := fmt.Sprintf("column %d", col+1)
	if col < len(r.Columns) {
		name = r.Columns[col]
	}
	return cells[col], fmt.Sprintf("%s of row %d", name, ri+1), nil
}

// logKeys writes the full key list to the log once at startup. The status bar
// only has room for the busiest few, and the ones it leaves out — history,
// focus cycling, the copy keys — are the ones nobody would guess.
func (a *App) logKeys() {
	keys := []struct{ key, what string }{
		{"^R", "run"}, {"^K", "stop"}, {"^P", "history"}, {"^T", "tables"},
		{"^E", "export"}, {"^O", "scripts"}, {"^L", "conns"},
		{"Tab", "focus"}, {"y/Y", "copy cell/row"}, {"^Q", "quit"},
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = tagAccent + k.key + tagOff + " " + k.what
	}
	a.logf("keys: %s", strings.Join(parts, tagMuted+" · "+tagOff))
}

func (a *App) cycleFocus(d int) {
	order := []tview.Primitive{a.editor, a.table, a.connList}
	cur := 0
	for i, p := range order {
		if p.HasFocus() {
			cur = i
			break
		}
	}
	a.app.SetFocus(order[(cur+d+len(order))%len(order)])
}

func (a *App) refreshConnList() {
	a.connList.Clear()
	for i, c := range a.cfg.Connections {
		name := c.Name
		label := "  " + name
		if name == a.active {
			label = "● " + name
		}
		a.connList.AddItem(label, "  "+c.Driver, 0, func() { a.setActive(name) })
		if name == a.active {
			a.connList.SetCurrentItem(i)
		}
	}
}

func (a *App) setActive(name string) {
	a.logf("connecting to "+tagAccent+"%s"+tagOff+"…", name)
	// A newer pick cancels an older connect still in flight: otherwise a slow
	// host picked first would land after a fast one picked second and switch
	// the connection back. The generation drops the canceled one's outcome.
	if a.connCancel != nil {
		a.connCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.connGen++
	gen := a.connGen
	a.connCancel, a.connName = cancel, name
	go func() {
		_, err := a.mgr.DBContext(ctx, name)
		cancel()
		a.app.QueueUpdateDraw(func() {
			if gen != a.connGen {
				return // superseded by a later connect
			}
			a.connCancel = nil
			if errors.Is(err, db.ErrCanceled) {
				a.logf(tagWarn+"connect to %s canceled", name)
				return
			}
			if err != nil {
				a.logf(tagErr+"connect failed: %s", tview.Escape(serr.StringFromErr(err)))
				return
			}
			a.active = name
			a.refreshConnList()
			a.logf(tagOk+"connected to %s", name)
			a.setStatusText(fmt.Sprintf(tagAccent+"%s"+tagOff+" │ connected", name))
			a.hostIdentSync() // the title names the connection, which just changed
			go a.releaseSessionUnless(name)
		})
	}()
}

// cancelConnect abandons the connect in flight, if any, and reports whether
// there was one. Without it only connect_timeout bounds a connect, and with
// connect_timeout = "0" an unreachable host would hold it until the OS gave
// up on the TCP connect. Call from the UI goroutine.
func (a *App) cancelConnect() bool {
	if a.connCancel == nil {
		return false
	}
	a.connCancel()
	a.logf(tagWarn+"canceling connect to %s…", a.connName)
	return true
}

// releaseSessionUnless closes the session pinned to any connection other than
// keep, so switching connections ends the old session — rolling back what it
// left open — then and there, rather than holding its transaction and locks
// open until the next run happens to replace it.
//
// Call it on its own goroutine: it takes sessMu, which a run in flight holds
// for as long as its statement takes. It is started only after a.active has
// moved to keep, so a run begun in the meantime is already on keep and its
// session is left alone. A session that only ever ran queries goes quietly.
func (a *App) releaseSessionUnless(keep string) {
	a.sessMu.Lock()
	if a.sess == nil || a.sessName == keep {
		a.sessMu.Unlock()
		return
	}
	left, stateful := a.sessName, a.sess.Stateful()
	a.dropSessionLocked()
	a.sessMu.Unlock()
	if stateful {
		a.app.QueueUpdateDraw(func() {
			a.logf(tagWarn+"left %s: its session was closed — any open transaction was rolled back, SET values and temp tables are gone", left)
		})
	}
}

// beginRun claims the single run slot, shows the Stop button, and returns the
// context the work must run under. Call it from the UI goroutine; whoever
// gets true back is responsible for a matching endRun.
func (a *App) beginRun(tag string) (context.Context, bool) {
	if !a.busy.CompareAndSwap(false, true) {
		a.runMu.Lock()
		running := a.runTag
		a.runMu.Unlock()
		a.logf(tagWarn+"busy — %s is still running (Ctrl+K stops it)", running)
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan struct{})

	a.runMu.Lock()
	a.cancel, a.runTag, a.runAt, a.tick = cancel, tag, time.Now(), tick
	a.runMu.Unlock()

	a.statusRow.ResizeItem(a.stopBtn, stopBtnWidth, 0)
	a.setStatusText(a.runningStatus())
	go a.tickElapsed(tick)
	a.catsAfterTransition() // idle → working, with the tag as the host's status
	return ctx, true
}

// endRun releases the run slot and hides the Stop button, returning how long
// the run took. Call it from the UI goroutine.
func (a *App) endRun() time.Duration {
	a.runMu.Lock()
	if a.cancel != nil {
		a.cancel() // release the context's resources; a no-op if it finished
		a.cancel = nil
	}
	if a.tick != nil {
		close(a.tick)
		a.tick = nil
	}
	elapsed := time.Since(a.runAt)
	a.runMu.Unlock()

	a.statusRow.ResizeItem(a.stopBtn, 0, 0)
	a.busy.Store(false)
	// working → idle, which is the edge cats turns into a "finished"
	// notification. It reports after busy is cleared, so the state it
	// publishes is the one that is now true.
	a.catsAfterTransition()
	return elapsed
}

// cancelRun stops whatever is running — Ctrl+K, or the Stop button.
func (a *App) cancelRun() {
	a.runMu.Lock()
	cancel, tag := a.cancel, a.runTag
	a.runMu.Unlock()

	if !a.busy.Load() || cancel == nil {
		// no run, but a connect may be dialing — Ctrl+K stops that too
		if !a.cancelConnect() {
			a.log(tagWarn + "nothing is running")
		}
		return
	}
	cancel()
	a.logf(tagWarn+"stopping %s…", tag)
	a.setStatusText(fmt.Sprintf(tagAccent+"%s"+tagOff+" │ "+tagWarn+"stopping %s…", a.active, tag))
}

// interrupt is Ctrl+C: stop the run in flight when there is one — psql muscle
// memory — and quit only when idle.
func (a *App) interrupt() {
	if a.busy.Load() {
		a.cancelRun()
		return
	}
	if a.cancelConnect() {
		return // a connect in flight is "something running": stop it, don't quit
	}
	a.quit()
}

// quit cancels anything in flight — so the server is told to abort rather
// than discovering a dropped connection later — and then exits.
func (a *App) quit() {
	a.runMu.Lock()
	cancel := a.cancel
	a.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if a.connCancel != nil {
		a.connCancel() // a connect still dialing is abandoned with the run
	}
	// Stop accepting posts from cats goroutines before the loop stops
	// draining them; catsClose does the rest once Run returns.
	a.catsStopping()
	a.app.Stop()
}

// tickElapsed keeps the running time on the status bar current, so a slow
// query looks alive rather than hung. It exits when the run ends.
func (a *App) tickElapsed(tick <-chan struct{}) {
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-tick:
			return
		case <-t.C:
			a.app.QueueUpdateDraw(func() {
				if !a.busy.Load() {
					return // the run ended while this update was queued
				}
				a.setStatusText(a.runningStatus())
			})
		}
	}
}

func (a *App) runningStatus() string {
	a.runMu.Lock()
	tag, at := a.runTag, a.runAt
	a.runMu.Unlock()
	return fmt.Sprintf(tagAccent+"%s"+tagOff+" │ "+tagWarn+"%s %s"+tagOff,
		a.active, tag, time.Since(at).Round(100*time.Millisecond))
}

// runOnSession executes one statement on a session pinned to conn, so
// session-scoped SQL — BEGIN/COMMIT, SET, temp tables — carries across runs
// the way it does across the statements of a headless buffer. The session is
// opened on first use, swapped when the connection changes (closing the old
// one rolls back whatever it left open), and rebuilt once when the pinned
// connection has gone bad, e.g. cut by a server-side idle timeout — but only
// when the session held no state; see the comment in the loop.
func (a *App) runOnSession(ctx context.Context, conn, stmt string) (*model.Result, error) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	for retried := false; ; retried = true {
		if a.sess == nil || a.sessName != conn {
			a.dropSessionLocked()
			sess, err := a.mgr.Session(ctx, conn)
			if err != nil {
				return nil, err
			}
			a.sess, a.sessName = sess, conn
		}
		res, err := a.sess.Run(ctx, stmt)
		// db.Session.Classify holds the rule; see the Fault constants.
		// In short: retry only what never reached the server on a session
		// that held nothing; fail loudly when a transaction or setting died
		// with the connection — replaying a COMMIT on a fresh session would
		// "succeed" with nothing to commit; otherwise just stop using the
		// dead session.
		switch a.sess.Classify(err) {
		case db.FaultRetry:
			a.dropSessionLocked()
			if !retried {
				continue
			}
		case db.FaultDrop:
			a.dropSessionLocked()
		case db.FaultLost:
			a.dropSessionLocked()
			return nil, db.SessionLost(conn, err)
		}
		return res, err
	}
}

// dropSessionLocked closes the pinned session, if any. The caller holds sessMu.
func (a *App) dropSessionLocked() {
	if a.sess != nil {
		_ = a.sess.Close()
		a.sess, a.sessName = nil, ""
	}
}

func (a *App) dropSession() {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	a.dropSessionLocked()
}

// stmtsToRun picks what Ctrl+R should execute: the statements of the
// selection when the user has made one, otherwise the statement the cursor
// sits in. tag names the run for the status bar and log.
func (a *App) stmtsToRun() (stmts []string, tag string) {
	sel, cursor, _ := a.editor.GetSelection()
	if strings.TrimSpace(sel) != "" {
		parts := sqlsplit.Split(sel)
		if len(parts) == 0 {
			return nil, "" // only comments or whitespace selected
		}
		stmts = make([]string, len(parts))
		for i, p := range parts {
			stmts[i] = p.Text
		}
		if len(stmts) == 1 {
			return stmts, "selection"
		}
		return stmts, fmt.Sprintf("selection (%d statements)", len(stmts))
	}
	all := sqlsplit.Split(a.editor.GetText())
	if len(all) == 0 {
		return nil, ""
	}
	i := sqlsplit.IndexAt(all, cursor)
	if len(all) == 1 {
		return []string{all[i].Text}, "query"
	}
	return []string{all[i].Text}, fmt.Sprintf("statement %d/%d", i+1, len(all))
}

func (a *App) runQuery() {
	stmts, tag := a.stmtsToRun()
	if len(stmts) == 0 {
		a.log(tagWarn + "nothing to run — type a query first")
		return
	}
	// recorded before the run, not after: the query worth recalling is very
	// often the one that just failed. Only what the user typed goes in —
	// Ctrl+T's catalog query is the app's, not theirs.
	for _, stmt := range stmts {
		a.record(stmt)
	}
	a.run(stmts, tag)
}

// record puts a statement in the query history. A write failure is reported
// once per session: a history that cannot be persisted still works for the
// session, and a log line per query would be worse than the problem.
func (a *App) record(stmt string) {
	err := a.hist.add(a.active, stmt, time.Now())
	if err == nil || a.histWarned {
		return
	}
	a.histWarned = true
	a.logf(tagWarn+"query history is not being saved: %s",
		tview.Escape(serr.StringFromErr(err)))
}

// listTables runs the active driver's catalog query — the \dt of whichever
// database this is. It goes through the same path Ctrl+R uses, so it is
// cancelable, lands in the results table, and exports like any other result.
func (a *App) listTables() {
	cc, ok := a.cfg.ConnByName(a.active)
	if !ok {
		a.log(tagWarn + "no active connection — Ctrl+L then Enter to pick one")
		return
	}
	q, err := db.TablesQuery(cc.Driver)
	if err != nil {
		a.logf(tagErr+"%s", tview.Escape(serr.StringFromErr(err)))
		return
	}
	a.run([]string{q}, "list tables")
}

// run executes statements in order on the active connection's pinned session,
// off the UI goroutine, and publishes the last result.
func (a *App) run(stmts []string, tag string) {
	if a.active == "" {
		a.log(tagWarn + "no active connection — Ctrl+L then Enter to pick one")
		return
	}
	ctx, ok := a.beginRun(tag)
	if !ok {
		return
	}
	conn := a.active
	a.logf("running %s on "+tagAccent+"%s"+tagOff+" — "+tagMuted+"%s",
		tag, conn, tview.Escape(preview(strings.Join(stmts, "; "))))
	go func() {
		// the statements run in order on the pinned session, stopping at the
		// first failure — which is tagged with its position, as headless runs
		// tag theirs
		var res *model.Result
		var err error
		for i, stmt := range stmts {
			res, err = a.runOnSession(ctx, conn, stmt)
			if err != nil {
				if len(stmts) > 1 {
					err = serr.Wrap(err, "statement", fmt.Sprintf("%d/%d", i+1, len(stmts)))
				}
				break
			}
		}
		a.app.QueueUpdateDraw(func() {
			elapsed := a.endRun()
			if err != nil {
				a.reportRunErr(conn, tag, err, elapsed)
				return
			}
			a.lastRunErr = "" // this query works; nothing to explain
			a.lastRes = res
			a.renderResult(res)
			a.setStatusFromResult(res)
			if len(stmts) > 1 {
				a.logf(tagOk+"%d statements completed — showing the last result", len(stmts))
			}
		})
	}()
}

func (a *App) runScript(path string) {
	tag := "script " + filepath.Base(path)
	ctx, ok := a.beginRun(tag)
	if !ok {
		return
	}
	a.logf("running "+tagAccent+"%s"+tagOff, tag)
	s := sdb.New(a.mgr,
		func(r *model.Result) {
			a.app.QueueUpdateDraw(func() {
				a.lastRes = r
				a.renderResult(r)
			})
		},
		func(msg string) {
			a.app.QueueUpdateDraw(func() {
				a.logf("%s", tview.Escape(msg))
			})
		}).WithContext(ctx)
	go func() {
		err := script.Run(path, s)
		a.app.QueueUpdateDraw(func() {
			elapsed := a.endRun()
			if err != nil {
				a.reportRunErr(a.active, tag, err, elapsed)
				return
			}
			a.logf(tagOk+"%s completed in %s", tag, elapsed.Round(time.Millisecond))
			if a.lastRes != nil {
				a.setStatusFromResult(a.lastRes)
			}
		})
	}()
}

// reportRunErr writes a failed or canceled run to the log and status bar.
func (a *App) reportRunErr(conn, tag string, err error, elapsed time.Duration) {
	// Remembered for "ask an agent about this". A cancellation is not a
	// failure the user needs explained, so it does not become one.
	if !errors.Is(err, db.ErrCanceled) {
		a.lastRunErr = serr.StringFromErr(err)
	}
	if errors.Is(err, db.ErrCanceled) {
		a.logf(tagWarn+"%s stopped after %s", tag, elapsed.Round(time.Millisecond))
		a.setStatusText(fmt.Sprintf(tagAccent+"%s"+tagOff+" │ "+tagWarn+"stopped"+tagOff+" after %s",
			conn, elapsed.Round(time.Millisecond)))
		// Stopping a statement can cost the connection (the driver may
		// close it to abort the statement). When the session held state,
		// that is worth more than "stopped" alone.
		if errors.Is(err, db.ErrSessionLost) {
			a.log(tagWarn + "the session was lost with it — its open transaction, SET values and temp tables are gone")
		}
		return
	}
	a.logf(tagErr+"%s", tview.Escape(serr.StringFromErr(err)))
	a.setStatusText(fmt.Sprintf(tagAccent+"%s"+tagOff+" │ "+tagErr+"error"+tagOff+" after %s",
		conn, elapsed.Round(time.Millisecond)))
}

// preview renders a one-line, length-capped echo of a statement for the log.
func preview(stmt string) string {
	s := strings.Join(strings.Fields(stmt), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}

func (a *App) renderResult(r *model.Result) {
	t := a.table
	t.Clear()
	for c, col := range r.Columns {
		t.SetCell(0, c, tview.NewTableCell(" "+tview.Escape(col)+" ").
			SetTextColor(colAccent).
			SetBackgroundColor(colPanel2). // a header band, so it holds when scrolled under
			SetAttributes(tcell.AttrBold).
			SetSelectable(false))
	}
	shown := a.rowsShown(r)
	if shown < len(r.Rows) {
		a.logf(tagWarn+"showing the first %d of %d rows"+tagOff+
			" — the rest are fetched, and go into an export (max_display_rows)",
			shown, len(r.Rows))
	}
	for ri, row := range r.Rows[:shown] {
		for ci, val := range row {
			cell := tview.NewTableCell(" " + tview.Escape(val) + " ").
				SetTextColor(colFg).
				SetMaxWidth(48)
			if isNull(r, ri, ci) {
				// a real NULL wears the muted color, so a column of missing
				// values reads as absence at a glance — and so it cannot be
				// mistaken for a column holding the string "NULL"
				cell.SetTextColor(colMuted)
			}
			t.SetCell(ri+1, ci, cell)
		}
	}
	t.ScrollToBeginning()
	if shown > 0 {
		t.Select(1, 0)
	}
}

// rowsShown is how many of a result's rows the table materializes. Fetching is
// capped by max_rows, which someone may raise so an export gets everything;
// rendering is capped separately by max_display_rows, because a cell widget
// per value is what turns a 50k-row result into a sluggish scroll. The rows
// beyond the cap are still in the result, and still in an export.
func (a *App) rowsShown(r *model.Result) int {
	n := len(r.Rows)
	if cap := a.cfg.MaxDisplayRows; cap > 0 && cap < n {
		return cap
	}
	return n
}

// isNull reports whether the display value at (row, col) came from a real SQL
// NULL rather than from a column holding the string "NULL" — both render as
// the same four characters, but Raw kept the typed value.
func isNull(r *model.Result, row, col int) bool {
	return row < len(r.Raw) && col < len(r.Raw[row]) && r.Raw[row][col] == nil
}

func (a *App) setStatusFromResult(r *model.Result) {
	verb := fmt.Sprintf("%d rows", len(r.Rows))
	if r.IsExec {
		verb = fmt.Sprintf("%d affected", r.Affected)
	}
	trunc := ""
	if r.Truncated {
		trunc = fmt.Sprintf(" "+tagWarn+"(truncated at %d)"+tagOff, a.cfg.MaxRows)
	}
	if shown := a.rowsShown(r); shown < len(r.Rows) {
		trunc += fmt.Sprintf(" "+tagWarn+"(showing %d)"+tagOff, shown)
	}
	a.setStatusText(fmt.Sprintf(tagAccent+"%s"+tagOff+" │ %s in %s%s",
		r.Conn, verb, r.Duration.Round(10*time.Microsecond), trunc))
}

func (a *App) setStatusText(left string) {
	a.status.SetText(" " + left + " │ " + keyHints)
}

// log appends a line that is already complete. It exists because the color
// tags became runtime values when the palette did: a message built by
// concatenating them is no longer a constant format string, and handing one
// to logf reads to vet as a formatting mistake waiting to happen. Anything
// with a real format goes to logf.
func (a *App) log(msg string) {
	a.logf("%s", msg)
}

// logf appends a timestamped line to the log pane. Call from the UI
// goroutine (or inside QueueUpdateDraw).
func (a *App) logf(format string, args ...any) {
	fmt.Fprintf(a.logView, tagMuted+"%s"+tagOff+" %s\n",
		time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	a.logView.ScrollToEnd()
}
