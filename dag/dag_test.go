package dag

import (
	"slices"
	"testing"
)

func TestRanks(t *testing.T) {
	cases := []struct {
		name  string
		n     int
		edges []Edge
		want  []int
	}{
		{"chain", 3, []Edge{{0, 1}, {1, 2}}, []int{0, 1, 2}},
		// copy → clean, breeds → report
		{"diamond", 4, []Edge{{0, 1}, {0, 2}, {1, 3}, {2, 3}}, []int{0, 1, 1, 2}},
		// the longest path wins: 3 waits for 2, which waits for 1
		{"longest path", 4, []Edge{{0, 1}, {1, 2}, {2, 3}, {0, 3}}, []int{0, 1, 2, 3}},
		// a root with its only downstream deep in the graph is pulled
		// right, to one rank before it
		{"root pulled", 4, []Edge{{0, 1}, {1, 2}, {3, 2}}, []int{0, 1, 2, 1}},
		// a cycle is cut at its back edge: the walk from 0 meets 1 → 0 last
		{"cycle", 2, []Edge{{0, 1}, {1, 0}}, []int{1, 0}},
		// a self loop and a duplicate are no constraint and one
		{"self and dup", 2, []Edge{{0, 0}, {0, 1}, {0, 1}}, []int{0, 1}},
		{"lone", 2, nil, []int{0, 0}},
	}
	for _, c := range cases {
		if got := Ranks(c.n, c.edges); !slices.Equal(got, c.want) {
			t.Errorf("%s: ranks %v, want %v", c.name, got, c.want)
		}
	}
}

// Order uncrosses two edges that start crossed.
func TestOrderUncrosses(t *testing.T) {
	// 0 → 3, 1 → 2: in index order the edges cross
	edges := []Edge{{0, 3}, {1, 2}}
	rank := Ranks(4, edges)
	layers := Layers(rank)
	Order(layers, rank, Neighbours(4, edges), Sweeps)
	// uncrossed: each edge joins the same position in both layers
	at := func(layer []int, v int) int { return slices.Index(layer, v) }
	if at(layers[0], 0) != at(layers[1], 3) || at(layers[0], 1) != at(layers[1], 2) {
		t.Errorf("layers %v: the edges still cross", layers)
	}
}

// Layout of a job-shaped diamond: one column per rank, the middle two
// stacked rowGap apart, the ends centred on them.
func TestLayoutDiamond(t *testing.T) {
	edges := []Edge{{0, 1}, {0, 2}, {1, 3}, {2, 3}}
	w, h := []float64{100, 100, 100, 100}, []float64{40, 40, 40, 40}
	p := Layout(4, edges, w, h, Options{X0: 10, Y0: 20, ColGap: 50, RowGap: 30})
	wantX := []float64{10, 160, 160, 310}
	wantY := []float64{20 + 35, 20, 20 + 70, 20 + 35}
	if !slices.Equal(p.X, wantX) || !slices.Equal(p.Y, wantY) {
		t.Errorf("x %v y %v, want x %v y %v", p.X, p.Y, wantX, wantY)
	}
	if p.W != 400 || p.H != 110 {
		t.Errorf("size %vx%v, want 400x110", p.W, p.H)
	}
	if !slices.Equal(p.Col, []int{0, 1, 1, 2}) {
		t.Errorf("columns %v", p.Col)
	}
}

// Wrap splits a rank taller than the target; Layout leaves it whole
// unless asked.
func TestWrap(t *testing.T) {
	var edges []Edge
	for i := 1; i <= 6; i++ {
		edges = append(edges, Edge{0, i})
	}
	w, h := make([]float64, 7), make([]float64, 7)
	for i := range w {
		w[i], h[i] = 100, 50
	}
	if p := Layout(7, edges, w, h, Options{ColGap: 50, RowGap: 10}); len(p.ColX) != 2 {
		t.Errorf("unwrapped: %d columns, want 2", len(p.ColX))
	}
	cols := Wrap(Layers(Ranks(7, edges)), h, 10, 170)
	if len(cols) != 3 || !slices.Equal(cols[1], []int{1, 2, 3}) || !slices.Equal(cols[2], []int{4, 5, 6}) {
		t.Errorf("wrapped %v: want [0] [1 2 3] [4 5 6]", cols)
	}
	if p := Layout(0, nil, nil, nil, Options{}); p.W != 0 || len(p.X) != 0 {
		t.Errorf("empty: %+v", p)
	}
}
