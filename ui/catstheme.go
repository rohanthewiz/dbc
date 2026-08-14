package ui

import (
	"encoding/json"

	"github.com/rohanthewiz/dbc/cats"
	"github.com/rohanthewiz/dbc/theme"
)

// Wearing the host's colors: dbc in a cats pane paints itself from the same
// palette everything else in that window uses, so a pane running dbc does not
// read as a foreign application pasted into the session.
//
// THE MAPPING IS SMALL BECAUSE THE TWO PALETTES ARE RELATED. dbc's ten
// surfaces were ported from cdx and cats' theme system descends from the same
// place, so nine of the ten are a rename rather than a translation. Only the
// selection has to be computed, and it is computed the way cats computes its
// own.
//
//	host      dbc       note
//	bg     →  Bg
//	fg     →  Fg
//	muted  →  Muted
//	line   →  Line
//	accent →  Accent
//	warn   →  Warn
//	err    →  Err
//	panel  →  Panel     the host's second surface
//	panel2 →  Panel2    the raised surface
//	(none) →  Sel       accent over bg at 0.30 — cats' own sel-fill recipe
//
// HEX ONLY, AND ALL OR NOTHING. cats emits rgba() for its translucent keys,
// which has no meaning for a terminal cell that is either one color or
// another. A missing or non-hex value in one of the seven CORE keys abandons
// the whole synthesis rather than shipping a palette that is half the host's
// and half dbc's — a half-translated theme looks like a bug, and the built-in
// palette is a perfectly good answer.
//
// The host's `ok` key is deliberately unused: dbc's success tag is its accent
// by design, so importing a separate green would introduce a distinction the
// rest of the UI does not make.

// catsThemeKeys is what the mapping needs, in the order it reads them. All
// seven must be present and hex.
var catsThemeCore = []struct {
	host string
	set  func(*theme.Palette, string)
}{
	{"bg", func(p *theme.Palette, v string) { p.Bg = v }},
	{"fg", func(p *theme.Palette, v string) { p.Fg = v }},
	{"muted", func(p *theme.Palette, v string) { p.Muted = v }},
	{"line", func(p *theme.Palette, v string) { p.Line = v }},
	{"accent", func(p *theme.Palette, v string) { p.Accent = v }},
	{"warn", func(p *theme.Palette, v string) { p.Warn = v }},
	{"err", func(p *theme.Palette, v string) { p.Err = v }},
}

// catsSelAlpha is how much accent goes into the selection color. It matches
// the weight cats uses to derive its own sel-fill, so a dbc selection and a
// cats selection in the next pane are the same color rather than two guesses
// at one.
const catsSelAlpha = 0.30

// catsHostPalette turns a host theme into a dbc palette. ok is false when the
// host's colors cannot produce a complete one — see the file comment.
func catsHostPalette(colors map[string]string) (theme.Palette, bool) {
	if len(colors) == 0 {
		return theme.Palette{}, false
	}
	var p theme.Palette
	for _, k := range catsThemeCore {
		v := colors[k.host]
		if _, _, _, ok := theme.ParseHex(v); !ok {
			return theme.Palette{}, false
		}
		k.set(&p, v)
	}
	// The two surfaces are optional: a host theme that names neither still
	// produces a usable palette, it is just flatter. Falling back inward
	// (panel2→panel→bg) keeps the depth ordering intact either way.
	p.Panel = catsHexOr(colors["panel"], p.Bg)
	p.Panel2 = catsHexOr(colors["panel2"], p.Panel)
	p.Sel = theme.Blend(p.Accent, p.Bg, catsSelAlpha)
	return p, true
}

// catsHexOr returns v when it is a usable hex color, and the fallback
// otherwise — which is how an rgba() value or a missing key is absorbed.
func catsHexOr(v, fallback string) string {
	if _, _, _, ok := theme.ParseHex(v); ok {
		return v
	}
	return fallback
}

// catsThemeAtStartup fetches the host's palette before any widget is built,
// so the FIRST frame is already in the host's colors.
//
// This is the one control call dbc makes synchronously, and it is bounded by
// the probe timeout — half a second in the worst case, and only ever inside a
// cats pane where the socket is local. The alternative, fetching on a
// goroutine and restyling when it lands, would paint one frame in the wrong
// palette and then repaint: a visible flash on every launch, to save
// milliseconds nobody can perceive.
//
// It deliberately does not probe first. A ping followed by a config.get is
// two round trips to answer one question; a config.get that fails IS the
// negative answer, and the real probe runs later on its own goroutine.
func catsThemeAtStartup() {
	env := cats.DetectEnv()
	if !env.InCats || env.ControlSocket == "" {
		return
	}
	client := &cats.Client{Socket: env.ControlSocket, Timeout: cats.ProbeTimeout}
	res, err := client.ConfigGet()
	if err != nil {
		return
	}
	if p, ok := catsHostPalette(res.Theme.Colors); ok {
		setPalette(p)
	}
}

// catsSubscribe opens the event stream. Called once Tier 1 is confirmed.
//
// The filter names events and never a pane, which is load-bearing for the
// theme: theme_changed is SESSION-scoped and cats emits it against pane 0, so
// a subscription narrowed to our own pane would never see it.
func (a *App) catsSubscribe() {
	if a.cats.stream != nil || !a.catsTier1() {
		return
	}
	a.cats.stream = cats.Subscribe(a.cats.caps.ControlSocket,
		cats.SubscribeFilter{Events: []string{
			cats.EventThemeChanged,
			cats.EventPaneAgent,
			cats.EventPaneNotify,
			cats.EventPaneAdded,
			cats.EventPaneRemoved,
			cats.EventFocusChanged,
		}},
		// Runs on the reader goroutine: post and nothing else.
		func(ev cats.Event) {
			a.catsPost(func() { a.catsFrame(ev) })
		},
		func(up bool, _ error) {
			a.catsPost(func() { a.cats.caps.Control = up })
		})
}

// catsFrame dispatches one event. Runs on the UI goroutine.
//
// An unknown name is ignored rather than logged: the event vocabulary grows
// on the host's schedule, and a client that complained about names newer than
// itself would turn every cats upgrade into noise.
func (a *App) catsFrame(ev cats.Event) {
	switch ev.Name {
	case cats.EventThemeChanged:
		var t cats.ThemeChangedEvent
		if err := json.Unmarshal(ev.Data, &t); err != nil {
			return
		}
		a.catsThemeArrived(t)
	}
}

// catsThemeArrived installs a host palette that arrived over the stream.
//
// The same-palette check is what makes this cheap enough to run on every
// frame: cats broadcasts its theme after any config change, including ones
// that touched no color at all, and a repaint that changes nothing still
// costs a full redraw.
func (a *App) catsThemeArrived(t cats.ThemeChangedEvent) {
	p, ok := catsHostPalette(t.Colors)
	if !ok || p == palette {
		return
	}
	setPalette(p)
	a.restyle()
	name := t.Name
	if name == "" {
		name = "host"
	}
	// Said out loud because the log keeps the tags older lines were written
	// with: this line is where the color changes, and naming it is what makes
	// that read as a theme change rather than as a rendering fault.
	a.logf(tagOk+"theme synced to %s", name)
}
