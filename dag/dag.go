// Package dag lays out a directed graph in layers, left to right: each node
// in a column by its rank (how far downstream it is), the nodes of a column
// ordered so that few edges cross, and the columns placed side by side.
//
//	 rank 0        rank 1         rank 2
//	┌──────┐      ┌───────┐
//	│ copy │──┬──►│ clean │──┐     ┌────────┐
//	└──────┘  │   └───────┘  ├────►│ report │
//	          │   ┌────────┐ │     └────────┘
//	          └──►│ breeds │─┘
//	              └────────┘
//
// It is the deterministic core of a Sugiyama layout — longest-path ranking,
// barycenter ordering, a few sweeps — and nothing more: no dummy nodes, no
// crossing minimisation beyond the sweeps, no coordinate assignment beyond
// centring each column. That draws the graphs dbc has (a schema's foreign
// keys, a job's pipelines: trees and shallow DAGs around a few hubs) well,
// and it draws the same graph the same way every time, which matters more
// to a reader who navigates a picture by where things are than a prettier
// layout that shifts when one edge is added.
//
// Two callers share it. erd (the entity-relationship diagram) lays out each
// connected group of tables with it, wrapping tall ranks and widening gaps
// for its routed lines (its own steps around these); the jobs tab of dbc
// web gets its job's pipeline cards placed by Layout, through the server, so
// the browser draws what the server measured rather than running a second
// copy of the algorithm in JavaScript.
//
// The steps are separate functions, so a caller can put its own between
// them (erd widens gaps between Place calls):
//
//	Ranks       a node's rank: one more than its deepest upstream's; a cycle
//	            is cut at its back edge first; a root is pulled right to sit
//	            just left of its nearest downstream
//	Layers      the nodes of each rank, in node order
//	Order       each layer sorted by its nodes' neighbours' mean position,
//	            over a few sweeps (the barycenter heuristic)
//	Wrap        a layer taller than a target split into several columns
//	Place       the columns left to right, each centred vertically
//
// Nodes are 0…n-1; a caller keeps its own objects in a slice and passes
// their indexes. Everything is in the caller's units (erd's and the jobs
// tab's are CSS pixels).
package dag

import (
	"math"
	"sort"
)

// Edge runs from an upstream node to a downstream one: a foreign key's
// parent table to its child, a job step to the step that waits for it. The
// downstream end is ranked right of the upstream one.
type Edge struct{ From, To int }

// Sweeps is how many barycenter passes Order makes by default. Real graphs
// converge long before this; more only costs time.
const Sweeps = 8

// Ranks gives each node its rank (its column, before any wrap).
//
// A depth-first walk up the upstream links marks the edges that close a
// cycle (a → b → a: two tables referencing each other, or a longer loop) as
// back edges, which ranking ignores — a cycle has no longest path. The walk
// starts from each node in index order, so which edge of a cycle is cut is
// deterministic. Duplicate edges (two foreign keys between one pair of
// tables) and self loops are one constraint and none.
//
// Then each root (a node with no upstream) is pulled right, to one rank left
// of its nearest downstream. Longest-path ranking puts every root at rank 0,
// so a lookup table referenced only from deep in a schema would otherwise sit
// at the far left with one long line to it.
func Ranks(n int, edges []Edge) []int {
	ups, downs := adjacency(n, edges)

	const (
		white = iota // not reached yet
		grey         // on the walk's current path
		black        // done
	)
	state := make([]int, n)
	back := map[Edge]bool{}
	var visit func(int)
	visit = func(v int) {
		state[v] = grey
		for _, u := range ups[v] {
			switch state[u] {
			case white:
				visit(u)
			case grey:
				back[Edge{From: u, To: v}] = true // u is upstream of v and on the path: a cycle closes here
			}
		}
		state[v] = black
	}
	for v := range n {
		if state[v] == white {
			visit(v)
		}
	}

	rank := make([]int, n)
	done := make([]bool, n)
	var rankOf func(int) int
	rankOf = func(v int) int {
		if done[v] {
			return rank[v]
		}
		r := 0
		for _, u := range ups[v] {
			if !back[Edge{From: u, To: v}] {
				r = max(r, rankOf(u)+1)
			}
		}
		rank[v], done[v] = r, true
		return r
	}
	for v := range n {
		rankOf(v)
	}

	// Pulling a root moves only that root: its downstream nodes have an
	// upstream (it), so none of them is a root, and their ranks stay put.
	for v := range n {
		if len(ups[v]) > 0 || len(downs[v]) == 0 {
			continue
		}
		nearest := math.MaxInt
		for _, d := range downs[v] {
			nearest = min(nearest, rank[d])
		}
		if nearest-1 > rank[v] {
			rank[v] = nearest - 1
		}
	}
	return rank
}

// adjacency lists each node's upstream and downstream nodes, in edge order,
// without self loops and with each pair once.
func adjacency(n int, edges []Edge) (ups, downs [][]int) {
	ups, downs = make([][]int, n), make([][]int, n)
	seen := map[Edge]bool{}
	for _, e := range edges {
		if e.From == e.To || seen[e] {
			continue
		}
		seen[e] = true
		ups[e.To] = append(ups[e.To], e.From)
		downs[e.From] = append(downs[e.From], e.To)
	}
	return ups, downs
}

// Layers is the nodes of each rank, in index order: the order the first
// ordering sweep starts from.
func Layers(rank []int) [][]int {
	if len(rank) == 0 {
		return nil
	}
	top := 0
	for _, r := range rank {
		top = max(top, r)
	}
	layers := make([][]int, top+1)
	for v, r := range rank {
		layers[r] = append(layers[r], v)
	}
	return layers
}

// Neighbours is, per node, the other end of each of its edges, in edge
// order — once per edge, so a pair joined twice pulls twice as hard in
// Order. Self loops are left out (a node is not its own neighbour).
func Neighbours(n int, edges []Edge) [][]int {
	nbrs := make([][]int, n)
	for _, e := range edges {
		if e.From == e.To {
			continue
		}
		nbrs[e.To] = append(nbrs[e.To], e.From)
		nbrs[e.From] = append(nbrs[e.From], e.To)
	}
	return nbrs
}

// Order sorts each layer in place by the barycenter heuristic: a node's key
// is the mean position (0…1 within its own layer) of its neighbours in any
// other layer, and each layer is re-sorted by it — positions updated at
// once, so the next layer in the same sweep already sees the move — over
// sweeps passes. A node with no neighbour outside its layer keeps its
// place. The sort is stable, so ties keep the previous order and the result
// is deterministic. It removes most crossings; it does not promise none.
func Order(layers [][]int, rank []int, nbrs [][]int, sweeps int) {
	pos := make([]float64, len(rank))
	setPos := func(layer []int) {
		for i, v := range layer {
			pos[v] = (float64(i) + 0.5) / float64(len(layer))
		}
	}
	for _, layer := range layers {
		setPos(layer)
	}
	key := make([]float64, len(rank))
	for range sweeps {
		for _, layer := range layers {
			for _, v := range layer {
				sum, cnt := 0.0, 0
				for _, nb := range nbrs[v] {
					if rank[nb] != rank[v] {
						sum += pos[nb]
						cnt++
					}
				}
				if cnt > 0 {
					key[v] = sum / float64(cnt)
				} else {
					key[v] = pos[v]
				}
			}
			sort.SliceStable(layer, func(i, j int) bool { return key[layer[i]] < key[layer[j]] })
			setPos(layer)
		}
	}
}

// WrapTarget is the height a wrapped column aims at: the side of a square
// of the nodes' total area (each with its gaps, with some air), but never
// shorter than the tallest node, so one very tall node does not force every
// layer to split.
func WrapTarget(w, h []float64, colGap, rowGap float64) float64 {
	area, tallest := 0.0, 0.0
	for i := range w {
		area += (w[i] + colGap) * (h[i] + rowGap)
		tallest = math.Max(tallest, h[i])
	}
	return math.Max(tallest, math.Sqrt(area)*0.9)
}

// Wrap splits each layer taller than target (its nodes' heights with rowGap
// between) into several columns, in order, so a hub with forty children is
// a block rather than a strip. A layer that fits is one column.
//
// Wrapping suits a schema, where a column's neighbour is only "further
// from the roots"; a job's DAG is not wrapped (Layout's Options.Wrap off),
// since two columns of one rank would read as one waiting for the other.
func Wrap(layers [][]int, h []float64, rowGap, target float64) [][]int {
	var cols [][]int
	for _, layer := range layers {
		var col []int
		colH := 0.0
		for _, v := range layer {
			if len(col) > 0 && colH+rowGap+h[v] > target {
				cols = append(cols, col)
				col, colH = nil, 0
			}
			if len(col) > 0 {
				colH += rowGap
			}
			col = append(col, v)
			colH += h[v]
		}
		if len(col) > 0 {
			cols = append(cols, col)
		}
	}
	return cols
}

// Placement is where Place put everything. X and Y are each node's top
// left; Col its column's index. ColX and ColW are each column's left edge
// and width (its widest node). W and H are the whole picture's extent from
// (x0, y0).
type Placement struct {
	X, Y       []float64
	Col        []int
	ColX, ColW []float64
	W, H       float64
}

// Place puts the columns left to right from (x0, y0), colGap apart, each as
// wide as its widest node and centred vertically on the tallest column; a
// node narrower than its column is centred in it, so edges in both
// directions have about the same room. Nodes in a column are rowGap apart,
// plus extra[i][j] above column i's node j when extra is given (erd widens
// the gaps its lines crowd; nil is no extra anywhere).
func Place(cols [][]int, w, h []float64, x0, y0, colGap, rowGap float64, extra [][]float64) Placement {
	p := Placement{X: make([]float64, len(w)), Y: make([]float64, len(w)), Col: make([]int, len(w)),
		ColX: make([]float64, len(cols)), ColW: make([]float64, len(cols))}
	gapAbove := func(i, j int) float64 {
		if extra == nil {
			return 0
		}
		return extra[i][j]
	}
	heights := make([]float64, len(cols))
	for i, col := range cols {
		for j, v := range col {
			if j > 0 {
				heights[i] += rowGap + gapAbove(i, j)
			}
			heights[i] += h[v]
		}
		p.H = math.Max(p.H, heights[i])
	}
	x := x0
	for i, col := range cols {
		colW := 0.0
		for _, v := range col {
			colW = math.Max(colW, w[v])
		}
		y := y0 + (p.H-heights[i])/2
		for j, v := range col {
			p.X[v] = x + (colW-w[v])/2
			p.Y[v] = y
			p.Col[v] = i
			y += h[v] + rowGap
			if j+1 < len(col) {
				y += gapAbove(i, j+1)
			}
		}
		p.ColX[i], p.ColW[i] = x, colW
		x += colW
		if i < len(cols)-1 {
			x += colGap
		}
	}
	p.W = x - x0
	return p
}

// Options shape Layout.
type Options struct {
	X0, Y0 float64 // the top left of the picture
	ColGap float64 // between two columns: room for the edges
	RowGap float64 // between two nodes in a column
	Sweeps int     // barycenter passes; 0 means Sweeps
	Wrap   bool    // split layers taller than WrapTarget into columns
}

// Layout runs every step: the whole graph as one picture, each node of the
// sizes given (w[i] × h[i]). Disconnected parts are laid out together, a
// node with no edge at rank 0; erd, which wants its groups apart, calls the
// steps per group instead.
func Layout(n int, edges []Edge, w, h []float64, opt Options) Placement {
	if n == 0 {
		return Placement{}
	}
	sweeps := opt.Sweeps
	if sweeps == 0 {
		sweeps = Sweeps
	}
	rank := Ranks(n, edges)
	layers := Layers(rank)
	Order(layers, rank, Neighbours(n, edges), sweeps)
	cols := layers
	if opt.Wrap {
		cols = Wrap(layers, h, opt.RowGap, WrapTarget(w, h, opt.ColGap, opt.RowGap))
	}
	return Place(cols, w, h, opt.X0, opt.Y0, opt.ColGap, opt.RowGap, nil)
}
