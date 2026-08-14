package theme

import "testing"

// The struct and the constants must not drift: Default is what makes the
// built-in palette swappable without changing what it looks like.
func TestDefaultMatchesTheConstants(t *testing.T) {
	p := Default()
	cases := []struct{ name, got, want string }{
		{"Bg", p.Bg, Bg}, {"Panel", p.Panel, Panel}, {"Panel2", p.Panel2, Panel2},
		{"Sel", p.Sel, Sel}, {"Line", p.Line, Line}, {"Fg", p.Fg, Fg},
		{"Muted", p.Muted, Muted}, {"Accent", p.Accent, Accent},
		{"Warn", p.Warn, Warn}, {"Err", p.Err, Err},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("Default().%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestParseHex(t *testing.T) {
	cases := []struct {
		in         string
		r, g, b    uint8
		ok         bool
		whyItFails string
	}{
		{in: "#1f2420", r: 0x1f, g: 0x24, b: 0x20, ok: true},
		{in: "#FFF", r: 0xff, g: 0xff, b: 0xff, ok: true},
		{in: "#4db380", r: 0x4d, g: 0xb3, b: 0x80, ok: true},
		{in: "  #000  ", ok: true},
		// A translucent host key: meaningless for a cell that is either one
		// color or another.
		{in: "rgba(0,0,0,0.3)", whyItFails: "not hex"},
		{in: "1f2420", whyItFails: "no leading #"},
		{in: "#12345", whyItFails: "wrong length"},
		{in: "#gggggg", whyItFails: "not hex digits"},
		{in: "", whyItFails: "empty"},
	}
	for _, c := range cases {
		r, g, b, ok := ParseHex(c.in)
		if ok != c.ok {
			t.Errorf("ParseHex(%q) ok = %v, want %v (%s)", c.in, ok, c.ok, c.whyItFails)
			continue
		}
		if ok && (r != c.r || g != c.g || b != c.b) {
			t.Errorf("ParseHex(%q) = %d,%d,%d; want %d,%d,%d", c.in, r, g, b, c.r, c.g, c.b)
		}
	}
}

func TestBlend(t *testing.T) {
	if got := Blend("#ffffff", "#000000", 0); got != "#000000" {
		t.Errorf("alpha 0 should be all background, got %q", got)
	}
	if got := Blend("#ffffff", "#000000", 1); got != "#ffffff" {
		t.Errorf("alpha 1 should be all foreground, got %q", got)
	}
	if got := Blend("#ffffff", "#000000", 0.5); got != "#808080" {
		t.Errorf("a half blend of black and white should be mid gray, got %q", got)
	}
	// Unparseable input yields the background rather than a second error for
	// the caller to handle.
	if got := Blend("rgba(1,2,3,0.5)", "#1f2420", 0.3); got != "#1f2420" {
		t.Errorf("a bad foreground should fall back to the background, got %q", got)
	}
	if got := Blend("#4db380", "nonsense", 0.3); got != "nonsense" {
		t.Errorf("a bad background should be returned unchanged, got %q", got)
	}
}
