package erd

import (
	"math"
	"sort"

	"github.com/rohanthewiz/dbc/raster"
)

// The diagram's layout: where every table's box goes.
//
//	┌ title ─────────────────────────────────────────────────────────┐
//	│ ┌owners─┐        ┌cats────┐        ┌visits──┐                  │  a connected
//	│ │PK id  │||───o<│PK id   │||───o<│PF cat_id│                  │  group: one
//	│ │  name │        │FK owner│        │PK seq   │                  │  column per
//	│ └───────┘        └────────┘        └─────────┘                  │  rank, parents
//	│  rank 0           rank 1             rank 2                     │  on the left
//	│                                                                 │
//	│ ┌group 2 …┐                                                     │  next group
//	│                                                                 │
//	│ TABLES WITHOUT RELATIONSHIPS                                    │  the rest,
//	│ ┌a──┐ ┌c──┐ ┌e──┐ ┌g──┐                                          │  packed into
//	│ │   │ └───┘ │   │ └───┘                                          │  columns
//	│ └───┘ ┌d──┐ └───┘                                                │
//	└─────────────────────────────────────────────────────────────────┘
//
// WHY LAYERED, PARENTS LEFT. A foreign key is a direction — the child
// depends on the parent — and a reader follows a schema the same way, from
// the things that exist on their own (customers, products) to the things
// that refer to them (orders, order lines). Ranking tables by that
// direction and giving each rank a column puts every line left-to-right
// from a child's key column to the parent's key, with its markers on the
// short horizontal stubs at either end.
//
// WHY NOT A GENERAL GRAPH LAYOUT. Force-directed and full Sugiyama layouts
// (with dummy nodes, crossing minimisation and coordinate assignment) give
// prettier results on hard graphs but are a large, iterative, hard-to-test
// piece of code, and their output shifts with small schema changes. This
// is the deterministic core of Sugiyama — longest-path ranking, barycenter
// ordering, a few sweeps — which draws the schemas people actually have
// (trees and shallow DAGs around a few hub tables) well, and draws the same
// schema the same way every time.
//
// The steps, per connected group of tables:
//  1. rank: a table's rank is one more than its deepest parent's (a cycle
//     of keys is cut at its back edge first), then every root is pulled
//     right to sit just left of its nearest child, so a lookup table used
//     only by a rank-3 table does not stretch a line across the picture.
//  2. order: each rank's tables are sorted by the mean position of their
//     neighbours, sweeping over all ranks a few times — the barycenter
//     heuristic, which removes most crossings.
//  3. wrap: a rank taller than the group's target height (about square,
//     by area) is split into several columns, so a hub with forty
//     children is a block, not a strip.
//  4. place: columns left to right, each centred vertically.
//  5. route: each line that skips a column finds its way through the gaps
//     between that column's boxes (route.go), rather than under them. A
//     gap that more lines need than it holds is widened first, and the
//     columns placed again (widen), so the lines stay between the boxes.

// Layout constants, in CSS pixels.
const (
	margin     = 28.0
	boxMinW    = 170.0
	boxMaxW    = 380.0
	headH      = 30.0  // a box's header band (the table's name)
	rowH       = 21.0  // one column's row
	padB       = 6.0   // below the last row
	padX       = 10.0  // inside a box, either side
	badgeW     = 26.0  // the PK/FK/UK column
	nameGap    = 16.0  // between a column's name and its type
	rankGap    = 110.0 // between two columns of boxes: room for the lines and markers
	stackGap   = 24.0  // between two boxes in one column
	groupGap   = 56.0  // between two connected groups, and before the loose tables
	looseGap   = 24.0  // between the loose tables' columns
	maxRows    = 40    // columns shown per table before "… n more"
	sweeps     = 8     // barycenter passes; converges long before this on real schemas
	titleH     = 58.0  // the title and legend band
	sectionH   = 26.0  // the "tables without relationships" caption
	minPicture = 720.0 // the title and legend need room even for one table
)

// box is one table's place on the picture.
type box struct {
	t       *Table
	rows    []*Column // the columns shown, in declared order
	more    int       // columns not shown
	nameW   float64   // the name column's width inside the box
	x, y    float64
	w, h    float64
	rank    int
	col     int     // its column's index within its group, left to right
	pos     float64 // position within its rank, 0…1, for the barycenter
	nbrs    []*box  // tables it shares a key with (not itself)
	hasRels bool    // any relationship, a self-reference included
}

// rowY is the vertical centre of a column's row, where a line attaches.
// A column that is not shown (a key column past maxRows cannot be, see
// newBox, but a caller may ask for any name) attaches at the header.
func (b *box) rowY(col string) float64 {
	for i, c := range b.rows {
		if c.Name == col {
			return b.y + headH + float64(i)*rowH + rowH/2
		}
	}
	return b.y + headH/2
}

// layout is a laid-out diagram.
type layout struct {
	boxes  []*box
	byT    map[*Table]*box
	loose  []*box // tables with no relationship at all
	paths  map[*Rel]*path
	looseY float64 // where the loose section's caption goes; <0 when none
	w, h   float64
}

// newBox measures a table's box.
//
// A table wider than boxMaxW has its names and types cut with an ellipsis
// rather than the box growing: one table with a 200-character generated
// column name should not make its whole rank column 2000 px wide.
//
// A table with more than maxRows columns shows its key columns (which lines
// attach to) and then as many of the rest as fit, in declared order, with a
// "… n more" row — a wide fact table is still one screen tall.
func newBox(t *Table, m *raster.Faces) *box {
	b := &box{t: t}
	if len(t.Cols) <= maxRows {
		b.rows = t.Cols
	} else {
		keys := 0
		for _, c := range t.Cols {
			if c.PK || c.FK || c.Unique {
				keys++
			}
		}
		room := maxRows - keys
		for _, c := range t.Cols {
			if c.PK || c.FK || c.Unique || room > 0 {
				if !(c.PK || c.FK || c.Unique) {
					room--
				}
				b.rows = append(b.rows, c)
			}
		}
		b.more = len(t.Cols) - len(b.rows)
	}

	var nameW, typeW float64
	for _, c := range b.rows {
		st := tCol
		if c.PK {
			st = tColB
		}
		nameW = math.Max(nameW, m.Width(st, c.Name))
		typeW = math.Max(typeW, m.Width(tType, c.Type))
	}
	w := padX + badgeW + nameW + nameGap + typeW + padX
	head := padX + m.Width(tTable, t.Label) + padX
	if t.View {
		head += m.Width(tBadge, "VIEW") + 8
	}
	w = math.Max(w, head)
	w = math.Max(boxMinW, math.Min(boxMaxW, w))
	// When names and types both fit, the names get all the slack (types
	// are right-aligned). When the box was capped at boxMaxW and they do
	// not, the names keep at least half: a column's name is what a reader
	// looks for, its type the detail.
	avail := w - 2*padX - badgeW - nameGap
	if nameW+typeW <= avail {
		b.nameW = avail - typeW
	} else {
		b.nameW = math.Max(avail-typeW, math.Min(nameW, avail/2))
	}
	b.w = math.Ceil(w)

	n := len(b.rows)
	if b.more > 0 {
		n++
	}
	b.h = headH + float64(n)*rowH + padB
	if n == 0 {
		b.h = headH + rowH // an empty box still looks like a table
	}
	return b
}

// newLayout lays the schema out. The title band's height is left free at
// the top; the picture draws into it.
func newLayout(s *Schema, m *raster.Faces) *layout {
	l := &layout{byT: map[*Table]*box{}, looseY: -1, paths: map[*Rel]*path{}}
	for _, t := range s.Tables {
		b := newBox(t, m)
		l.boxes = append(l.boxes, b)
		l.byT[t] = b
	}
	for _, r := range s.Rels {
		c, p := l.byT[r.Child], l.byT[r.Parent]
		c.hasRels, p.hasRels = true, true
		if c != p {
			c.nbrs = append(c.nbrs, p)
			p.nbrs = append(p.nbrs, c)
		}
	}

	// connected groups, largest first (ties by the first table's name, so
	// the order is stable); a table whose only relationship is to itself
	// is a group of one and is drawn with the groups, since it has a line
	groups := l.groups()
	sort.SliceStable(groups, func(i, j int) bool { return len(groups[i]) > len(groups[j]) })

	y := margin + titleH
	width := minPicture - 2*margin
	for _, g := range groups {
		if len(g) == 1 && !g[0].hasRels {
			l.loose = append(l.loose, g[0])
			continue
		}
		gw, gh := layGroup(g, s.Rels, l.byT, margin, y, l.paths)
		width = math.Max(width, gw)
		y += gh + groupGap
	}
	if len(l.loose) > 0 {
		if len(l.loose) < len(l.boxes) {
			// a caption only when there is something to tell them apart
			// from; a schema with no keys at all is just its tables
			l.looseY = y
			y += sectionH
		}
		lw, lh := layLoose(l.loose, margin, y, width)
		width = math.Max(width, lw)
		y += lh + groupGap
	}
	l.w = math.Ceil(width + 2*margin)
	l.h = math.Ceil(y - groupGap + margin)
	return l
}

// groups splits the boxes into connected groups (union-find over the
// relationships), each in the schema's table order.
func (l *layout) groups() [][]*box {
	parent := make(map[*box]*box, len(l.boxes))
	var find func(*box) *box
	find = func(b *box) *box {
		if parent[b] == nil || parent[b] == b {
			return b
		}
		r := find(parent[b])
		parent[b] = r // path compression
		return r
	}
	for _, b := range l.boxes {
		for _, n := range b.nbrs {
			if ra, rb := find(b), find(n); ra != rb {
				parent[ra] = rb
			}
		}
	}
	idx := map[*box]int{}
	var out [][]*box
	for _, b := range l.boxes {
		r := find(b)
		i, ok := idx[r]
		if !ok {
			i = len(out)
			idx[r] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], b)
	}
	return out
}

// layGroup places one connected group with its top-left at (x0, y0),
// routes its relationships' lines into paths, and returns its size. See the
// top of this file for the steps.
func layGroup(g []*box, rels []*Rel, byT map[*Table]*box, x0, y0 float64, paths map[*Rel]*path) (w, h float64) {
	in := make(map[*box]bool, len(g))
	for _, b := range g {
		in[b] = true
	}
	// parents and children within the group, deduplicated (two keys
	// between the same pair of tables are one ranking constraint)
	parents, children := map[*box][]*box{}, map[*box][]*box{}
	seen := map[[2]*box]bool{}
	for _, r := range rels {
		c, p := byT[r.Child], byT[r.Parent]
		if c == p || !in[c] || seen[[2]*box{c, p}] {
			continue
		}
		seen[[2]*box{c, p}] = true
		parents[c] = append(parents[c], p)
		children[p] = append(children[p], c)
	}

	// 1. Rank. A depth-first walk up the parent links marks the edges that
	// close a cycle (a → b → a: two tables referencing each other, or a
	// longer loop) as back edges, which ranking ignores — a cycle has no
	// longest path. The walk starts from each table in order, so which
	// edge of a cycle is cut is deterministic.
	const (
		white = iota
		grey
		black
	)
	state := map[*box]int{}
	back := map[[2]*box]bool{}
	var visit func(*box)
	visit = func(b *box) {
		state[b] = grey
		for _, p := range parents[b] {
			switch state[p] {
			case white:
				visit(p)
			case grey:
				back[[2]*box{b, p}] = true
			}
		}
		state[b] = black
	}
	for _, b := range g {
		if state[b] == white {
			visit(b)
		}
	}
	rank := map[*box]int{}
	done := map[*box]bool{}
	var rankOf func(*box) int
	rankOf = func(b *box) int {
		if done[b] {
			return rank[b]
		}
		r := 0
		for _, p := range parents[b] {
			if !back[[2]*box{b, p}] {
				r = max(r, rankOf(p)+1)
			}
		}
		rank[b], done[b] = r, true
		return r
	}
	for _, b := range g {
		rankOf(b)
	}
	// Pull each root (a table with no parents) right, to one rank left of
	// its nearest child. Longest-path ranking puts every root at rank 0,
	// so a lookup table referenced only from deep in the graph would
	// otherwise sit at the far left with one long line to it.
	for _, b := range g {
		if len(parents[b]) > 0 || len(children[b]) == 0 {
			continue
		}
		nearest := math.MaxInt
		for _, c := range children[b] {
			nearest = min(nearest, rank[c])
		}
		if nearest-1 > rank[b] {
			rank[b] = nearest - 1
		}
	}
	maxRank := 0
	for _, b := range g {
		b.rank = rank[b]
		maxRank = max(maxRank, b.rank)
	}
	layers := make([][]*box, maxRank+1)
	for _, b := range g { // g is in table order: the first sweep's start
		layers[b.rank] = append(layers[b.rank], b)
	}

	// 2. Order: the barycenter heuristic. A table's key is the mean
	// position (0…1 within its own rank) of its neighbours in any other
	// rank; each rank is re-sorted by it, and positions updated, over a
	// few sweeps. A table with no neighbours outside its rank keeps its
	// place. The sort is stable, so ties keep the previous order and the
	// result is deterministic.
	setPos := func(layer []*box) {
		for i, b := range layer {
			b.pos = (float64(i) + 0.5) / float64(len(layer))
		}
	}
	for _, layer := range layers {
		setPos(layer)
	}
	for range sweeps {
		for _, layer := range layers {
			key := make(map[*box]float64, len(layer))
			for _, b := range layer {
				sum, n := 0.0, 0
				for _, nb := range b.nbrs {
					if nb.rank != b.rank && in[nb] {
						sum += nb.pos
						n++
					}
				}
				if n > 0 {
					key[b] = sum / float64(n)
				} else {
					key[b] = b.pos
				}
			}
			sort.SliceStable(layer, func(i, j int) bool { return key[layer[i]] < key[layer[j]] })
			setPos(layer)
		}
	}

	// 3. Wrap tall ranks. The target is the side of a square of the
	// group's total box area (with some air), but never shorter than its
	// tallest box, so one very tall table does not force every rank to
	// split.
	area, tallest := 0.0, 0.0
	for _, b := range g {
		area += (b.w + rankGap) * (b.h + stackGap)
		tallest = math.Max(tallest, b.h)
	}
	target := math.Max(tallest, math.Sqrt(area)*0.9)
	var cols [][]*box
	for _, layer := range layers {
		var col []*box
		colH := 0.0
		for _, b := range layer {
			if len(col) > 0 && colH+stackGap+b.h > target {
				cols = append(cols, col)
				col, colH = nil, 0
			}
			if len(col) > 0 {
				colH += stackGap
			}
			col = append(col, b)
			colH += b.h
		}
		if len(col) > 0 {
			cols = append(cols, col)
		}
	}

	// 4. Place: columns left to right, each as wide as its widest box and
	// centred vertically on the tallest column. extra[i][j] is room added
	// to the gap above column i's box j, beyond stackGap, when more lines
	// cross that gap than it holds (see step 5); it starts at nothing.
	placed := make([]*column, len(cols))
	extra := make([][]float64, len(cols))
	for i, col := range cols {
		extra[i] = make([]float64, len(col))
	}
	place := func() {
		h = 0
		heights := make([]float64, len(cols))
		for i, col := range cols {
			for j, b := range col {
				if j > 0 {
					heights[i] += stackGap + extra[i][j]
				}
				heights[i] += b.h
			}
			h = math.Max(h, heights[i])
		}
		x := x0
		for i, col := range cols {
			colW := 0.0
			for _, b := range col {
				colW = math.Max(colW, b.w)
			}
			y := y0 + (h-heights[i])/2
			for j, b := range col {
				// boxes narrower than the column are centred in it, so
				// lines in both directions have about the same room
				b.x = x + (colW-b.w)/2
				b.y = y
				b.col = i
				y += b.h + stackGap
				if j+1 < len(col) {
					y += extra[i][j+1]
				}
			}
			placed[i] = &column{x: x, w: colW, boxes: col}
			x += colW
			if i < len(cols)-1 {
				x += rankGap
			}
		}
		w = x - x0
	}
	place()

	// 5. Route. First make room: a gap between two boxes holds about five
	// lines, and a column that more lines cross than its gaps hold (the
	// first wrapped column of a big hub's children) would send the rest
	// over or under the whole column, in ribbons. So the lines are routed
	// once as if every gap were wide enough (measure), each gap is widened
	// to the lanes that chose it, and the columns are placed again. Moving
	// boxes moves the rows lines attach to, so a line may then prefer
	// another gap; a few rounds settle it (widening only ever grows, so it
	// ends). A diagram whose gaps all fit (nearly every real schema) is
	// measured once and placed exactly as before.
	for range widenRounds {
		route(placed, rels, byT, in, true)
		if !widen(placed, extra) {
			break
		}
		place()
	}

	// Then route for real. A line may still leave the boxes' extent: above
	// a column's top box (when every hole nearer its ends is full, or it is
	// the shorter way), below its bottom one, or, for a loop on the last
	// column, out to the right. The group grows to hold its lines — moved
	// down by whatever rises above y0, so it never overlaps the title or
	// the group before it.
	top, bot, right := y0, y0+h, x0+w
	for r, p := range route(placed, rels, byT, in, false) {
		paths[r] = p
		for _, q := range p.pts {
			top = math.Min(top, q.Y-laneGap/2)
			bot = math.Max(bot, q.Y+laneGap/2)
			right = math.Max(right, q.X+edgeW)
		}
	}
	if dy := y0 - top; dy > 0 {
		for _, b := range g {
			b.y += dy
		}
		for r, p := range paths {
			if in[byT[r.Child]] {
				p.shift(dy)
			}
		}
	}
	return right - x0, bot - top
}

// layLoose packs the tables with no relationships into columns, masonry
// style, within width: each table (in name order) goes to the currently
// shortest column, which keeps the columns about even whatever the tables'
// heights. The column width is the widest box, so the grid lines up.
func layLoose(bs []*box, x0, y0, width float64) (w, h float64) {
	colW := 0.0
	for _, b := range bs {
		colW = math.Max(colW, b.w)
	}
	n := max(1, int((width+looseGap)/(colW+looseGap)))
	n = min(n, len(bs))
	heights := make([]float64, n)
	for _, b := range bs {
		c := 0
		for i := range heights {
			if heights[i] < heights[c] {
				c = i
			}
		}
		b.x = x0 + float64(c)*(colW+looseGap)
		b.y = y0 + heights[c]
		heights[c] += b.h + stackGap
	}
	for _, v := range heights {
		h = math.Max(h, v-stackGap)
	}
	return float64(n)*(colW+looseGap) - looseGap, h
}
