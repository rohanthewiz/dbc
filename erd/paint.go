package erd

import (
	"image/color"

	"github.com/rohanthewiz/dbc/raster"
	"github.com/rohanthewiz/dbc/theme"
)

// The diagram's text styles and colors. The drawing itself — the Go fonts,
// text measuring and fitting, filled rounded rectangles and the polyline
// stroke its lines and markers are made of — is the raster package's,
// shared with the plan's picture (explain/picture.go).
//
// UNITS. Layout is in CSS pixels, measured at 1× with unhinted faces
// (unhinted advances scale linearly), and painted at k device pixels per
// CSS pixel, so the layout never depends on the scale it is drawn at.

// pt2 and txt are raster's point and text style under the short names the
// layout, the router and the drawer use throughout.
type (
	pt2 = raster.Pt
	txt = raster.Style
)

var (
	tTitle  = txt{Font: raster.Bold, Size: 16}
	tTable  = txt{Font: raster.Bold, Size: 13}
	tCol    = txt{Font: raster.Regular, Size: 12}
	tColB   = txt{Font: raster.Bold, Size: 12}
	tType   = txt{Font: raster.Mono, Size: 11}
	tBadge  = txt{Font: raster.Mono, Size: 9.5}
	tLegend = txt{Font: raster.Regular, Size: 11.5}
)

// palette is the theme's hex colors, parsed, plus the few mixes the
// diagram uses.
type palette struct {
	bg, panel, panel2, line, fg, muted, accent, warn color.RGBA
	edge                                             color.RGBA // relationship lines
}

func newPalette(p theme.Palette) palette {
	c := raster.RGB
	out := palette{bg: c(p.Bg), panel: c(p.Panel), panel2: c(p.Panel2), line: c(p.Line), fg: c(p.Fg),
		muted: c(p.Muted), accent: c(p.Accent), warn: c(p.Warn)}
	// lines a little quieter than the accent the table names wear, so the
	// boxes read first and the lines second
	out.edge = raster.Mix(out.accent, out.muted, 0.7)
	return out
}
