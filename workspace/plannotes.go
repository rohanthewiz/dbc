package workspace

import (
	"fmt"
	"strings"
	"time"

	"github.com/rohanthewiz/dbc/explain"
)

// The words for a plan that landed, shared by the TUI and dbc web so a
// tuning session's log reads the same in either.

// PlanNotes is what the log says when a plan lands: how long the explain
// took and the plan's headline, how it compares with prev (the last plan of
// the same statement, or nil), and each finding worth acting on.
func PlanNotes(p *explain.Plan, prev *explain.Plan, elapsed time.Duration) []Note {
	notes := []Note{notef(Ok, "explained in %s — %s", explain.FmtMs(float64(elapsed.Microseconds())/1000),
		strings.TrimPrefix(p.Headline(), "Plan · "))}
	if explain.SameSubject(prev, p) {
		if cmp, _ := explain.Compare(prev, p); cmp != "" {
			notes = append(notes, notef(Accent, "vs the last plan of this statement: %s", cmp))
		}
	}
	for _, in := range p.Insights {
		switch in.Severity {
		case explain.SevCrit:
			notes = append(notes, Note{Err, in.Severity.Glyph() + " " + in.Title})
		case explain.SevWarn:
			notes = append(notes, Note{Warn, in.Severity.Glyph() + " " + in.Title})
		}
	}
	return notes
}

// PlanStatus is the status-bar summary of a plan: "plan · 12 steps, cost
// 431 · 2 findings".
func PlanStatus(p *explain.Plan) string {
	s := strings.TrimPrefix(p.Headline(), "Plan · ")
	if crit, warn := p.Findings(); crit+warn > 0 {
		n := crit + warn
		word := "findings"
		if n == 1 {
			word = "finding"
		}
		s += fmt.Sprintf(" · %d %s", n, word)
	}
	return "plan · " + s
}
