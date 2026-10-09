package erd

import (
	"math"
	"sort"

	"github.com/rohanthewiz/dbc/dag"
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
// The steps, per connected group of tables — 1 to 4 are package dag's,
// which the jobs tab of dbc web lays its pipelines out with too; erd keeps
// the boxes, the groups, and 5:
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
	sweeps     = dag.Sweeps
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
	rank    int    // its rank within its group (dag.Ranks)
	col     int    // its column's index within its group, left to right
	nbrs    []*box // tables it shares a key with (not itself)
	hasRels bool   // any relationship, a self-reference included
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
	idx := make(map[*box]int, len(g))
	for i, b := range g {
		in[b] = true
		idx[b] = i
	}
	// The group as package dag sees it: node i is g[i] (the schema's table
	// order, which is where every sweep starts, so the result is stable),
	// and an edge per relationship, parent → child, in the schema's
	// relationship order. dag ranks two keys between one pair of tables as
	// one constraint and skips a self-reference, but lets both keys pull
	// in the ordering — as the tables' nbrs do.
	var edges []dag.Edge
	for _, r := range rels {
		c, p := byT[r.Child], byT[r.Parent]
		if !in[c] {
			continue
		}
		edges = append(edges, dag.Edge{From: idx[p], To: idx[c]})
	}
	ws, hs := make([]float64, len(g)), make([]float64, len(g))
	for i, b := range g {
		ws[i], hs[i] = b.w, b.h
	}

	// 1. Rank: parents left; a cycle of keys cut at its back edge; a root
	// pulled right to just left of its nearest child (dag.Ranks).
	rank := dag.Ranks(len(g), edges)
	for i, b := range g {
		b.rank = rank[i]
	}

	// 2. Order: the barycenter heuristic over sweeps passes (dag.Order).
	layers := dag.Layers(rank)
	dag.Order(layers, rank, dag.Neighbours(len(g), edges), sweeps)

	// 3. Wrap tall ranks into several columns: the target is about a
	// square of the group's box area, never shorter than its tallest box
	// (dag.WrapTarget), so one very tall table does not force every rank
	// to split.
	cols := dag.Wrap(layers, hs, stackGap, dag.WrapTarget(ws, hs, rankGap, stackGap))

	// 4. Place: columns left to right, each as wide as its widest box and
	// centred vertically on the tallest column (dag.Place). extra[i][j] is
	// room added to the gap above column i's box j, beyond stackGap, when
	// more lines cross that gap than it holds (see step 5); it starts at
	// nothing. The columns' box lists are made once: routing keeps them.
	placed := make([]*column, len(cols))
	extra := make([][]float64, len(cols))
	colBoxes := make([][]*box, len(cols))
	for i, col := range cols {
		extra[i] = make([]float64, len(col))
		for _, v := range col {
			colBoxes[i] = append(colBoxes[i], g[v])
		}
	}
	place := func() {
		p := dag.Place(cols, ws, hs, x0, y0, rankGap, stackGap, extra)
		for i, b := range g {
			b.x, b.y, b.col = p.X[i], p.Y[i], p.Col[i]
		}
		for i := range cols {
			placed[i] = &column{x: p.ColX[i], w: p.ColW[i], boxes: colBoxes[i]}
		}
		w, h = p.W, p.H
	}
	place()

	// 5. Route. First make room: a gap between two boxes holds about five
	// lanes, and a column that more lanes cross than its gaps hold would
	// send the rest over or under the whole column, in ribbons. (A big
	// hub's lines ride buses, a lane per trunk rather than per line — see
	// route.go — so it is many distinct keys crossing one column that
	// crowds it, not one hub's.) So the lines are routed
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
