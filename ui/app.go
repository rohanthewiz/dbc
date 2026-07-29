// Package ui is the tview terminal interface: connections sidebar, SQL
// editor, results table, log pane, and status bar.
package ui

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/serr"
)

const keyHints = "[gray]^R[-] run  [gray]^E[-] export  [gray]^O[-] scripts  [gray]^L[-] conns  [gray]Tab[-] focus  [gray]^Q[-] quit"

type App struct {
	cfg *config.Config
	mgr *db.Manager

	app      *tview.Application
	pages    *tview.Pages
	connList *tview.List
	editor   *tview.TextArea
	table    *tview.Table
	logView  *tview.TextView
	status   *tview.TextView

	active  string // active connection name
	lastRes *model.Result
	busy    atomic.Bool // a query or script is running
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

	right := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.editor, 0, 3, true).
		AddItem(a.table, 0, 7, false).
		AddItem(a.logView, 7, 0, false)

	main := tview.NewFlex().
		AddItem(a.connList, 28, 0, false).
		AddItem(right, 0, 1, true)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(main, 0, 1, true).
		AddItem(a.status, 1, 0, false)

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
		case tcell.KeyCtrlQ:
			a.app.Stop()
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

func (a *App) runQuery() {
	if !a.busy.CompareAndSwap(false, true) {
		a.logf("[yellow]busy — a query or script is already running")
		return
	}
	stmt := strings.TrimSpace(a.editor.GetText())
	if stmt == "" {
		a.busy.Store(false)
		a.logf("[yellow]nothing to run — type a query first")
		return
	}
	if a.active == "" {
		a.busy.Store(false)
		a.logf("[yellow]no active connection — Ctrl+L then Enter to pick one")
		return
	}
	conn := a.active
	a.logf("running on [aqua]%s[-]…", conn)
	go func() {
		defer a.busy.Store(false)
		res, err := a.mgr.Run(conn, stmt)
		a.app.QueueUpdateDraw(func() {
			if err != nil {
				a.logf("[red]%s", tview.Escape(serr.StringFromErr(err)))
				return
			}
			a.lastRes = res
			a.renderResult(res)
			a.setStatusFromResult(res)
		})
	}()
}

func (a *App) runScript(path string) {
	if !a.busy.CompareAndSwap(false, true) {
		a.logf("[yellow]busy — a query or script is already running")
		return
	}
	a.logf("running script [aqua]%s[-]", path)
	s := sdb.New(a.mgr,
		func(r *model.Result) {
			a.app.QueueUpdateDraw(func() {
				a.lastRes = r
				a.renderResult(r)
				a.setStatusFromResult(r)
			})
		},
		func(msg string) {
			a.app.QueueUpdateDraw(func() {
				a.logf("%s", tview.Escape(msg))
			})
		})
	go func() {
		defer a.busy.Store(false)
		err := script.Run(path, s)
		a.app.QueueUpdateDraw(func() {
			if err != nil {
				a.logf("[red]script error: %s", tview.Escape(serr.StringFromErr(err)))
				return
			}
			a.logf("[green]script completed")
		})
	}()
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
