package jobs

import (
	"math"

	"github.com/rohanthewiz/dbc/dag"
)

// Where the jobs tab of dbc web draws a job's steps: one card per step,
// left to right by how far downstream it is (package dag), with an edge
// from each step to every step that waits for it.
//
//	 ┌ copy ───┐         ┌ clean ──┐         ┌ report ─┐
//	 │ ⛓ copy- │────┬───►│ ⛓ clean-│───┬────►│ ⛓ cats- │
//	 └─────────┘    │    └─────────┘   │     └─────────┘
//	                │    ┌ breeds ─┐   │
//	                └───►│ ⛓ breed-│───┘
//	                     └─────────┘
//
// WHY THE SERVER LAYS IT OUT. The ERD already has this algorithm in Go;
// the page asks for positions and draws cards and Bezier edges there, so
// the ranking and ordering exist once. The card's size is the server's too
// (Layout.Card), so the page never measures what the server placed.
//
// Unlike the ERD, a rank is never wrapped into several columns: in a DAG
// of steps, a column to the right reads as "runs after", and a wrapped
// rank would claim an order that is not there.

// The cards' size and the gaps between them, in CSS pixels.
const (
	cardW      = 196.0
	cardH      = 74.0
	stepColGap = 70.0 // between ranks: room for the edges' curves
	stepRowGap = 22.0 // between two steps of one rank
	layoutPad  = 20.0 // around the picture
)

// Layout is the positions of a job's step cards: each step's top left by
// its id, the picture's size, and the size every card is drawn at.
type Layout struct {
	Nodes map[string][2]float64 `json:"nodes"`
	W     float64               `json:"w"`
	H     float64               `json:"h"`
	Card  [2]float64            `json:"card"`
}

// LayoutSteps lays out steps given by their ids and, for each, the ids it
// waits for. An after naming no step, or the step itself, is ignored, and a
// step named twice keeps its first place — a half-built job in an editor
// still draws.
func LayoutSteps(ids []string, after [][]string) Layout {
	idx := make(map[string]int, len(ids))
	for i, id := range ids {
		if _, dup := idx[id]; !dup {
			idx[id] = i
		}
	}
	var edges []dag.Edge
	for i, as := range after {
		for _, a := range as {
			if j, ok := idx[a]; ok && j != i {
				edges = append(edges, dag.Edge{From: j, To: i})
			}
		}
	}
	w, h := make([]float64, len(ids)), make([]float64, len(ids))
	for i := range ids {
		w[i], h[i] = cardW, cardH
	}
	p := dag.Layout(len(ids), edges, w, h, dag.Options{X0: layoutPad, Y0: layoutPad, ColGap: stepColGap, RowGap: stepRowGap})
	out := Layout{Nodes: make(map[string][2]float64, len(ids)), Card: [2]float64{cardW, cardH}}
	for i, id := range ids {
		if idx[id] == i {
			out.Nodes[id] = [2]float64{math.Round(p.X[i]), math.Round(p.Y[i])}
		}
	}
	if len(ids) > 0 {
		out.W, out.H = math.Ceil(p.W+2*layoutPad), math.Ceil(p.H+2*layoutPad)
	}
	return out
}

// Layout is the job's steps laid out as the jobs tab draws them.
func (s *Spec) Layout() Layout {
	ids, after := make([]string, len(s.Pipelines)), make([][]string, len(s.Pipelines))
	for i, st := range s.Pipelines {
		ids[i], after[i] = st.ID, st.After
	}
	return LayoutSteps(ids, after)
}

// Layout is a job run's steps laid out as the run page draws them — from
// the record, so a run is drawn as it ran even after its job file changed.
func (r *Run) Layout() Layout {
	ids, after := make([]string, len(r.Pipelines)), make([][]string, len(r.Pipelines))
	for i, p := range r.Pipelines {
		ids[i], after[i] = p.ID, p.After
	}
	return LayoutSteps(ids, after)
}
