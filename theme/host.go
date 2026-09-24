package theme

// FromHost turns a host terminal's theme (cats' config.get / theme_changed
// colors) into a dbc palette. ok is false when the host's colors cannot make
// a complete one.
//
// THE MAPPING IS SMALL BECAUSE THE TWO PALETTES ARE RELATED. dbc's ten
// surfaces were ported from cdx and cats' theme system descends from the same
// place, so nine of the ten are a rename; only the selection is computed, the
// way cats computes its own:
//
//	host  bg fg muted line accent warn err  panel  panel2   (none)
//	dbc   Bg Fg Muted Line Accent Warn Err  Panel  Panel2   Sel = accent over bg at HostSelAlpha
//
// HEX ONLY, AND ALL OR NOTHING. A missing or non-hex value (cats emits rgba()
// for translucent keys) in one of the seven core keys abandons the whole
// synthesis: a palette half the host's and half dbc's looks like a bug, and
// the built-in palette is a perfectly good answer.
//
// The mapping was first written for the former tview UI (its
// catsHostPalette); this is now its only copy.
func FromHost(colors map[string]string) (Palette, bool) {
	if len(colors) == 0 {
		return Palette{}, false
	}
	var p Palette
	for _, k := range []struct {
		host string
		dst  *string
	}{
		{"bg", &p.Bg}, {"fg", &p.Fg}, {"muted", &p.Muted}, {"line", &p.Line},
		{"accent", &p.Accent}, {"warn", &p.Warn}, {"err", &p.Err},
	} {
		v := colors[k.host]
		if _, _, _, ok := ParseHex(v); !ok {
			return Palette{}, false
		}
		*k.dst = v
	}
	// The two surfaces are optional; falling back inward (panel2 → panel →
	// bg) keeps the depth ordering intact when a host names neither.
	p.Panel = hexOr(colors["panel"], p.Bg)
	p.Panel2 = hexOr(colors["panel2"], p.Panel)
	p.Sel = Blend(p.Accent, p.Bg, HostSelAlpha)
	return p, true
}

// HostSelAlpha is how much accent goes into a host-derived selection color —
// the weight cats uses for its own sel-fill, so a dbc selection and a cats
// selection in the next pane are the same color.
const HostSelAlpha = 0.30

func hexOr(v, fallback string) string {
	if _, _, _, ok := ParseHex(v); ok {
		return v
	}
	return fallback
}
