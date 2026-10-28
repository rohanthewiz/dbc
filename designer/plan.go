package designer

import (
	"math"
	"strings"

	"github.com/rohanthewiz/dbc/explain"
)

// The engine's own view. The rules in lint.go reason from the catalog —
// keys, indexes, estimates — which is evidence, but not the plan. So the
// page also asks the engine to EXPLAIN the statement (estimates only: the
// statement is planned, never run) whenever the design settles, and paints
// the answer back onto the canvas:
//
//	plan step "Seq Scan on orders o"      ─► card o: "full scan · ~1.2M rows"
//	plan step "Index Scan using … on c"   ─► card c: "index customers_pkey"
//	insight on step 4 (a missing index)   ─► the guide, pointed at its card
//
// That is what makes the designer honest on the engines whose catalogs say
// little (SQLite keeps no row estimates; bytdb lists no indexes) and keeps
// it honest on the others: a rule may think an index serves a filter, but
// the plan shows whether the planner agrees.

// PlanView is a plan as the canvas shows it.
type PlanView struct {
	Engine  string               `json:"engine"`
	EstRows float64              `json:"estRows"`
	HasEst  bool                 `json:"hasEst"`
	Cost    float64              `json:"cost"`
	HasCost bool                 `json:"hasCost"`
	Tables  map[string]TablePlan `json:"tables"` // by canvas table ID
	// Insights are the explain package's findings on the plan, each
	// pointed at the canvas table its step reads, when one does.
	Insights []PlanInsight `json:"insights"`
}

// TablePlan is how the plan reads one canvas table.
type TablePlan struct {
	Op      string  `json:"op"`              // the step as the engine names it
	Index   string  `json:"index,omitempty"` // the index it uses, if any
	Full    bool    `json:"full"`            // it reads the whole table
	EstRows float64 `json:"estRows"`         // rows the step returns, per loop
	HasEst  bool    `json:"hasEst"`
	Loops   float64 `json:"loops,omitempty"`
}

// PlanInsight is one finding of the plan.
type PlanInsight struct {
	explain.Insight
	Table string `json:"table,omitempty"`
}

// PaintPlan maps a plan of the design's statement onto its tables. The
// statement is written with every table aliased, so a step's alias finds
// its card; a step that names only its table (Postgres drops an alias equal
// to the name; SQLite names the table) finds the one card of that table,
// when there is one.
func PaintPlan(p *explain.Plan, d Design) *PlanView {
	v := &PlanView{Engine: p.Engine, Tables: map[string]TablePlan{}, Insights: []PlanInsight{}}
	if r := p.Root; r != nil {
		v.EstRows, v.HasEst = r.EstRows, r.HasEst
		v.Cost, v.HasCost = r.TotalCost, r.HasCost
	}
	byAlias := map[string]string{}
	byName := map[string][]string{}
	for _, t := range d.Tables {
		byAlias[strings.ToLower(t.Alias)] = t.ID
		byName[strings.ToLower(t.Name)] = append(byName[strings.ToLower(t.Name)], t.ID)
	}
	tableOf := func(n *explain.Node) string {
		if n == nil {
			return ""
		}
		if id, ok := byAlias[strings.ToLower(n.Alias)]; ok && n.Alias != "" {
			return id
		}
		rel := strings.ToLower(n.Relation)
		if i := strings.LastIndexByte(rel, '.'); i >= 0 {
			rel = rel[i+1:]
		}
		if id, ok := byAlias[rel]; ok && rel != "" {
			return id // SQLite names a step by its alias when there is one
		}
		if ids := byName[rel]; len(ids) == 1 {
			return ids[0]
		}
		return ""
	}
	for _, n := range p.Nodes() {
		if n.Kind != explain.KindScan && n.Kind != explain.KindIndex {
			continue
		}
		id := tableOf(n)
		if id == "" {
			continue
		}
		tp := TablePlan{Op: n.Op, Index: n.Index, Full: n.Kind == explain.KindScan,
			EstRows: n.EstRows, HasEst: n.HasEst, Loops: n.Loops}
		// a table read twice (a self-join under one alias cannot happen;
		// a CTE or a subquery can) shows its worst read: a full scan over
		// an index lookup, the bigger over the smaller
		if old, ok := v.Tables[id]; ok && (old.Full && !tp.Full || old.Full == tp.Full && old.EstRows >= tp.EstRows) {
			continue
		}
		v.Tables[id] = tp
	}
	for _, in := range p.Insights {
		v.Insights = append(v.Insights, PlanInsight{Insight: in, Table: tableOf(p.Node(in.NodeID))})
	}
	if math.IsNaN(v.EstRows) {
		v.EstRows, v.HasEst = 0, false
	}
	return v
}
