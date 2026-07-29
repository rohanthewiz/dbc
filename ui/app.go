// Package ui is the tview terminal interface: connections sidebar, SQL
// editor, results table, log pane, and status bar.
package ui

import (
	"context"
	"errors"
	"fmt"
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
// hint line is the first thing an 80-column terminal truncates.
const keyHints = "[gray]^R[-] run [gray]^K[-] stop [gray]^E[-] export [gray]^O[-] scripts [gray]^L[-] conns [gray]Tab[-] focus [gray]^Q[-] quit"

// stopBtnWidth is the width the Stop button takes in the status row while
// something is running. It is resized to zero the rest of the time.
const stopBtnWidth = 10

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

	active  string // active connection name
	lastRes *model.Result

	busy atomic.Bool // a query or script is running

	runMu  sync.Mutex // guards the run state below
	cancel context.CancelFunc
	runTag string        // what is running, for status and log text
	runAt  time.Time     // when the run started
	tick   chan struct{} // closed to stop the elapsed-time ticker
}

// Run builds and runs the TUI. It blocks until the user quits.
func Run(cfg *config.Config, mgr *db.Manager) error {
	a := &App{cfg: cfg, mgr: mgr, app: tview.NewApplication()}
	a.build()

	if _, ok := cfg.ConnByName(cfg.DefaultConnection); ok {
		a.active = cfg.DefaultConnection
	} else if len(cfg.Connections) > 0 {
		a.active = cfg.Connections[0].Name
	}
	a.refreshConnList()

	if cfg.Demo {
		a.editor.SetText("SELECT id, name, breed, age, adopted FROM cats ORDER BY age", true)
		a.logf("no config found — using the built-in [aqua]demo[-] connection (see dbc.example.toml)")
		a.logf("press [yellow]Ctrl+R[-] to run the query")
	} else if cfg.Path != "" {
		a.logf("loaded config from %s", cfg.Path)
	}
	a.setStatusText(fmt.Sprintf("[aqua]%s[-] │ ready", a.active))

	a.app.EnableMouse(true)
	return a.app.Run()
}

func (a *App) build() {
	a.connList = tview.NewList().ShowSecondaryText(true)
	a.connList.SetBorder(true).SetTitle(" Connections ")

	a.editor = tview.NewTextArea()
	a.editor.SetPlaceholder("Type SQL here, then Ctrl+R to run…")
	a.editor.SetBorder(true).SetTitle(" Query ")

	a.table = tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	a.table.SetBorder(true).SetTitle(" Results ")

	a.logView = tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	a.logView.SetBorder(true).SetTitle(" Log ")

	a.status = tview.NewTextView().SetDynamicColors(true)
	a.setStatusText("ready")

	// The Stop button lives at the right edge of the status bar. It is only
	// given width while a run is in flight, so it can neither be seen nor
	// clicked when there is nothing to stop.
	a.stopBtn = tview.NewButton("■ Stop").SetSelectedFunc(func() {
		a.cancelRun()
		a.app.SetFocus(a.editor)
	})
	a.stopBtn.SetStyle(tcell.StyleDefault.
		Background(tcell.ColorDarkRed).Foreground(tcell.ColorWhite))
	a.stopBtn.SetActivatedStyle(tcell.StyleDefault.
		Background(tcell.ColorRed).Foreground(tcell.ColorWhite))

	a.statusRow = tview.NewFlex().
		AddItem(a.status, 0, 1, false).
		AddItem(a.stopBtn, 0, 0, false)

	right := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.editor, 0, 3, true).
		AddItem(a.table, 0, 7, false).
		AddItem(a.logView, 7, 0, false)

	main := tview.NewFlex().
		AddItem(a.connList, 28, 0, false).
		AddItem(right, 0, 1, true)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(main, 0, 1, true).
		AddItem(a.statusRow, 1, 0, false)

	a.pages = tview.NewPages().AddPage("main", root, true, true)
	a.app.SetRoot(a.pages, true)

	a.app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		front, _ := a.pages.GetFrontPage()
		if front != "main" {
			if ev.Key() == tcell.KeyEsc {
				a.pages.RemovePage(front)
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
		case tcell.KeyCtrlL:
			a.app.SetFocus(a.connList)
			return nil
		case tcell.KeyTab:
			a.cycleFocus(1)
			return nil
		case tcell.KeyBacktab:
			a.cycleFocus(-1)
			return nil
		case tcell.KeyCtrlQ, tcell.KeyCtrlC:
			a.quit()
			return nil
		}
		return ev
	})
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
	a.logf("connecting to [aqua]%s[-]…", name)
	go func() {
		_, err := a.mgr.DB(name)
		a.app.QueueUpdateDraw(func() {
			if err != nil {
				a.logf("[red]connect failed: %s", tview.Escape(serr.StringFromErr(err)))
				return
			}
			a.active = name
			a.refreshConnList()
			a.logf("[green]connected to %s", name)
			a.setStatusText(fmt.Sprintf("[aqua]%s[-] │ connected", name))
		})
	}()
}

// beginRun claims the single run slot, shows the Stop button, and returns the
// context the work must run under. Call it from the UI goroutine; whoever
// gets true back is responsible for a matching endRun.
func (a *App) beginRun(tag string) (context.Context, bool) {
	if !a.busy.CompareAndSwap(false, true) {
		a.runMu.Lock()
		running := a.runTag
		a.runMu.Unlock()
		a.logf("[yellow]busy — %s is still running (Ctrl+K stops it)", running)
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
	return elapsed
}

// cancelRun stops whatever is running — Ctrl+K, or the Stop button.
func (a *App) cancelRun() {
	a.runMu.Lock()
	cancel, tag := a.cancel, a.runTag
	a.runMu.Unlock()

	if !a.busy.Load() || cancel == nil {
		a.logf("[yellow]nothing is running")
		return
	}
	cancel()
	a.logf("[yellow]stopping %s…", tag)
	a.setStatusText(fmt.Sprintf("[aqua]%s[-] │ [yellow]stopping %s…", a.active, tag))
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
	return fmt.Sprintf("[aqua]%s[-] │ [yellow]%s %s[-]",
		a.active, tag, time.Since(at).Round(100*time.Millisecond))
}

// stmtToRun picks what Ctrl+R should execute: the selection when the user has
// made one, otherwise the statement the cursor sits in. idx and total are the
// statement's 1-based position in the buffer, or zero for a selection.
func (a *App) stmtToRun() (stmt string, idx, total int) {
	sel, cursor, _ := a.editor.GetSelection()
	if s := strings.TrimSpace(sel); s != "" {
		return s, 0, 0
	}
	stmts := sqlsplit.Split(a.editor.GetText())
	if len(stmts) == 0 {
		return "", 0, 0
	}
	i := sqlsplit.IndexAt(stmts, cursor)
	return stmts[i].Text, i + 1, len(stmts)
}

func (a *App) runQuery() {
	stmt, idx, total := a.stmtToRun()
	if stmt == "" {
		a.logf("[yellow]nothing to run — type a query first")
		return
	}
	if a.active == "" {
		a.logf("[yellow]no active connection — Ctrl+L then Enter to pick one")
		return
	}
	tag := "query"
	switch {
	case idx == 0:
		tag = "selection"
	case total > 1:
		tag = fmt.Sprintf("statement %d/%d", idx, total)
	}
	ctx, ok := a.beginRun(tag)
	if !ok {
		return
	}
	conn := a.active
	a.logf("running %s on [aqua]%s[-] — [gray]%s", tag, conn, tview.Escape(preview(stmt)))
	go func() {
		res, err := a.mgr.RunContext(ctx, conn, stmt)
		a.app.QueueUpdateDraw(func() {
			elapsed := a.endRun()
			if err != nil {
				a.reportRunErr(conn, tag, err, elapsed)
				return
			}
			a.lastRes = res
			a.renderResult(res)
			a.setStatusFromResult(res)
		})
	}()
}

func (a *App) runScript(path string) {
	tag := "script " + filepath.Base(path)
	ctx, ok := a.beginRun(tag)
	if !ok {
		return
	}
	a.logf("running [aqua]%s[-]", tag)
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
			a.logf("[green]%s completed in %s", tag, elapsed.Round(time.Millisecond))
			if a.lastRes != nil {
				a.setStatusFromResult(a.lastRes)
			}
		})
	}()
}

// reportRunErr writes a failed or canceled run to the log and status bar.
func (a *App) reportRunErr(conn, tag string, err error, elapsed time.Duration) {
	if errors.Is(err, db.ErrCanceled) {
		a.logf("[yellow]%s stopped after %s", tag, elapsed.Round(time.Millisecond))
		a.setStatusText(fmt.Sprintf("[aqua]%s[-] │ [yellow]stopped[-] after %s",
			conn, elapsed.Round(time.Millisecond)))
		return
	}
	a.logf("[red]%s", tview.Escape(serr.StringFromErr(err)))
	a.setStatusText(fmt.Sprintf("[aqua]%s[-] │ [red]error[-] after %s",
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
			SetTextColor(tcell.ColorYellow).
			SetAttributes(tcell.AttrBold).
			SetSelectable(false))
	}
	for ri, row := range r.Rows {
		for ci, val := range row {
			t.SetCell(ri+1, ci, tview.NewTableCell(" "+tview.Escape(val)+" ").SetMaxWidth(48))
		}
	}
	t.ScrollToBeginning()
	if len(r.Rows) > 0 {
		t.Select(1, 0)
	}
}

func (a *App) setStatusFromResult(r *model.Result) {
	verb := fmt.Sprintf("%d rows", len(r.Rows))
	if r.IsExec {
		verb = fmt.Sprintf("%d affected", r.Affected)
	}
	trunc := ""
	if r.Truncated {
		trunc = fmt.Sprintf(" [yellow](truncated at %d)[-]", a.cfg.MaxRows)
	}
	a.setStatusText(fmt.Sprintf("[aqua]%s[-] │ %s in %s%s",
		r.Conn, verb, r.Duration.Round(10*time.Microsecond), trunc))
}

func (a *App) setStatusText(left string) {
	a.status.SetText(" " + left + " │ " + keyHints)
}

// logf appends a timestamped line to the log pane. Call from the UI
// goroutine (or inside QueueUpdateDraw).
func (a *App) logf(format string, args ...any) {
	fmt.Fprintf(a.logView, "[gray]%s[-] %s\n",
		time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	a.logView.ScrollToEnd()
}
