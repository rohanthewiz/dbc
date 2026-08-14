package theme

import (
	"fmt"
	"strconv"
	"strings"
)

// Palette is the ten surfaces dbc paints, as hex strings. The constants above
// are one instance of it — the built-in muted green — and Default returns
// them; anything else is a palette handed in from outside, which today means
// the colors of the cats host dbc is running inside (see ui/catstheme.go).
//
// It is a struct rather than a map because these ten are the whole vocabulary:
// a missing key would be a compile error rather than a black surface nobody
// notices until it is on screen.
type Palette struct {
	Bg     string // deepest surface — editor, results table
	Panel  string // sidebar, log pane
	Panel2 string // status bar, modal fields, table header band
	Sel    string // selected row, hovered row
	Line   string // borders and rules at rest
	Fg     string
	Muted  string
	Accent string
	Warn   string
	Err    string
}

// Default is the built-in palette: the constants this package has always
// exported, in the shape the rest of the program can now swap out.
//
// The HTML exporter deliberately keeps reading the CONSTANTS rather than the
// active palette. An exported report is a document that outlives the session
// and travels to people who are not sitting in this terminal, so it should
// look like dbc rather than like whatever theme the author's multiplexer
// happened to be wearing.
func Default() Palette {
	return Palette{
		Bg: Bg, Panel: Panel, Panel2: Panel2, Sel: Sel, Line: Line,
		Fg: Fg, Muted: Muted, Accent: Accent, Warn: Warn, Err: Err,
	}
}

// Blend mixes fg over bg at the given alpha (0 = bg, 1 = fg) and returns the
// result as "#rrggbb". Both inputs must parse; anything else returns bg
// unchanged, so a caller that is already refusing non-hex input gets a
// defined answer rather than a second error to handle.
//
// It exists because a host palette may not carry every surface dbc needs: a
// selection color is a blend of the accent over the background, which is how
// cats derives its own, so dbc can compute the same thing rather than ask for
// it.
func Blend(fg, bg string, alpha float64) string {
	fr, fgn, fb, ok := ParseHex(fg)
	if !ok {
		return bg
	}
	br, bgn, bb, ok := ParseHex(bg)
	if !ok {
		return bg
	}
	mix := func(f, b uint8) uint8 {
		return uint8(float64(f)*alpha + float64(b)*(1-alpha) + 0.5)
	}
	return fmt.Sprintf("#%02x%02x%02x", mix(fr, br), mix(fgn, bgn), mix(fb, bb))
}

// ParseHex reads "#rgb" or "#rrggbb". It reports false for anything else —
// including the rgba() form a host may use for translucent surfaces, which
// has no meaning for a terminal cell that is either one color or another.
func ParseHex(s string) (r, g, b uint8, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") {
		return 0, 0, 0, false
	}
	h := s[1:]
	if len(h) == 3 {
		// #rgb is #rrggbb with each digit doubled, which is the definition
		// rather than an approximation.
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return 0, 0, 0, false
	}
	n, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return uint8(n >> 16), uint8(n >> 8), uint8(n), true
}
