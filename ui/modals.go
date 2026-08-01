package ui

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/serr"
)

// center wraps a primitive so it floats centered at the given size.
func center(p tview.Primitive, w, h int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(p, h, 0, true).
			AddItem(nil, 0, 1, false), w, 0, true).
		AddItem(nil, 0, 1, false)
}

func (a *App) showExportModal() {
	if a.lastRes == nil {
		a.logf(tagWarn + "no result to export — run a query first")
		return
	}
	var form *tview.Form
	doExport := func() {
		_, fstr := form.GetFormItem(0).(*tview.DropDown).GetCurrentOption()
		path := strings.TrimSpace(form.GetFormItem(1).(*tview.InputField).GetText())
		a.pages.RemovePage("export")

		f, err := export.ParseFormat(fstr)
		if err != nil {
			a.logf(tagErr+"%s", tview.Escape(serr.StringFromErr(err)))
			return
		}
		dest := "clipboard"
		if path == "" {
			err = export.ToClipboard(a.lastRes, f)
		} else {
			err = export.ToFile(a.lastRes, f, path)
			dest = path
		}
		if err != nil {
			a.logf(tagErr+"export failed: %s", tview.Escape(serr.StringFromErr(err)))
			return
		}
		a.logf(tagOk+"exported %d rows as %s to %s", len(a.lastRes.Rows), fstr, dest)
	}
	form = tview.NewForm().
		AddDropDown("Format", export.Names(), 0, nil).
		AddInputField("File (empty = clipboard)", "", 40, nil, nil).
		AddButton("Export", doExport).
		AddButton("Cancel", func() { a.pages.RemovePage("export") })
	form.SetBorder(true)
	form.SetTitle(" Export result (Esc to close) ")
	themeForm(form)

	a.pages.AddPage("export", center(form, 64, 11), true, true)
	a.app.SetFocus(form)
}

// showHistoryModal lists the statements run before, newest first, with a live
// filter over them. Enter drops the chosen one into the editor at the cursor —
// replacing the selection if there is one — rather than running it, so a
// recalled query can be edited before it goes anywhere near the database.
func (a *App) showHistoryModal() {
	entries := a.hist.recent()
	if len(entries) == 0 {
		a.logf(tagWarn + "no query history yet — it fills up as you run queries")
		return
	}

	list := tview.NewList().ShowSecondaryText(true)
	list.SetMainTextColor(colFg).SetSecondaryTextColor(colMuted).
		SetSelectedStyle(tcell.StyleDefault.
			Background(colSel).Foreground(colFg).Bold(true))
	list.SetHighlightFullLine(true)

	// shown tracks what the list currently holds, so Enter picks the entry the
	// user is looking at rather than the one at that index unfiltered
	var shown []histEntry
	fill := func(q string) {
		shown = matchHistory(entries, q)
		list.Clear()
		for _, e := range shown {
			list.AddItem(preview(e.SQL),
				"  "+e.At.Local().Format("Jan 2 15:04")+" · "+e.Conn, 0, nil)
		}
	}
	fill("")

	filter := tview.NewInputField().SetLabel(" filter ").SetChangedFunc(fill)
	filter.SetLabelColor(colMuted).
		SetFieldBackgroundColor(colPanel2).
		SetFieldTextColor(colFg)

	// the filter keeps the focus the whole time — the arrows drive the list
	// from here, so recall is one uninterrupted piece of typing
	body := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(filter, 1, 0, true).
		AddItem(list, 0, 1, false)
	body.SetBorder(true)
	body.SetTitle(" History (↑↓ pick · Enter inserts · Esc closes) ")
	themeModal(body.Box)

	// the capture belongs on the field, not on the flex around it: tview hands
	// a key straight to the focused primitive, so a capture anywhere else in
	// the modal would never see one
	filter.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyDown, tcell.KeyCtrlN:
			list.SetCurrentItem(list.GetCurrentItem() + 1)
			return nil
		case tcell.KeyUp, tcell.KeyCtrlP:
			list.SetCurrentItem(list.GetCurrentItem() - 1)
			return nil
		case tcell.KeyEnter:
			i := list.GetCurrentItem()
			if i < 0 || i >= len(shown) {
				return nil
			}
			a.pages.RemovePage("history")
			a.insertInEditor(shown[i].SQL)
			return nil
		}
		return ev
	})

	a.pages.AddPage("history", center(body, 76, 18), true, true)
	a.app.SetFocus(filter)
}

// insertInEditor drops text in at the cursor, replacing the selection when
// there is one, and leaves the focus in the editor ready to run it.
func (a *App) insertInEditor(text string) {
	_, start, end := a.editor.GetSelection()
	a.editor.Replace(start, end, text)
	a.app.SetFocus(a.editor)
	a.logf(tagOk+"recalled — "+tagMuted+"%s", tview.Escape(preview(text)))
}

func (a *App) showScriptsModal() {
	files, err := filepath.Glob(filepath.Join(a.cfg.ScriptsDir, "*.go"))
	if err != nil || len(files) == 0 {
		a.logf(tagWarn+"no scripts found in %s — add .go files with func Run(s *sdb.S) error", a.cfg.ScriptsDir)
		return
	}
	sort.Strings(files)

	list := tview.NewList().ShowSecondaryText(false)
	list.SetMainTextColor(colFg).
		SetSelectedStyle(tcell.StyleDefault.
			Background(colSel).Foreground(colFg).Bold(true))
	for _, f := range files {
		f := f
		list.AddItem(filepath.Base(f), "", 0, func() {
			a.pages.RemovePage("scripts")
			a.runScript(f)
		})
	}
	list.SetBorder(true)
	list.SetTitle(" Scripts (Enter runs, Esc closes) ")
	themeModal(list.Box)

	h := len(files) + 4
	if h > 20 {
		h = 20
	}
	a.pages.AddPage("scripts", center(list, 56, h), true, true)
	a.app.SetFocus(list)
}
