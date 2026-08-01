package ui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/rohanthewiz/dbc/theme"
)

// The palette as tcell colors, for widget styles. theme.Warn has no
// counterpart here — it is only ever markup.
var (
	colBg     = tcell.GetColor(theme.Bg)
	colPanel  = tcell.GetColor(theme.Panel)
	colPanel2 = tcell.GetColor(theme.Panel2)
	colSel    = tcell.GetColor(theme.Sel)
	colLine   = tcell.GetColor(theme.Line)
	colFg     = tcell.GetColor(theme.Fg)
	colMuted  = tcell.GetColor(theme.Muted)
	colAccent = tcell.GetColor(theme.Accent)
	colErr    = tcell.GetColor(theme.Err)
)

// The palette as tview color tags, for the log and status text. tagOff closes
// a run.
const (
	tagAccent = "[" + theme.Accent + "]" // connection names, key hints
	tagOk     = "[" + theme.Accent + "]" // success — the accent doubles as "good"
	tagMuted  = "[" + theme.Muted + "]"
	tagWarn   = "[" + theme.Warn + "]"
	tagErr    = "[" + theme.Err + "]"
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
