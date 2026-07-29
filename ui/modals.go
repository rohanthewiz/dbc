package ui

import (
	"path/filepath"
	"sort"
	"strings"

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
		a.logf("[yellow]no result to export — run a query first")
		return
	}
	var form *tview.Form
	doExport := func() {
		_, fstr := form.GetFormItem(0).(*tview.DropDown).GetCurrentOption()
		path := strings.TrimSpace(form.GetFormItem(1).(*tview.InputField).GetText())
		a.pages.RemovePage("export")

		f, err := export.ParseFormat(fstr)
		if err != nil {
			a.logf("[red]%s", tview.Escape(serr.StringFromErr(err)))
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
			a.logf("[red]export failed: %s", tview.Escape(serr.StringFromErr(err)))
			return
		}
		a.logf("[green]exported %d rows as %s to %s", len(a.lastRes.Rows), fstr, dest)
	}
	form = tview.NewForm().
		AddDropDown("Format", export.Names(), 0, nil).
		AddInputField("File (empty = clipboard)", "", 40, nil, nil).
		AddButton("Export", doExport).
		AddButton("Cancel", func() { a.pages.RemovePage("export") })
	form.SetBorder(true)
	form.SetTitle(" Export result (Esc to close) ")

	a.pages.AddPage("export", center(form, 64, 11), true, true)
	a.app.SetFocus(form)
}

func (a *App) showScriptsModal() {
	files, err := filepath.Glob(filepath.Join(a.cfg.ScriptsDir, "*.go"))
	if err != nil || len(files) == 0 {
		a.logf("[yellow]no scripts found in %s — add .go files with func Run(s *sdb.S) error", a.cfg.ScriptsDir)
		return
	}
	sort.Strings(files)

	list := tview.NewList().ShowSecondaryText(false)
	for _, f := range files {
		f := f
		list.AddItem(filepath.Base(f), "", 0, func() {
			a.pages.RemovePage("scripts")
			a.runScript(f)
		})
	}
	list.SetBorder(true)
	list.SetTitle(" Scripts (Enter runs, Esc closes) ")

	h := len(files) + 4
	if h > 20 {
		h = 20
	}
	a.pages.AddPage("scripts", center(list, 56, h), true, true)
	a.app.SetFocus(list)
}
