package explain

import (
	"fmt"
	"math"
	"strings"
)

// Before and after. Tuning is explain, change something (an index, a
// rewrite), explain again — so a UI keeps the plan it is replacing when the
// new one is of the same statement, and says how the two compare. The TUI's
// Plan tab and dbc web's both use these, so "was 41 ms → 3 ms ▼93%" reads
// the same in either.

// SameSubject reports whether two plans explain the same statement on the
// same connection, give or take whitespace and case — the condition for
// showing one as the "before" of the other. A plan detected in a result (no
// statement) has no subject, so it compares with nothing.
func SameSubject(a, b *Plan) bool {
	if a == nil || b == nil {
		return false
	}
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	return a.Conn == b.Conn && a.Engine == b.Engine && norm(a.Statement) == norm(b.Statement) && a.Statement != ""
}

// Compare describes after against before by the best figure both have:
// measured time, else the planner's total cost. good says whether it went
// the right way (down, or within 1%). "" when there is nothing to compare —
// no before, or no figure the two share.
func Compare(before, after *Plan) (text string, good bool) {
	if before == nil || after == nil || before.Root == nil || after.Root == nil {
		return "", false
	}
	var b, a float64
	var f func(float64) string
	switch {
	case before.ExecutionMs > 0 && after.ExecutionMs > 0:
		b, a, f = before.ExecutionMs, after.ExecutionMs, FmtMs
	case before.Root.HasCost && after.Root.HasCost:
		b, a, f = before.Root.TotalCost, after.Root.TotalCost, func(c float64) string { return "cost " + FmtCost(c) }
	default:
		return "", false
	}
	if b <= 0 {
		return "", false
	}
	change := (a - b) / b * 100
	arrow := "▼"
	if change > 0 {
		arrow = "▲"
	}
	if math.Abs(change) < 1 {
		return fmt.Sprintf("same as before: %s", f(a)), true
	}
	return fmt.Sprintf("was %s → %s %s%.0f%%", f(b), f(a), arrow, math.Abs(change)), change < 0
}

// Findings counts the plan's critical and warning insights — the ones
// worth acting on; info-level notes are not counted.
func (p *Plan) Findings() (crit, warn int) {
	for _, in := range p.Insights {
		switch in.Severity {
		case SevCrit:
			crit++
		case SevWarn:
			warn++
		}
	}
	return crit, warn
}
