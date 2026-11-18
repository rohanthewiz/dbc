package tui

import "github.com/rohanthewiz/dbc/theme"

// styles is every style the UI paints with, derived from one palette.
//
// It is a value built by newStyles rather than a set of package globals: a
// theme arriving from the cats host mid-session replaces the whole value in
// one assignment (m.st = newStyles(p)), and the next frame is drawn in it.
// There is no restyle pass over existing widgets, because nothing keeps a
// style between frames — every frame is drawn from scratch.
type styles struct {
	pal theme.Palette

	// lowColor is set when the terminal shows fewer than 256 colours, or
	// none (NO_COLOR): Bubble Tea's colour profile then strips or
	// downsamples the palette, and a selection drawn as a background alone
	// can vanish into the cells around it. picked draws it in reverse
	// video instead (N-294). It is the Model's (Update's ColorProfileMsg),
	// kept across a theme change.
	lowColor bool

	base   Style // text on the deepest surface (editor, grid)
	panel  Style // text on the sidebar/log surface
	raised Style // text on the status bar / modal fields
	muted  Style
	accent Style
	warn   Style
	err    Style
	ok     Style

	border      Style // a pane's border at rest
	borderFocus Style // the pane that has the keyboard
	title       Style
	titleFocus  Style

	sel      Style // the cursor row/cell, a selected list item
	selRange Style // a multi-cell selection in the grid, one step quieter than sel
	hover    Style // what the mouse is over
	header   Style // the grid's column header band
	null     Style // a real SQL NULL
	lineNo   Style // editor gutter

	button      Style // a clickable chip at rest
	buttonHover Style // under the mouse
	buttonHot   Style // the primary action (Run), always lit
	buttonStop  Style // Stop, while something runs

	scrollTrack Style
	scrollThumb Style

	// syntax colors for the editor
	synKeyword Style
	synString  Style
	synNumber  Style
	synComment Style
	synIdent   Style
	synParam   Style
	synVar     Style // a shell $variable in an answer's code block

	// chat transcript
	chatUser  Style
	chatAgent Style
	chatTool  Style
	chatCode  Style // code block body
}

// newStyles derives the style set from a palette. Only ten colors exist, so
// the syntax colors reuse them with attributes to separate what the palette
// cannot: keywords in the accent, strings and numbers in the warm tone,
// comments muted and italic.
func newStyles(p theme.Palette) styles {
	bg, panel, panel2 := hex(p.Bg), hex(p.Panel), hex(p.Panel2)
	sel, line := hex(p.Sel), hex(p.Line)
	fg, muted, accent := hex(p.Fg), hex(p.Muted), hex(p.Accent)
	warn, errc := hex(p.Warn), hex(p.Err)
	on := func(f, b Color) Style { return Style{Fg: f, Bg: b} }

	return styles{
		pal:    p,
		base:   on(fg, bg),
		panel:  on(fg, panel),
		raised: on(fg, panel2),
		muted:  on(muted, bg),
		accent: on(accent, bg),
		warn:   on(warn, bg),
		err:    on(errc, bg),
		ok:     on(accent, bg),

		border:      on(line, bg),
		borderFocus: on(accent, bg),
		title:       on(muted, bg),
		titleFocus:  on(accent, bg).Bold(),

		sel:      on(fg, sel).Bold(),
		selRange: on(fg, hex(theme.Blend(p.Sel, p.Bg, 0.6))),
		hover:    on(fg, hex(theme.Blend(p.Sel, p.Bg, 0.45))),
		header:   on(accent, panel2).Bold(),
		null:     on(muted, bg).Italic(),
		lineNo:   on(line, bg),

		button:      on(fg, panel2),
		buttonHover: on(bg, accent).Bold(),
		buttonHot:   on(accent, panel2).Bold(),
		buttonStop:  on(errc, panel2).Bold(),

		scrollTrack: on(line, bg),
		scrollThumb: on(muted, bg),

		synKeyword: on(accent, bg).Bold(),
		synString:  on(warn, bg),
		synNumber:  on(warn, bg),
		synComment: on(muted, bg).Italic(),
		synIdent:   on(fg, bg).Underline(),
		synParam:   on(errc, bg),
		// a shell $variable: the accent without the keywords' bold. Not
		// synParam's error color — a shell block has a $var on most lines,
		// and they are not mistakes
		synVar: on(accent, bg),

		chatUser:  on(accent, bg).Bold(),
		chatAgent: on(fg, bg),
		chatTool:  on(muted, bg).Italic(),
		chatCode:  on(fg, panel),
	}
}

// onBg returns st painted on a different background — for text that sits on
// the sidebar or status bar rather than the base surface.
func onBg(st Style, bg Style) Style { return st.WithBg(bg.Bg) }

// themeDefault is theme.Default, named for call sites that build styles.
func themeDefault() theme.Palette { return theme.Default() }

// picked is cs as a selected cell: on bg, the selection's background, or
// in reverse video where colours may not show (lowColor), so a selection
// stays visible under NO_COLOR and on a 16-colour terminal (N-294).
func (st styles) picked(cs Style, bg Color) Style {
	if st.lowColor {
		return cs.Reverse()
	}
	return cs.WithBg(bg)
}
