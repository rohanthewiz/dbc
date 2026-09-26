package theme

import (
	"fmt"
	"strconv"
	"strings"
)

// Palette is the ten surfaces dbc paints, as hex strings. The constants above
// are one instance of it — the built-in muted green — and Default returns
// them; anything else is a palette handed in from outside, which today means
// the colors of the cats host dbc is running inside (see FromHost in host.go).
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

// Light is the palette's daylight variant: the same muted green on paper
// instead of on slate, for dbc web's light mode (a terminal brings its own
// background, so the TUI never needs one). The values are the ones the
// standalone plan page's light toggle uses (explain/assets/plan.css), so a
// plan opened from a light workbench looks like the workbench it came from.
func Light() Palette {
	return Palette{
		Bg: "#f6f8f6", Panel: "#ffffff", Panel2: "#eef2ee", Sel: "#d7e8dc", Line: "#d3dbd4",
		Fg: "#1f2a22", Muted: "#5f7066", Accent: "#2e8f5f", Warn: "#b7791f", Err: "#c53d3d",
	}
}

// ByName is the built-in palette a setting names: "dark" (or "", the
// default) is Default, "light" is Light. Case and surrounding space are
// ignored. Anything else is an error rather than a quiet dark, so a typo in
// a flag is reported instead of producing the picture that was not asked for.
//
// Only the two built-ins are nameable. A host palette (FromHost) belongs to
// the terminal dbc happens to run in, and a plan picture is sent to people
// who are not sitting in it — see Default's note on the HTML exporter.
func ByName(name string) (Palette, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "dark":
		return Default(), nil
	case "light":
		return Light(), nil
	}
	return Palette{}, fmt.Errorf("unknown theme %q (use light or dark)", name)
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
