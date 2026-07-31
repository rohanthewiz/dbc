package ui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Muted green theme, ported from cdx (GoNotes palette): warm dark gray-green
// surfaces with a single green accent. The hex strings are the one source of
// truth — the tcell colors below drive widget styles, and the tag constants
// drive tview's dynamic-color markup in log and status text.
const (
	hexBg     = "#1f2420" // deepest surface — editor, results table
	hexPanel  = "#242a25" // sidebar, log pane
	hexPanel2 = "#2b322c" // status bar, modal fields, table header band
	hexSel    = "#3a4a3f" // selected row
	hexLine   = "#38403a" // borders and rules at rest
	hexFg     = "#d6ddd6"
	hexMuted  = "#9db0a2"
	hexAccent = "#4db380"
	// Not in cdx; dbc needs a busy/stopped tone. It is markup-only, so it has
	// no tcell.Color counterpart below.
	hexWarn = "#d9a45b"
	hexErr  = "#e57373"
)

var (
	colBg     = tcell.GetColor(hexBg)
	colPanel  = tcell.GetColor(hexPanel)
	colPanel2 = tcell.GetColor(hexPanel2)
	colSel    = tcell.GetColor(hexSel)
	colLine   = tcell.GetColor(hexLine)
	colFg     = tcell.GetColor(hexFg)
	colMuted  = tcell.GetColor(hexMuted)
	colAccent = tcell.GetColor(hexAccent)
	colErr    = tcell.GetColor(hexErr)
)

// Color tags for tview's dynamic-color markup. tagOff closes a run.
const (
	tagAccent = "[" + hexAccent + "]" // connection names, key hints
	tagOk     = "[" + hexAccent + "]" // success — the accent doubles as "good"
	tagMuted  = "[" + hexMuted + "]"
	tagWarn   = "[" + hexWarn + "]"
	tagErr    = "[" + hexErr + "]"
	tagOff    = "[-]"
)

// applyTheme installs the palette as tview's defaults. Primitives read
// tview.Styles when they are constructed, so this must run before build()
// creates any of them.
func applyTheme() {
	tview.Styles = tview.Theme{
		PrimitiveBackgroundColor:    colBg,
		ContrastBackgroundColor:     colSel,
		MoreContrastBackgroundColor: colPanel2,
		BorderColor:                 colLine,
		TitleColor:                  colMuted,
		GraphicsColor:               colLine,
		PrimaryTextColor:            colFg,
		SecondaryTextColor:          colAccent,
		TertiaryTextColor:           colMuted,
		InverseTextColor:            colBg,
		ContrastSecondaryTextColor:  colAccent,
	}
}

// themeModal styles a floating panel. It owns the screen for as long as it is
// up, so its border wears the accent rather than the resting chrome.
func themeModal(b *tview.Box) {
	b.SetBackgroundColor(colPanel).
		SetBorderColor(colAccent).
		SetTitleColor(colAccent)
}

// themeForm styles a modal form's labels, fields, and buttons.
func themeForm(f *tview.Form) {
	themeModal(f.Box)
	f.SetLabelColor(colMuted).
		SetFieldBackgroundColor(colPanel2).
		SetFieldTextColor(colFg).
		SetButtonBackgroundColor(colPanel2).
		SetButtonTextColor(colFg)
	activated := tcell.StyleDefault.Background(colAccent).Foreground(colBg).Bold(true)
	f.SetButtonActivatedStyle(activated)

	// A focused dropdown otherwise inverts to near-white, which is the one
	// bright block in an otherwise muted screen. Give it the accent instead,
	// so a focused field reads like a focused pane.
	for i := range f.GetFormItemCount() {
		dd, ok := f.GetFormItem(i).(*tview.DropDown)
		if !ok {
			continue
		}
		dd.SetFocusedStyle(activated)
		dd.SetPrefixStyle(activated)
		dd.SetListStyles(
			tcell.StyleDefault.Background(colPanel2).Foreground(colFg),
			tcell.StyleDefault.Background(colSel).Foreground(colFg).Bold(true))
	}
}

// pane themes a bordered pane and makes focus visible: the border and title
// light up in the accent while the pane has focus, and fall back to the muted
// chrome when it loses it. With no reverse-video or bright chrome anywhere
// else, that is the only cue the user needs for where the keys go.
func pane(b *tview.Box, bg tcell.Color) {
	blur := func() { b.SetBorderColor(colLine).SetTitleColor(colMuted) }
	b.SetBackgroundColor(bg)
	b.SetFocusFunc(func() { b.SetBorderColor(colAccent).SetTitleColor(colAccent) })
	b.SetBlurFunc(blur)
	blur()
}
