package ui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/rohanthewiz/dbc/theme"
)

// The active palette. It starts as the built-in one and is replaced only by
// setPalette — today from the cats host dbc is running inside
// (ui/catstheme.go), which is why these are vars rather than the constants
// they were: a palette that can arrive at runtime cannot be folded in at
// compile time.
var (
	palette = theme.Default()

	// The palette as tcell colors, for widget styles.
	colBg     = tcell.GetColor(palette.Bg)
	colPanel  = tcell.GetColor(palette.Panel)
	colPanel2 = tcell.GetColor(palette.Panel2)
	colSel    = tcell.GetColor(palette.Sel)
	colLine   = tcell.GetColor(palette.Line)
	colFg     = tcell.GetColor(palette.Fg)
	colMuted  = tcell.GetColor(palette.Muted)
	colAccent = tcell.GetColor(palette.Accent)
	colErr    = tcell.GetColor(palette.Err)

	// The palette as tview color tags, for the log and status text. tagOff
	// closes a run.
	tagAccent = "[" + palette.Accent + "]" // connection names, key hints
	tagOk     = "[" + palette.Accent + "]" // success — the accent doubles as "good"
	tagMuted  = "[" + palette.Muted + "]"
	tagWarn   = "[" + palette.Warn + "]"
	tagErr    = "[" + palette.Err + "]"
	tagOff    = "[-]"
)

// setPalette installs a palette as the colors and tags every draw reads.
//
// It only changes what is DERIVED from the palette. The widgets built from
// those values keep the ones they were constructed with, because tview
// primitives copy tview.Styles at construction — applyTheme and App.restyle
// are the other half, and a caller that wants a visible change calls all
// three. Splitting it that way keeps the startup path (set the palette before
// anything is built) from having to restyle widgets that do not exist yet.
//
// Text already written into the log keeps the tags it was written with: those
// are bytes in a TextView, not a style. A theme change therefore leaves older
// lines in the previous accent, which is a seam worth one log line rather
// than a rewrite of the buffer.
func setPalette(p theme.Palette) {
	palette = p
	colBg = tcell.GetColor(p.Bg)
	colPanel = tcell.GetColor(p.Panel)
	colPanel2 = tcell.GetColor(p.Panel2)
	colSel = tcell.GetColor(p.Sel)
	colLine = tcell.GetColor(p.Line)
	colFg = tcell.GetColor(p.Fg)
	colMuted = tcell.GetColor(p.Muted)
	colAccent = tcell.GetColor(p.Accent)
	colErr = tcell.GetColor(p.Err)

	tagAccent = "[" + p.Accent + "]"
	tagOk = "[" + p.Accent + "]"
	tagMuted = "[" + p.Muted + "]"
	tagWarn = "[" + p.Warn + "]"
	tagErr = "[" + p.Err + "]"

	keyHints = buildKeyHints() // written in the accent, so it follows too
}

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

// restyle paints the active palette onto the long-lived widgets.
//
// It exists because tview primitives COPY tview.Styles when they are
// constructed: applyTheme sets the defaults for widgets not yet built, and
// this sets the colors of the ones already on screen. build calls it once,
// and a theme arriving mid-session calls it again — which is the whole reason
// the styling lives here in one place instead of inline where each widget is
// created.
//
// Modals are deliberately absent: they are built fresh on every open, so they
// pick up the new tview.Styles and the new col* values by themselves.
func (a *App) restyle() {
	applyTheme() // the defaults any widget built after this point inherits

	a.connList.SetMainTextColor(colFg).SetSecondaryTextColor(colMuted).
		SetSelectedStyle(tcell.StyleDefault.
			Background(colSel).Foreground(colFg).Bold(true))
	pane(a.connList.Box, colPanel)

	a.editor.SetTextStyle(tcell.StyleDefault.Background(colBg).Foreground(colFg))
	a.editor.SetPlaceholderStyle(tcell.StyleDefault.Background(colBg).Foreground(colMuted))
	a.editor.SetSelectedStyle(tcell.StyleDefault.Background(colSel).Foreground(colFg))
	pane(a.editor.Box, colBg)

	a.table.SetSelectedStyle(tcell.StyleDefault.
		Background(colSel).Foreground(colFg).Bold(true))
	pane(a.table.Box, colBg)

	// A TextView keeps a SECOND style for its text area, captured from
	// tview.Styles when it was constructed, and pane() only reaches the Box —
	// so the log's interior would otherwise keep the background the palette
	// had at build time forever. The color is colBg rather than colPanel
	// because that is what it has always been: the border wears the panel,
	// the interior is the deep surface. Setting it explicitly changes nothing
	// about how it looks and everything about whether it follows a theme.
	a.logView.SetTextStyle(tcell.StyleDefault.Background(colBg).Foreground(colFg))
	pane(a.logView.Box, colPanel)

	a.status.SetTextColor(colMuted).SetBackgroundColor(colPanel2)
	a.stopBtn.SetStyle(tcell.StyleDefault.
		Background(colPanel2).Foreground(colErr).Bold(true))
	a.stopBtn.SetActivatedStyle(tcell.StyleDefault.
		Background(colErr).Foreground(colBg).Bold(true))
	a.statusRow.SetBackgroundColor(colPanel2)

	a.restyleLayout()
}

// restyleLayout paints the containers between the panes. They are separate
// from restyle only because build constructs them after the widgets, so it
// calls this second; a theme change runs both through restyle.
//
// Skipped when the layout does not exist yet, which is the state build is in
// when it calls restyle the first time.
func (a *App) restyleLayout() {
	for _, f := range []*tview.Flex{a.root, a.mainRow, a.rightCol} {
		if f != nil {
			f.SetBackgroundColor(colBg)
		}
	}
	if a.pages != nil {
		a.pages.SetBackgroundColor(colBg)
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
