package erd

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/raster"
	"github.com/rohanthewiz/dbc/theme"
)

// The diagram as a picture: boxes for tables (name, columns, types, key
// badges), lines for foreign keys from the child's key column to the
// parent's, with crow's-foot markers at both ends.
//
//	parent end                             child end
//	  ─┼┼  exactly one (key NOT NULL)        >o─  zero or many
//	  ─┼o  zero or one (key nullable)        ┼o─  zero or one (1:1 key)
//
// WHY DRAWN IN GO. Like a plan's picture, it has to come out of the shell
// (`dbc erd -t png`) and the TUI, where there is no browser to render a
// Mermaid chart or screenshot a page; one renderer in Go serves all three
// homes and is tested with `go test`.

// Options choose how the picture looks. The zero value is dbc's dark
// palette at 2×.
type Options struct {
	// Palette colors the picture; the zero value is theme.Default(). dbc
	// web passes the palette the page is wearing; the shell and the TUI
	// pass plan_theme (or `dbc erd --theme`), the one setting for every
	// picture dbc draws.
	Palette theme.Palette
	// Scale is device pixels per CSS pixel; 0 means 2 (sharp when zoomed).
	// A diagram too big to draw at it is drawn smaller (see maxPixels).
	Scale float64
}

const (
	// maxPixels bounds the raster: RGBA is 4 bytes a pixel, so 36M pixels
	// is ~144 MB at worst. A schema big enough to hit it is drawn at a
	// smaller scale rather than refused — Around narrows it for a sharper
	// picture.
	maxPixels = 36_000_000
	// maxSide keeps both sides within what every encoder and viewer takes
	// (JPEG's hard limit is 65,535).
	maxSide = 20_000.0
)

// JPEG renders the picture as a JPEG. Quality 90 keeps text edges clean.
func (s *Schema) JPEG(opt Options) ([]byte, error) {
	img, err := s.Picture(opt)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err = jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, serr.Wrap(err, "op", "encode erd jpeg")
	}
	return b.Bytes(), nil
}

// PNG renders the picture as a PNG: lossless, and for flat fills and text
// usually smaller than the JPEG as well.
func (s *Schema) PNG(opt Options) ([]byte, error) {
	img, err := s.Picture(opt)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err = png.Encode(&b, img); err != nil {
		return nil, serr.Wrap(err, "op", "encode erd png")
	}
	return b.Bytes(), nil
}

// Picture draws the diagram.
//
// Two passes: the layout is measured at 1× (so it does not depend on the
// scale), then the image is allocated at its final size and painted.
func (s *Schema) Picture(opt Options) (*image.RGBA, error) {
	fs, err := raster.Fonts()
	if err != nil {
		return nil, err
	}
	if opt.Palette == (theme.Palette{}) {
		opt.Palette = theme.Default()
	}
	meas := raster.NewFaces(fs, 1)
	defer meas.Close()
	l := newLayout(s, meas)

	k := opt.Scale
	if k <= 0 {
		k = 2
	}
	// +1 a side: the image is ceil(w·k) × ceil(h·k), which can round a
	// picture sized exactly at the limit just over it
	k = math.Min(k, math.Sqrt(maxPixels/((l.w+1)*(l.h+1))))
	k = math.Min(k, math.Min(maxSide/l.w, maxSide/l.h))
	img := image.NewRGBA(image.Rect(0, 0, int(math.Ceil(l.w*k)), int(math.Ceil(l.h*k))))
	pt := &raster.Painter{Img: img, K: k, Faces: raster.NewFaces(fs, k)}
	defer pt.Faces.Close()
	pal := newPalette(opt.Palette)
	draw.Draw(img, img.Bounds(), image.NewUniform(pal.bg), image.Point{}, draw.Src)

	d := drawer{pt: pt, pal: pal, meas: meas, l: l}
	d.title(s)
	// lines first, boxes over them: lines are routed around boxes, but a
	// line's ends sit on its boxes' edges, and the boxes' borders should
	// be drawn over the line's anti-aliased fringe there
	drawn := map[[2]pt2]bool{}
	for _, r := range s.Rels {
		d.rel(r, drawn)
	}
	// markers after every line, so no line is drawn across another's
	// marker
	d.markers(s.Rels)
	for _, b := range l.boxes {
		d.box(b)
	}
	if l.looseY >= 0 {
		pt.Text(tTable, pal.muted, margin, l.looseY, "TABLES WITHOUT RELATIONSHIPS")
	}
	return img, nil
}

// drawer holds what every drawing step needs.
type drawer struct {
	pt   *raster.Painter
	pal  palette
	meas *raster.Faces
	l    *layout
}

// title draws the heading and the legend of the markers.
func (d drawer) title(s *Schema) {
	pt, pal := d.pt, d.pal
	x, y := margin, margin
	pt.Text(tTitle, pal.fg, x, y-4, s.Title())

	// the legend: each marker at the end of a short sample line, with its
	// meaning, then the badges
	y += 30
	lx := x
	sample := func(kind marker, label string) {
		cy := y + tLegend.LineH()/2
		pt.Polyline([]pt2{{X: lx, Y: cy}, {X: lx + 36, Y: cy}}, 1.6, pal.edge)
		d.marker(pt2{X: lx, Y: cy}, 1, kind)
		lx += 44
		pt.Text(tLegend, pal.muted, lx, y, label)
		lx += d.meas.Width(tLegend, label) + 22
	}
	sample(mOne, "exactly one")
	sample(mZeroOne, "zero or one")
	sample(mMany, "zero or many")
	for _, bg := range []struct{ tag, label string }{{"PK", "primary key"}, {"FK", "foreign key"}, {"UK", "unique"}} {
		d.badge(bg.tag, lx, y+(tLegend.LineH()-rowH)/2)
		lx += badgeW
		pt.Text(tLegend, pal.muted, lx, y, bg.label)
		lx += d.meas.Width(tLegend, bg.label) + 18
	}
}

// marker is one end of a relationship line.
type marker int

const (
	mOne     marker = iota // ┼┼ exactly one
	mZeroOne               // ┼o zero or one
	mMany                  // >o zero or many
)

// Marker geometry, in CSS px from the box edge along the line.
const (
	stub    = 26.0 // the straight run out of the box that the markers sit on
	barAt1  = 7.0  // the bar nearest the box
	barAt2  = 12.0 // the second bar of "exactly one"
	ringAt  = 18.0 // the circle's centre, for the "zero" of zero-or-…
	ringR   = 4.2
	footLen = 11.0 // a crow's foot's prongs run from here back to the box
	footW   = 6.5  // how far the outer prongs spread either side
	barHalf = 6.0
	edgeW   = 1.6
)

// marker draws a crow's-foot marker at p, the point where the line meets a
// box, with the line running away from the box in direction dir (+1 right,
// -1 left).
func (d drawer) marker(p pt2, dir float64, kind marker) {
	pt, col := d.pt, d.pal.edge
	bar := func(at float64) {
		x := p.X + dir*at
		pt.Polyline([]pt2{{X: x, Y: p.Y - barHalf}, {X: x, Y: p.Y + barHalf}}, edgeW, col)
	}
	ring := func() {
		cx := p.X + dir*ringAt
		pt.Circle(cx, p.Y, ringR, col)
		// hollow: the background punched back in
		pt.Circle(cx, p.Y, ringR-edgeW, d.pal.bg)
	}
	switch kind {
	case mOne:
		bar(barAt1)
		bar(barAt2)
	case mZeroOne:
		bar(barAt1)
		ring()
	case mMany:
		// three prongs from a point on the line out to the box edge: the
		// foot touches the child, which is the "many" side
		q := pt2{X: p.X + dir*footLen, Y: p.Y}
		pt.Polyline([]pt2{q, {X: p.X, Y: p.Y - footW}}, edgeW, col)
		pt.Polyline([]pt2{q, {X: p.X, Y: p.Y + footW}}, edgeW, col)
		ring()
	}
}

// rel draws one relationship: the line layout routed for it (route.go),
// from the child's first key column to the parent's first referenced
// column, with its markers.
//
// Which sides of the boxes it leaves from depends on where they are:
//
//	parent left of child     parent right of child     same column, or itself
//	┌P┐       ┌C┐            ┌C┐       ┌P┐             ┌P┐─╮
//	│ │──────<│ │            │ │>──────│ │             │ │ │  both on the right,
//	└─┘       └─┘            └─┘       └─┘             ├C┤─╯  looping outward
//
// The ends are straight stubs (where the markers sit); a line to a column
// further away than the next one threads between the boxes in its way.
//
// Only the line is drawn here; the markers are drawn by markers, after
// every line, since several lines can share one marker (see setPorts).
//
// Lines from one port slot share their way out of it (route.go's BUSES):
// the same points, to the bit, up to where they part. Stroking that trunk
// once per line would composite its anti-aliased fringe again and again —
// a hub's trunk would come out bold — so drawn holds every segment already
// stroked, and a line skips the run of them at its start and at its end
// (a bus is a prefix from one end, never a stretch in the middle).
func (d drawer) rel(r *Rel, drawn map[[2]pt2]bool) {
	pts := d.l.paths[r].pts
	if len(pts) < 2 {
		return
	}
	seg := func(i int) [2]pt2 { return [2]pt2{pts[i], pts[i+1]} } // segment i: pts[i] → pts[i+1]
	lo, hi := 0, len(pts)-1                                       // the points still to stroke
	for lo < hi && drawn[seg(lo)] {
		lo++
	}
	for hi > lo && drawn[seg(hi-1)] {
		hi--
	}
	for i := 0; i < len(pts)-1; i++ {
		drawn[seg(i)] = true
	}
	if lo < hi {
		d.pt.Polyline(pts[lo:hi+1], edgeW, d.pal.edge)
	}
}

// markers draws every line's two end markers, each distinct one once.
// Lines that share a port's slot end at the same point with the same
// marker; drawing it once per line would composite its anti-aliased edges
// again and again, and it would come out heavier than a lone line's.
func (d drawer) markers(rels []*Rel) {
	type key struct {
		p    pt2
		dir  float64
		kind marker
	}
	done := map[key]bool{}
	draw := func(k key) {
		if !done[k] {
			done[k] = true
			d.marker(k.p, k.dir, k.kind)
		}
	}
	for _, r := range rels {
		p := d.l.paths[r]
		draw(key{p.child, p.cdir, p.ckind})
		draw(key{p.parent, p.pdir, p.pkind})
	}
}

// endMarkers is a relationship's notation: the child end is zero-or-many,
// or zero-or-one for a 1:1 key; the parent end is exactly one, or
// zero-or-one when the key may be NULL.
func endMarkers(r *Rel) (child, parent marker) {
	child, parent = mMany, mOne
	if r.OneToOne() {
		child = mZeroOne
	}
	if r.Optional() {
		parent = mZeroOne
	}
	return child, parent
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// box draws one table.
func (d drawer) box(b *box) {
	pt, pal, m := d.pt, d.pal, d.meas
	const rad = 6.0
	// border, then the header band, then the body inset by the border's
	// width: a filled border is crisper than a stroked one at any scale
	pt.RoundRect(b.x, b.y, b.w, b.h, rad, pal.line)
	pt.RoundRect(b.x+1, b.y+1, b.w-2, b.h-2, rad-1, pal.panel2)
	// The body: square down to one radius above the bottom, then a rounded
	// strip whose top corners fall inside that square part and whose
	// bottom corners match the border's — a square body all the way down
	// would paint over the border's rounded corners.
	pt.RoundRect(b.x+1, b.y+headH, b.w-2, b.h-headH-1-rad, 0, pal.panel)
	pt.RoundRect(b.x+1, b.y+b.h-1-2*rad, b.w-2, 2*rad, rad-1, pal.panel)
	pt.RoundRect(b.x+1, b.y+headH, b.w-2, 1, 0, pal.line)

	// header: the name, and a VIEW tag on a view
	nameW := b.w - 2*padX
	if b.t.View {
		tw := m.Width(tBadge, "VIEW")
		pt.Text(tBadge, pal.muted, b.x+b.w-padX-tw, b.y+(headH-tBadge.LineH())/2, "VIEW")
		nameW -= tw + 8
	}
	pt.Text(tTable, pal.accent, b.x+padX, b.y+(headH-tTable.LineH())/2, m.Fit(tTable, b.t.Label, nameW))

	// rows: badge · name · type (right-aligned)
	y := b.y + headH
	typeX := b.x + padX + badgeW + b.nameW + nameGap
	typeW := b.x + b.w - padX - typeX
	for _, c := range b.rows {
		if tag := badgeTag(c); tag != "" {
			d.badge(tag, b.x+padX, y)
		}
		st, col := tCol, pal.fg
		if c.PK {
			st = tColB
		}
		pt.Text(st, col, b.x+padX+badgeW, y+(rowH-st.LineH())/2, m.Fit(st, c.Name, b.nameW))
		ty := m.Fit(tType, c.Type, typeW)
		pt.Text(tType, pal.muted, b.x+b.w-padX-m.Width(tType, ty), y+(rowH-tType.LineH())/2, ty)
		y += rowH
	}
	if b.more > 0 {
		pt.Text(tCol, pal.muted, b.x+padX+badgeW, y+(rowH-tCol.LineH())/2, "… "+plural(b.more, "more column"))
	}
}

// badgeTag is a column's key badge: PK, FK, UK, or PF for a primary key
// column that is also a foreign key (an identifying relationship's column).
func badgeTag(c *Column) string {
	switch {
	case c.PK && c.FK:
		return "PF"
	case c.PK:
		return "PK"
	case c.FK:
		return "FK"
	case c.Unique:
		return "UK"
	}
	return ""
}

// badge draws a key badge in a row whose top is y: a small tinted pill with
// the tag in it. Primary keys wear the warn color (gold in both palettes),
// foreign keys the accent, unique keys the muted gray, so a key's role is
// readable at a glance and in the legend.
func (d drawer) badge(tag string, x, y float64) {
	pal := d.pal
	var col color.RGBA
	switch tag {
	case "PK", "PF":
		col = pal.warn
	case "FK":
		col = pal.accent
	default:
		col = pal.muted
	}
	const w, h = 20.0, 13.0
	by := y + (rowH-h)/2
	d.pt.RoundRect(x, by, w, h, 3, raster.Mix(col, pal.panel, 0.22))
	tw := d.meas.Width(tBadge, tag)
	d.pt.Text(tBadge, col, x+(w-tw)/2, by+(h-tBadge.LineH())/2, tag)
}
