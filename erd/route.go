package erd

import (
	"math"
	"slices"
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
//
// BUSES. Lines that leave one port slot (see setPorts) already start as
// one: a hub's forty keys to its id all meet its row at one point, under
// one marker. Between columns they used to part at once, each with its own
// lane through every gap it crossed, so a 400-child star needed a lane per
// line in its first wrapped column's gaps and widen stretched that column
// to hold them. Such lines say the same thing at their shared end, so
// they may also share the way out of it: lines from one slot that take
// the same holes, in the same order, share ONE lane through each of them,
// and part only where their holes do. The routes form a tree rooted at
// the slot, and a reader follows a trunk from the hub and then the branch
// to the box they want:
//
//	 hub        col 1          col 2          col 3
//	┌───┐      ┌─────┐
//	│ id│||──┬─┤     │
//	└───┘    │ └─────┘
//	         ╰───────────┬──────────────╮     ← one lane through col 1's
//	           ┌─────┐   │ ┌─────┐      │       gap for every line that
//	           │     │   │ │     │      │       took it; it forks in the gap
//	           └─────┘   ╰<│ c2a │      │       after, never inside a hole
//	                       └─────┘      ╰───<│ c3a │
//
// The shared run is the line's PREFIX from its anchored end: two lines
// share a lane in a hole only if they share the slot and every hole
// before it. Merging lines that had parted would join two branches back
// into one, and a reader could no longer tell which branch went on where;
// a prefix keeps it a tree. Each line's prefixes are nodes of a trie (bus)
// rooted at its slot; a hole's lanes are its distinct nodes, not its
// lines, and a line that joins a node already through a hole adds no lane
// there (hole.cost charges it nothing), so lines from a slot gather into
// trunks rather than spreading out. A line is anchored at whichever of its
// ends shares its slot with more lines (the left on a tie), which for a
// hub is the hub's end; holes are chosen from that end outward.
//
// A line whose slot it shares with no other line is a trie of one node per
// hole: one lane, exactly as before buses.

// Routing constants, in CSS pixels.
const (
	holePad  = 5.0 // kept clear between a lane and the boxes either side of it
	laneGap  = 6.0 // between two lanes, when the hole has room
	minLane  = 3.0 // closest two lanes of a crowded hole get
	laneLoad = 6.0 // charge for each lane already through a hole
	overLoad = 120.0
	growLoad = 12.0 // overLoad's stand-in while measuring: what widening a gap one lane costs
	// earlyTilt breaks a shared slot's travel ties toward climbing early (see
	// chooseHoles): per gap out, a fraction of the travel far below a pixel
	earlyTilt = 1e-6
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
	buses    map[*bus]int // the lanes through it: each bus, with how many of users ride it
	measure  bool         // routing to measure demand for widen: growLoad past capacity, not overLoad
}

// lanes is how many lanes the hole needs: one per bus through it, however
// many lines ride each.
func (h *hole) lanes() int { return len(h.buses) }

// join puts x through the hole on bus b (its route's node at this hole).
func (h *hole) join(x *crossing, b *bus) {
	h.users = append(h.users, x)
	if h.buses == nil {
		h.buses = map[*bus]int{}
	}
	h.buses[b]++
}

// bus is one node of a slot's route trie (see BUSES above): the lines from
// one port slot that took the same holes, in the same order, from their
// anchored end up to and including this node's hole. They ride one lane
// through that hole. The root is the slot itself, with no hole.
type bus struct {
	h    *hole
	next map[*hole]*bus // the routes that go on from here, by their next hole
	// shared is set on a root whose slot more than one line leaves: its
	// lines choose holes with chooseHoles' early tilt
	shared bool
}

// step is the existing node for going on from b through h, or nil when no
// line has gone that way yet (or b is itself nil: a route no line has
// taken has no continuations either). It never creates a node, so
// chooseHoles can price any route without changing the trie.
func (b *bus) step(h *hole) *bus {
	if b == nil {
		return nil
	}
	return b.next[h]
}

// grow is step's node, made if no line has gone that way yet.
func (b *bus) grow(h *hole) *bus {
	if n := b.next[h]; n != nil {
		return n
	}
	if b.next == nil {
		b.next = map[*hole]*bus{}
	}
	n := &bus{h: h}
	b.next[h] = n
	return n
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

// cost is the charge for one more line through the hole on bus b (nil for
// a route no line has taken yet). Riding a bus already through the hole
// is free: it adds no lane, and its line is drawn over one that is there.
// A new lane pays laneLoad per lane already in it (a crowded hole pushes
// lanes toward its edges, and a line through the open side is pushed
// further out), and a steep overLoad past its capacity, so lines only
// share a lane's width when every hole near them is full.
func (h *hole) cost(b *bus) float64 {
	if b != nil && h.buses[b] > 0 {
		return 0
	}
	n := h.lanes()
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
	buses  []*bus  // its route's node at each of holes: the lane it rides there
}

// laneOf is the bus x rides through h, which decides its lane: lines on
// one bus share one. A crossing made without buses (a test's) rides a
// lane of its own.
func (x *crossing) laneOf(h *hole) any {
	for i, y := range x.holes {
		if y == h && i < len(x.buses) {
			return x.buses[i]
		}
	}
	return x
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

	// 2. Choose each crossing line's holes, from its anchored end (see
	// BUSES): roots holds each slot's trie, made when a line first leaves
	// the slot across a column.
	roots := map[int]*bus{}
	root := func(e *end) *bus {
		if roots[e.slot] == nil {
			roots[e.slot] = &bus{shared: e.share > 1}
		}
		return roots[e.slot]
	}
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
		el, er := ends[r][1], ends[r][0]
		if c.col < p.col {
			x.l, x.rb, x.yl, x.yr = c, p, cy, py
			el, er = er, el
		}
		way := cols[x.l.col+1 : x.rb.col]
		if er.share > el.share {
			// anchored on the right: choose from the right end leftward,
			// then put the holes (and their nodes) back left to right
			back := make([]*column, len(way))
			for k, c := range way {
				back[len(way)-1-k] = c
			}
			x.holes, x.buses = chooseHoles(back, x.yr, x.yl, root(er))
			slices.Reverse(x.holes)
			slices.Reverse(x.buses)
		} else {
			x.holes, x.buses = chooseHoles(way, x.yl, x.yr, root(el))
		}
		for k, h := range x.holes {
			h.join(x, x.buses[k])
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
	slot  int     // its slot's number, unique within one setPorts: the ends that share a point and a marker
	share int     // how many ends are in that slot, itself included
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

	nslots := 0
	for _, k := range order {
		es := ports[k]
		// the port's distinct markers, each with the mean height of its
		// lines' other ends
		type slot struct {
			kind   marker
			id     int
			sum    float64
			n      int
			offset float64
		}
		var slots []*slot
		byKind := map[marker]*slot{}
		for _, e := range es {
			s := byKind[e.kind]
			if s == nil {
				s = &slot{kind: e.kind, id: nslots}
				nslots++
				byKind[e.kind] = s
				slots = append(slots, s)
			}
			s.sum += e.other
			s.n++
		}
		// every end learns its slot, which route anchors buses on
		for _, e := range es {
			e.slot, e.share = byKind[e.kind].id, byKind[e.kind].n
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
			// lanes, not lines: a bus of any size is one lane wide
			n := c.holes[j].lanes()
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
// two boxes, in order from its anchored end) for a line from height y0 (the
// anchored end) to height y1, riding the buses of the trie at root where
// it can. It returns the holes and, for each, the bus the line rides
// through it, made where the line is the first to go that way.
//
// Dynamic programming over the columns: best[j] is the cheapest way to
// reach column k's hole j from y0, where a step costs its vertical travel
// plus the hole's cost (see hole.cost). Vertical travel is what makes a
// line hard to follow — every column crosses the same horizontal distance
// whichever hole it picks. A column has at most a few dozen holes, so the
// k·n² table is small; ties keep the earlier (upper) hole, so the choice
// is deterministic.
//
// A hole's cost depends on the route to it (riding an existing bus is
// free, and which bus that is depends on every hole before), so each cell
// also carries the bus its own cheapest route reaches. That prices a hole
// by the best route to its predecessor only, not by every route — the
// table is no longer exact, but a cheaper route the table misses is one
// that rides a bus its predecessor's best route did not, which costs at
// most one lane's charge, and the order the lines are routed in already
// decides more than that.
//
// EARLY TILT. Every route that only ever travels toward y1 travels the
// same distance, so ties are common, and a lone line keeps the old rule:
// the upper hole wins. A bus's lines are better served by doing their
// climbing or falling early, in the gaps near the anchored end, where
// they fan out of their slot anyway, and then running straight to their
// boxes: the trunks are then one per row of targets, and each branch
// leaves the trunk once, at its box. Taking the upper hole instead keeps
// every line below the slot on one trunk at the slot's height until the
// last moment, so each gap holds a comb of curves dropping from one point.
// So for a shared slot's lines, the vertical travel in the k-th gap out
// costs (1 + k·earlyTilt) times its length: far too little to outweigh a
// pixel of real travel or any hole's charge, enough to break the tie.
func chooseHoles(cols []*column, y0, y1 float64, root *bus) ([]*hole, []*bus) {
	if len(cols) == 0 {
		return nil, nil
	}
	tilt := 0.0
	if root != nil && root.shared {
		tilt = earlyTilt
	}
	// travel is the charge for climbing or falling d in the k-th gap out
	// from the anchored end
	travel := func(k int, d float64) float64 { return math.Abs(d) * (1 + float64(k)*tilt) }
	type cell struct {
		cost float64
		from int
		bus  *bus // the node this route reaches; nil once it leaves every existing route
	}
	table := make([][]cell, len(cols))
	for k, c := range cols {
		table[k] = make([]cell, len(c.holes))
		for j, h := range c.holes {
			best := cell{cost: math.Inf(1), from: -1}
			if k == 0 {
				best.cost = travel(0, h.at()-y0)
				best.bus = root.step(h)
				best.cost += h.cost(best.bus)
			} else {
				for i, ph := range cols[k-1].holes {
					b := table[k-1][i].bus.step(h)
					if v := table[k-1][i].cost + travel(k, h.at()-ph.at()) + h.cost(b); v < best.cost {
						best = cell{cost: v, from: i, bus: b}
					}
				}
			}
			table[k][j] = best
		}
	}
	// the last column's hole also pays the travel to y1
	last := len(cols) - 1
	j, bestCost := 0, math.Inf(1)
	for i, h := range cols[last].holes {
		if v := table[last][i].cost + travel(last+1, y1-h.at()); v < bestCost {
			j, bestCost = i, v
		}
	}
	out := make([]*hole, len(cols))
	for k := last; k >= 0; k-- {
		out[k] = cols[k].holes[j]
		j = table[k][j].from
	}
	// the route joins the trie: the buses it rode, then new nodes from
	// where it first went its own way
	buses := make([]*bus, len(out))
	at := root
	for k, h := range out {
		at = at.grow(h)
		buses[k] = at
	}
	return out, buses
}

// setLanes orders a hole's lines (see crossing.laneKey) and gives each its
// height. In a band between boxes the lanes are centred and laneGap apart,
// closing up to fit when there are many; in the open space above or below
// a column they stack outward from the box, laneGap apart, the first one
// stackGap/2 out — as far as a band's middle lane is from its boxes.
//
// The lines riding one bus share its lane. A bus is ordered by the mean of
// its lines' keys: they all come from one height on the anchored side and
// fan out to several on the other, so the mean is where the bus is headed
// on the whole.
func setLanes(h *hole) {
	if len(h.users) == 0 {
		return
	}
	type lane struct {
		mid, prev, nx float64
		n             int
		idx           int // its first line's place in the schema, for stable ties
		lines         []*crossing
	}
	var lanes []*lane
	byID := map[any]*lane{}
	for _, x := range h.users {
		id := x.laneOf(h)
		ln := byID[id]
		if ln == nil {
			ln = &lane{idx: x.idx}
			byID[id] = ln
			lanes = append(lanes, ln)
		}
		m, p, nx := x.laneKey(h)
		ln.mid += m
		ln.prev += p
		ln.nx += nx
		ln.n++
		ln.idx = min(ln.idx, x.idx)
		ln.lines = append(ln.lines, x)
	}
	for _, ln := range lanes {
		k := float64(ln.n)
		ln.mid, ln.prev, ln.nx = ln.mid/k, ln.prev/k, ln.nx/k
	}
	sort.SliceStable(lanes, func(i, j int) bool {
		a, b := lanes[i], lanes[j]
		switch {
		case a.mid != b.mid:
			return a.mid < b.mid
		case a.prev != b.prev:
			return a.prev < b.prev
		case a.nx != b.nx:
			return a.nx < b.nx
		}
		return a.idx < b.idx
	})
	// users back in lane order, a bus's lines together
	h.users = h.users[:0]
	for _, ln := range lanes {
		h.users = append(h.users, ln.lines...)
	}
	n := len(lanes)
	h.lane = make(map[*crossing]float64, len(h.users))
	for i, ln := range lanes {
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
		for _, x := range ln.lines {
			h.lane[x] = y
		}
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
