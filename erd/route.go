package erd

import (
	"math"
	"sort"

	"github.com/rohanthewiz/dbc/raster"
)

// Routing: the path each relationship's line takes between its two boxes.
//
// A line between neighbouring columns has a clear run: it leaves one box
// sideways, crosses the empty gap between the columns in one S-curve, and
// enters the other box. A line to a column further away — a far rank, or a
// hub's children wrapped into several columns — has whole columns of boxes
// in its way. Drawn straight, it passes under them (lines are painted
// first), and a reader loses it behind a box and has to guess which of the
// lines coming out the other side is the same one.
//
// CHANNEL ROUTING. Instead, such a line crosses every column in its way
// through a "hole": one of the stackGap-high bands between two boxes of
// that column, or the open space above its top box or below its bottom
// one. Between columns it curves from one hole's height to the next's,
// with the same horizontal-tangent S-curve an adjacent line uses, so every
// line is still one smooth, left-to-right stroke:
//
//	 col i       gap       col i+1       gap       col i+2
//	┌─────┐               ┌─────┐
//	│  L  │──╮            │     │
//	└─────┘  ╰──────╮     └─────┘
//	                ╰─────────────────╮      ← the hole between
//	                      ┌─────┐     │        col i+1's two boxes
//	                      │     │     │      ┌─────┐
//	                      │     │     ╰────<│  R  │
//	                      └─────┘            └─────┘
//
// WHY NOT DUMMY NODES. Full Sugiyama puts a dummy node on every rank a long
// edge crosses and lets the ordering sweeps place them, which also reorders
// the real boxes around the dummies. Here the boxes are placed first and
// never moved for a line: a diagram's boxes are what a reader navigates by,
// and a schema with one more foreign key should not reshuffle them. The
// holes already exist (stackGap between boxes, the space around a short
// column); routing only chooses among them, after the fact, which keeps the
// layout deterministic and this step independent of it.
//
// The steps, per connected group:
//  1. holes: each column's free bands, top to bottom.
//  2. choose: each line that skips columns picks one hole per column in its
//     way by dynamic programming over the columns, minimising the total
//     vertical travel plus a charge for every line already using a hole —
//     so a hub's forty lines spread over the holes near their targets
//     rather than all squeezing through the one nearest the hub. Lines are
//     routed in the schema's relationship order, so the result is stable.
//  3. lanes: the lines through one hole are spread across it in the order
//     of the heights they come from and go to, which keeps them from
//     crossing each other inside it.
//  4. points: straight runs through the holes (and out of each box, past
//     its markers, to its column's edge) joined by S-curves in the gaps.
//
// A line routed above a column's top box can rise above the group's first
// row of boxes; layGroup then moves the group down to make room.

// Routing constants, in CSS pixels.
const (
	holePad  = 5.0 // kept clear between a lane and the boxes either side of it
	laneGap  = 6.0 // between two lanes, when the hole has room
	minLane  = 3.0 // closest two lanes of a crowded hole get
	laneLoad = 6.0 // charge for each line already through a hole
	overLoad = 120.0
	growLoad = 12.0 // overLoad's stand-in while measuring: what widening a gap one lane costs
)

// column is one column of boxes in a group, as placed.
type column struct {
	x, w  float64
	boxes []*box  // top to bottom
	holes []*hole // top to bottom: above the first box, between each pair, below the last
}

// hole is a horizontal band across a column that no box of the column
// covers, which a line can cross the column through.
type hole struct {
	top, bot float64     // the band; -Inf/+Inf for the open side above the top box / below the bottom one
	users    []*crossing // the lines through it, in lane order once lanes are set
	lane     map[*crossing]float64
	measure  bool // routing to measure demand for widen: growLoad past capacity, not overLoad
}

// bounded reports whether the hole is between two boxes (so its lanes must
// fit in stackGap) rather than above or below the column.
func (h *hole) bounded() bool { return !math.IsInf(h.top, 0) && !math.IsInf(h.bot, 0) }

// at is the height a line would cross at, for choosing: a band's middle,
// or one lane out from the box for the open sides.
func (h *hole) at() float64 {
	switch {
	case math.IsInf(h.top, -1):
		return h.bot - stackGap/2
	case math.IsInf(h.bot, 1):
		return h.top + stackGap/2
	}
	return (h.top + h.bot) / 2
}

// capacity is how many lanes fit in a bounded hole at minLane apart.
func (h *hole) capacity() int {
	if !h.bounded() {
		return math.MaxInt
	}
	return int((h.bot-h.top-2*holePad)/minLane) + 1
}

// cost is the charge for one more line through the hole: laneLoad per line
// already in it (a crowded hole pushes lanes toward its edges, and a line
// through the open side is pushed further out), and a steep overLoad past
// its capacity, so lines only share a lane's width when every hole near
// them is full.
func (h *hole) cost() float64 {
	n := len(h.users)
	c := laneLoad * float64(n)
	if over := n - h.capacity() + 1; over > 0 {
		if h.measure {
			// while measuring, a full gap can still be widened, which
			// costs the column a few pixels of height rather than the
			// line a detour over or under the column: past capacity is
			// dearer than a gap with room, so lines still spread first,
			// but far cheaper than going round
			c += growLoad * float64(over)
		} else {
			c += overLoad * float64(over)
		}
	}
	return c
}

// path is a relationship's line, ready to draw.
type path struct {
	pts           []pt2   // the stroke, box edge to box edge
	child, parent pt2     // where it meets each box: its markers go here
	cdir, pdir    float64 // the way it leaves each box: +1 right, -1 left
	ckind, pkind  marker  // the marker at each end (see endMarkers)
}

// crossing is a line between two different columns while it is routed.
type crossing struct {
	r      *Rel
	idx    int  // the relationship's place in the schema, for stable ties
	l, rb  *box // the box on the left and the one on the right
	yl, yr float64
	holes  []*hole // one per column strictly between l's and rb's
}

// laneKey orders the lines through h: by the heights the line comes from
// and goes to (averaged, then each), so two lines keep their relative
// order across the hole instead of crossing inside it.
func (c *crossing) laneKey(h *hole) (mid, prev, next float64) {
	prev, next = c.yl, c.yr
	for i, x := range c.holes {
		if x == h {
			if i > 0 {
				prev = c.holes[i-1].at()
			}
			if i < len(c.holes)-1 {
				next = c.holes[i+1].at()
			}
		}
	}
	return (prev + next) / 2, prev, next
}

// makeHoles sets a column's holes from its boxes.
func (c *column) makeHoles() {
	c.holes = []*hole{{top: math.Inf(-1), bot: c.boxes[0].y}}
	for i := 1; i < len(c.boxes); i++ {
		a := c.boxes[i-1]
		c.holes = append(c.holes, &hole{top: a.y + a.h, bot: c.boxes[i].y})
	}
	last := c.boxes[len(c.boxes)-1]
	c.holes = append(c.holes, &hole{top: last.y + last.h, bot: math.Inf(1)})
}

// route computes the path of every relationship within one placed group.
// cols are the group's columns, left to right; every box's col is its index.
//
// With measure set, it only chooses holes, as if every gap between boxes
// held any number of lanes, and leaves each hole's users for widen to count;
// it returns nil.
func route(cols []*column, rels []*Rel, byT map[*Table]*box, in map[*box]bool, measure bool) map[*Rel]*path {
	out := map[*Rel]*path{}
	for _, c := range cols {
		c.makeHoles()
		for _, h := range c.holes {
			h.measure = measure
		}
	}

	// 1b. Ports: where on its row each end attaches, fanned out when
	// different markers meet at one row.
	ends := setPorts(rels, byT, in)

	// 2. Choose each crossing line's holes.
	var xs []*crossing
	for i, r := range rels {
		c, p := byT[r.Child], byT[r.Parent]
		if !in[c] {
			continue
		}
		cy, py := ends[r][0].y, ends[r][1].y
		if c.col == p.col {
			out[r] = loopPath(cols[c.col], c, p, cy, py)
			out[r].ckind, out[r].pkind = ends[r][0].kind, ends[r][1].kind
			continue
		}
		x := &crossing{r: r, idx: i, l: p, rb: c, yl: py, yr: cy}
		if c.col < p.col {
			x.l, x.rb, x.yl, x.yr = c, p, cy, py
		}
		x.holes = chooseHoles(cols[x.l.col+1:x.rb.col], x.yl, x.yr)
		for _, h := range x.holes {
			h.users = append(h.users, x)
		}
		xs = append(xs, x)
	}
	if measure {
		return nil
	}

	// 3. Lanes.
	for _, c := range cols {
		for _, h := range c.holes {
			setLanes(h)
		}
	}

	// 4. Points.
	for _, x := range xs {
		p := x.path(cols)
		p.ckind, p.pkind = ends[x.r][0].kind, ends[x.r][1].kind
		out[x.r] = p
	}
	return out
}

// Port constants, in CSS pixels.
const (
	portGap  = 14.0 // between two slots of one port: two markers' bars (barHalf each) and 2 px of air
	portSpan = 20.0 // the most a port's slots spread, first to last, so they stay near their row
	// loopOrder is how far a loop's ordering height is pushed past its
	// real one (see setPorts): further than any picture is tall, and
	// finite, so a slot's mean stays a number
	loopOrder = 1e7
)

// end is one end of a relationship's line while its port is set.
type end struct {
	r     *Rel
	kind  marker
	y     float64 // where it attaches: the row's centre, then its slot's
	other float64 // the row the line's other end attaches to, for ordering slots
}

// portKey names a port: one row of one box, on one side of it. Every line
// ending at the same port attaches to the same row from the same direction.
type portKey struct {
	b    *box
	row  string
	side float64 // +1 right edge, -1 left
}

// setPorts decides where each relationship's two ends attach: [0] is the
// child end, [1] the parent's.
//
// PORTS AND SLOTS. Every end attaches at its key column's row, so several
// foreign keys to one parent column (created_by, updated_by → users.id) all
// meet at one point, on one side, and their markers are drawn over each
// other. When the markers are the same, that is right: the lines join like
// a bus into one marker, which says the same thing once. When they differ
// (one key NOT NULL, one nullable), the overlap reads as ||o, which is no
// notation at all. So each port gets one slot per distinct marker, spread
// down the row portGap apart and centred on it; lines with the same marker
// still share their slot. A port has at most three distinct markers (a
// row's ends can be ||, |o or >o), so the slots stay within portSpan and a
// line still visibly joins its own row (rowH is 21):
//
//	      ┌users────────┐
//	──||──┤PK id        │   slot 1: the NOT NULL keys, sharing one ||
//	──|o──┤             │   slot 2: the nullable keys, sharing one |o
//	      │   name      │
//
// Slots are ordered by the mean height of their lines' other ends, so the
// lines fan out toward where they are going instead of crossing at the box.
//
// A key column that references itself (a loop of zero height) attaches its
// parent end to the header instead, as its own port.
func setPorts(rels []*Rel, byT map[*Table]*box, in map[*box]bool) map[*Rel][2]*end {
	out := map[*Rel][2]*end{}
	ports := map[portKey][]*end{}
	var order []portKey // first-seen order, so the result does not depend on map order
	add := func(k portKey, e *end) {
		if _, ok := ports[k]; !ok {
			order = append(order, k)
		}
		ports[k] = append(ports[k], e)
	}
	for _, r := range rels {
		c, p := byT[r.Child], byT[r.Parent]
		if !in[c] {
			continue
		}
		crow, prow := first(r.ChildCols), first(r.ParentCols)
		if c == p && crow == prow {
			prow = "\x00header" // no column has this name, so rowY gives the header
		}
		// which side each end leaves from: see drawer.rel's picture
		cside, pside := 1.0, 1.0
		switch {
		case c.col < p.col:
			pside = -1
		case c.col > p.col:
			cside = -1
		}
		ck, pk := endMarkers(r)
		ce := &end{r: r, kind: ck, y: c.rowY(crow), other: p.rowY(prow)}
		pe := &end{r: r, kind: pk, y: p.rowY(prow), other: c.rowY(crow)}
		if c.col == p.col {
			// A loop bows out just beside the column, inside every line
			// that leaves the same side for another column, so its slot
			// must be the outermost one toward its other end: were it
			// above a line heading down past it, that line would cross
			// the loop. Pushing its ordering height far that way does it.
			for _, e := range []*end{ce, pe} {
				switch {
				case e.other > e.y:
					e.other = e.y + loopOrder
				case e.other < e.y:
					e.other = e.y - loopOrder
				}
			}
		}
		add(portKey{c, crow, cside}, ce)
		add(portKey{p, prow, pside}, pe)
		out[r] = [2]*end{ce, pe}
	}

	for _, k := range order {
		es := ports[k]
		// the port's distinct markers, each with the mean height of its
		// lines' other ends
		type slot struct {
			kind   marker
			sum    float64
			n      int
			offset float64
		}
		var slots []*slot
		byKind := map[marker]*slot{}
		for _, e := range es {
			s := byKind[e.kind]
			if s == nil {
				s = &slot{kind: e.kind}
				byKind[e.kind] = s
				slots = append(slots, s)
			}
			s.sum += e.other
			s.n++
		}
		if len(slots) < 2 {
			continue
		}
		sort.SliceStable(slots, func(i, j int) bool {
			a, b := slots[i].sum/float64(slots[i].n), slots[j].sum/float64(slots[j].n)
			if a != b {
				return a < b
			}
			return slots[i].kind < slots[j].kind
		})
		n := len(slots)
		sp := math.Min(portGap, portSpan/float64(n-1))
		for i, s := range slots {
			s.offset = (float64(i) - float64(n-1)/2) * sp
		}
		for _, e := range es {
			e.y += byKind[e.kind].offset
		}
	}
	return out
}

// widenRounds bounds widen's measure → widen → place rounds. One round
// settles every diagram but a big hub's; the hub's lines shift a little as
// its children's columns stretch, and a second or third round catches the
// gaps that shift overfills. Past that, a leftover line takes the open
// side, as it would have with no widening at all.
const widenRounds = 4

// widen grows extra (see layGroup's step 4) so that every gap between two
// boxes holds the lines route chose for it in measure mode. Only a gap
// past its capacity grows, and only to fit its lines minLane apart within
// holePad of the boxes — the spacing a crowded gap already has. A gap that
// fits keeps stackGap, so a diagram only changes where lines would
// otherwise have gone round a column, and a hub's wrapped children keep
// their grid wherever they can. Extra only grows: a gap is never narrowed
// again, so the rounds end. It reports whether anything grew.
func widen(cols []*column, extra [][]float64) bool {
	grew := false
	for i, c := range cols {
		// holes[j] is the gap above box j; holes[0] and the last are the
		// open sides, which need no room
		for j := 1; j < len(c.boxes); j++ {
			n := len(c.holes[j].users)
			need := 2*holePad + float64(n-1)*minLane - stackGap
			if need > extra[i][j]+0.5 {
				extra[i][j] = math.Ceil(need)
				grew = true
			}
		}
	}
	return grew
}

// chooseHoles picks one hole in each of cols (the columns between a line's
// two boxes, left to right) for a line from height y0 to height y1.
//
// Dynamic programming over the columns: best[j] is the cheapest way to
// reach column k's hole j from y0, where a step costs its vertical travel
// plus the hole's cost (see hole.cost). Vertical travel is what makes a
// line hard to follow — every column crosses the same horizontal distance
// whichever hole it picks. A column has at most a few dozen holes, so the
// k·n² table is small; ties keep the earlier (upper) hole, so the choice
// is deterministic.
func chooseHoles(cols []*column, y0, y1 float64) []*hole {
	if len(cols) == 0 {
		return nil
	}
	type cell struct {
		cost float64
		from int
	}
	table := make([][]cell, len(cols))
	for k, c := range cols {
		table[k] = make([]cell, len(c.holes))
		for j, h := range c.holes {
			best := cell{cost: math.Inf(1), from: -1}
			if k == 0 {
				best.cost = math.Abs(h.at() - y0)
			} else {
				for i, ph := range cols[k-1].holes {
					if v := table[k-1][i].cost + math.Abs(h.at()-ph.at()); v < best.cost {
						best = cell{cost: v, from: i}
					}
				}
			}
			best.cost += h.cost()
			table[k][j] = best
		}
	}
	// the last column's hole also pays the travel to y1
	last := len(cols) - 1
	j, bestCost := 0, math.Inf(1)
	for i, h := range cols[last].holes {
		if v := table[last][i].cost + math.Abs(y1-h.at()); v < bestCost {
			j, bestCost = i, v
		}
	}
	out := make([]*hole, len(cols))
	for k := last; k >= 0; k-- {
		out[k] = cols[k].holes[j]
		j = table[k][j].from
	}
	return out
}

// setLanes orders a hole's lines (see crossing.laneKey) and gives each its
// height. In a band between boxes the lanes are centred and laneGap apart,
// closing up to fit when there are many; in the open space above or below
// a column they stack outward from the box, laneGap apart, the first one
// stackGap/2 out — as far as a band's middle lane is from its boxes.
func setLanes(h *hole) {
	n := len(h.users)
	if n == 0 {
		return
	}
	sort.SliceStable(h.users, func(i, j int) bool {
		a, b := h.users[i], h.users[j]
		am, ap, an := a.laneKey(h)
		bm, bp, bn := b.laneKey(h)
		switch {
		case am != bm:
			return am < bm
		case ap != bp:
			return ap < bp
		case an != bn:
			return an < bn
		}
		return a.idx < b.idx
	})
	h.lane = make(map[*crossing]float64, n)
	for i, x := range h.users {
		var y float64
		switch {
		case math.IsInf(h.top, -1):
			// the last in order is nearest the box, so lanes stay in
			// order top to bottom
			y = h.bot - stackGap/2 - float64(n-1-i)*laneGap
		case math.IsInf(h.bot, 1):
			y = h.top + stackGap/2 + float64(i)*laneGap
		default:
			sp := laneGap
			if n > 1 {
				sp = math.Min(laneGap, (h.bot-h.top-2*holePad)/float64(n-1))
			}
			y = (h.top+h.bot)/2 + (float64(i)-float64(n-1)/2)*sp
		}
		h.lane[x] = y
	}
}

// run is a straight horizontal piece of a line.
type run struct{ x0, x1, y float64 }

// path turns a routed crossing into points: a run out of the left box past
// its markers and on to its column's edge, a run through each hole, a run
// into the right box from its column's edge, and an S-curve in each gap
// between them.
func (x *crossing) path(cols []*column) *path {
	l, r := x.l, x.rb
	lc, rc := cols[l.col], cols[r.col]
	runs := []run{{l.x + l.w, math.Max(l.x+l.w+stub, lc.x+lc.w), x.yl}}
	for k, h := range x.holes {
		c := cols[l.col+1+k]
		runs = append(runs, run{c.x, c.x + c.w, h.lane[x]})
	}
	runs = append(runs, run{math.Min(r.x-stub, rc.x), r.x, x.yr})

	pts := []pt2{{X: runs[0].x0, Y: runs[0].y}}
	for i := 1; i < len(runs); i++ {
		a, b := runs[i-1], runs[i]
		p, q := pt2{X: a.x1, Y: a.y}, pt2{X: b.x0, Y: b.y}
		// horizontal tangents at both ends, so the line leaves one run
		// and joins the next square; the control reach is the adjacent
		// line's rule from before routing, so a line between neighbouring
		// columns is drawn exactly as it always was
		dx := math.Max(30, (q.X-p.X)*0.45)
		pts = append(pts, raster.CubicPts(p, pt2{X: p.X + dx, Y: p.Y}, pt2{X: q.X - dx, Y: q.Y}, q)...)
	}
	last := runs[len(runs)-1]
	pts = append(pts, pt2{X: last.x1, Y: last.y})

	out := &path{pts: pts}
	lp, rp := pt2{X: l.x + l.w, Y: x.yl}, pt2{X: r.x, Y: x.yr}
	if l.t == x.r.Child {
		out.child, out.cdir, out.parent, out.pdir = lp, 1, rp, -1
	} else {
		out.child, out.cdir, out.parent, out.pdir = rp, -1, lp, 1
	}
	return out
}

// loopPath is the line between two boxes in one column, or a table's key to
// itself: both ends leave on the right and it bows out into the gap beside
// the column.
//
//	┌P┐──╮
//	│ │  │
//	├C┤──╯
//
// Each end runs straight past its markers to the column's edge before it
// bends, so a loop from a box narrower than its column does not cut
// through a wider box stacked between the two.
func loopPath(col *column, c, p *box, cy, py float64) *path {
	if c == p && math.Abs(cy-py) < 1 {
		// a key column referencing itself would be a loop of zero
		// height; attach the parent end to the header instead
		py = p.y + headH/2
	}
	edge := col.x + col.w
	cx, px := c.x+c.w, p.x+p.w
	a := pt2{X: math.Max(cx+stub, edge), Y: cy}
	b := pt2{X: math.Max(px+stub, edge), Y: py}
	// bow out past the further of the two, more for a taller loop so it
	// stays round
	mx := math.Max(a.X, b.X) + 18 + math.Min(60, math.Abs(b.Y-a.Y)*0.15)
	pts := []pt2{{X: cx, Y: cy}}
	pts = append(pts, raster.CubicPts(a, pt2{X: mx, Y: a.Y}, pt2{X: mx, Y: b.Y}, b)...)
	pts = append(pts, pt2{X: px, Y: py})
	return &path{pts: pts, child: pt2{X: cx, Y: cy}, cdir: 1, parent: pt2{X: px, Y: py}, pdir: 1}
}

// shift moves a path down by dy.
func (p *path) shift(dy float64) {
	for i := range p.pts {
		p.pts[i].Y += dy
	}
	p.child.Y += dy
	p.parent.Y += dy
}
