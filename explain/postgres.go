package explain

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// ParsePostgresJSON reads the output of EXPLAIN (FORMAT JSON …): a one-element
// array holding {"Plan": {...}, "Planning Time": …, "Execution Time": …}.
//
// It decodes into maps rather than a struct because the key set is open:
// every Postgres version and node type adds keys ("Cache Hits" for Memoize,
// "Presorted Key" for Incremental Sort), and a struct would silently drop the
// ones it did not list. The keys the rules need are lifted into typed Node
// fields; the rest become Props in the engine's order (see pgPropOrder).
func ParsePostgresJSON(data []byte) (*Plan, error) {
	var docs []map[string]any
	if err := json.Unmarshal(data, &docs); err != nil {
		// a single object (not wrapped in an array) is what some clients
		// hand back after unwrapping psql's output; accept it too
		var one map[string]any
		if err2 := json.Unmarshal(data, &one); err2 != nil {
			return nil, serr.Wrap(err, "format", "postgres json")
		}
		docs = []map[string]any{one}
	}
	if len(docs) == 0 {
		return nil, serr.New("empty EXPLAIN output")
	}
	doc := docs[0]
	root, ok := doc["Plan"].(map[string]any)
	if !ok {
		return nil, serr.New("EXPLAIN JSON has no Plan")
	}
	p := &Plan{Engine: Postgres, Raw: string(data)}
	p.Root = pgNode(root)
	p.Analyzed = p.Root.HasActual || p.Root.NeverExecuted
	p.PlanningMs, _ = num(doc["Planning Time"])
	p.ExecutionMs, _ = num(doc["Execution Time"])
	if trig, ok := doc["Triggers"].([]any); ok {
		for _, t := range trig {
			if tm, ok := t.(map[string]any); ok {
				name, _ := tm["Trigger Name"].(string)
				ms, _ := num(tm["Time"])
				calls, _ := num(tm["Calls"])
				p.Notes = append(p.Notes, fmt.Sprintf("trigger %s: %s over %s calls", name, FmtMs(ms), FmtCount(calls)))
			}
		}
	}
	if jit, ok := doc["JIT"].(map[string]any); ok {
		if timing, ok := jit["Timing"].(map[string]any); ok {
			if total, ok := num(timing["Total"]); ok && total > 0 {
				p.Notes = append(p.Notes, "JIT compilation took "+FmtMs(total))
			}
		}
	}
	p.Finalize()
	return p, nil
}

// pgTyped are the keys lifted into typed fields (or folded into Op), so they
// are not repeated as Props.
var pgTyped = map[string]bool{
	"Node Type": true, "Plans": true, "Relation Name": true, "Alias": true, "Index Name": true,
	"Parent Relationship": true, "Subplan Name": true, "Startup Cost": true, "Total Cost": true,
	"Plan Rows": true, "Plan Width": true, "Actual Startup Time": true, "Actual Total Time": true,
	"Actual Rows": true, "Actual Loops": true, "Join Type": true, "Strategy": true,
	"Partial Mode": true, "Parallel Aware": true, "Async Capable": true, "Operation": true,
	"Scan Direction": true, "Schema": true, "Workers Planned": true, "Workers Launched": true,
	"Command": true,
}

// pgPropOrder is the order Props are listed in, most telling first; keys not
// named here follow alphabetically. JSON objects have no order of their own
// once decoded into a map, so without this the detail panel would shuffle.
var pgPropOrder = []string{
	"Hash Cond", "Merge Cond", "Join Filter", "Index Cond", "Recheck Cond", "Filter",
	"Rows Removed by Filter", "Rows Removed by Join Filter", "Rows Removed by Index Recheck",
	"Sort Key", "Presorted Key", "Sort Method", "Sort Space Used", "Sort Space Type",
	"Group Key", "Cache Key", "Cache Hits", "Cache Misses", "Cache Evictions",
	"Hash Buckets", "Hash Batches", "Original Hash Batches", "Peak Memory Usage",
	"HashAgg Batches", "Disk Usage", "Heap Fetches", "Exact Heap Blocks", "Lossy Heap Blocks",
	"Output", "Inner Unique",
}

// pgNode converts one plan object and its children.
func pgNode(m map[string]any) *Node {
	n := &Node{}
	typ, _ := m["Node Type"].(string)
	n.Op = pgOpName(typ, m)
	n.Relation, _ = m["Relation Name"].(string)
	if s, _ := m["Schema"].(string); s != "" && n.Relation != "" {
		n.Relation = s + "." + n.Relation
	}
	n.Alias, _ = m["Alias"].(string)
	if n.Alias == n.Relation || (n.Relation != "" && strings.HasSuffix(n.Relation, "."+n.Alias)) {
		n.Alias = "" // the JSON repeats the table as its own alias; the text format does not
	}
	n.Index, _ = m["Index Name"].(string)
	if typ == "CTE Scan" {
		n.Relation, _ = m["CTE Name"].(string)
	}
	n.Relationship, _ = m["Parent Relationship"].(string)
	if sub, _ := m["Subplan Name"].(string); sub != "" {
		n.Relationship = sub
	}

	n.StartupCost, n.HasCost = num(m["Startup Cost"])
	n.TotalCost, _ = num(m["Total Cost"])
	n.EstRows, n.HasEst = num(m["Plan Rows"])
	n.Width, _ = num(m["Plan Width"])
	if loops, ok := num(m["Actual Loops"]); ok {
		n.Loops = loops
		if loops == 0 {
			n.NeverExecuted = true
		} else {
			n.HasActual = true
			n.ActualStartMs, _ = num(m["Actual Startup Time"])
			n.ActualMs, _ = num(m["Actual Total Time"])
			n.ActualRows, _ = num(m["Actual Rows"])
		}
	}
	n.WorkersPlanned, _ = num(m["Workers Planned"])
	n.WorkersLaunched, _ = num(m["Workers Launched"])
	for _, k := range []string{"Rows Removed by Filter", "Rows Removed by Join Filter", "Rows Removed by Index Recheck"} {
		v, _ := num(m[k])
		n.RowsRemoved += v
	}
	if st, _ := m["Sort Space Type"].(string); st == "Disk" {
		n.SortSpill = true
		n.SpillKB, _ = num(m["Sort Space Used"])
	}
	if du, ok := num(m["Disk Usage"]); ok && du > 0 {
		n.SpillKB = math.Max(n.SpillKB, du)
	}
	n.Batches, _ = num(m["Hash Batches"])
	if b, ok := num(m["HashAgg Batches"]); ok {
		n.Batches = math.Max(n.Batches, b)
	}
	n.SharedHit, _ = num(m["Shared Hit Blocks"])
	n.SharedRead, _ = num(m["Shared Read Blocks"])
	n.TempBlocks, _ = num(m["Temp Written Blocks"])

	// Props: the rest, in a stable, telling order. Buffer counters are
	// folded into one "Buffers" line the way the text format writes them —
	// sixteen zero-valued block counters per step would bury everything else.
	var rest []string
	for k := range m {
		if !pgTyped[k] && !strings.HasSuffix(k, " Blocks") && !strings.HasSuffix(k, " I/O Read Time") &&
			!strings.HasSuffix(k, " I/O Write Time") && k != "CTE Name" && k != "Workers" {
			rest = append(rest, k)
		}
	}
	rank := func(k string) int {
		for i, o := range pgPropOrder {
			if o == k {
				return i
			}
		}
		return len(pgPropOrder)
	}
	sort.Slice(rest, func(i, j int) bool {
		ri, rj := rank(rest[i]), rank(rest[j])
		if ri != rj {
			return ri < rj
		}
		return rest[i] < rest[j]
	})
	for _, k := range rest {
		if v := pgValue(k, m[k]); v != "" {
			n.addProp(k, v)
		}
	}
	if b := pgBuffers(m); b != "" {
		n.addProp("Buffers", b)
	}
	if n.WorkersPlanned > 0 {
		n.addProp("Workers", fmt.Sprintf("%s planned, %s launched", FmtCount(n.WorkersPlanned), FmtCount(n.WorkersLaunched)))
	}

	if kids, ok := m["Plans"].([]any); ok {
		for _, k := range kids {
			if km, ok := k.(map[string]any); ok {
				n.Children = append(n.Children, pgNode(km))
			}
		}
	}
	return n
}

// pgOpName rebuilds the operation name the way EXPLAIN's text format prints
// it, which is what every Postgres user recognizes: "Hash Left Join" rather
// than a "Hash Join" with a "Join Type: Left" beside it, "HashAggregate"
// rather than "Aggregate" + "Strategy: Hashed".
func pgOpName(typ string, m map[string]any) string {
	op := typ
	switch typ {
	case "Aggregate":
		switch s, _ := m["Strategy"].(string); s {
		case "Hashed":
			op = "HashAggregate"
		case "Sorted":
			op = "GroupAggregate"
		case "Mixed":
			op = "MixedAggregate"
		}
	case "ModifyTable":
		if o, _ := m["Operation"].(string); o != "" {
			op = o
		}
	case "SetOp":
		if c, _ := m["Command"].(string); c != "" {
			op = "SetOp " + c
		}
	}
	if jt, _ := m["Join Type"].(string); jt != "" && jt != "Inner" {
		if i := strings.LastIndex(op, "Join"); i >= 0 {
			op = op[:i] + jt + " Join"
		} else {
			op += " " + jt + " Join" // "Nested Loop Left Join"
		}
	}
	if d, _ := m["Scan Direction"].(string); d == "Backward" {
		op += " Backward"
	}
	if pm, _ := m["Partial Mode"].(string); pm != "" && pm != "Simple" {
		op = pm + " " + op
	}
	if pa, _ := m["Parallel Aware"].(bool); pa {
		op = "Parallel " + op
	}
	return op
}

// pgValue renders one JSON value as the text format would.
func pgValue(key string, v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		switch {
		case strings.Contains(key, "Memory") || key == "Sort Space Used" || key == "Disk Usage":
			return FmtKB(t)
		case t == math.Trunc(t):
			return FmtCount(t)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			parts = append(parts, pgValue(key, x))
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		b, _ := json.Marshal(t)
		return string(b)
	}
	return fmt.Sprint(v)
}

// pgBuffers folds the block counters into the text format's one line:
// "shared hit=1645 read=3, temp written=120". Zero counters are left out.
func pgBuffers(m map[string]any) string {
	var groups []string
	for _, g := range []string{"Shared", "Local", "Temp"} {
		var parts []string
		for _, k := range []string{"Hit", "Read", "Dirtied", "Written"} {
			if v, ok := num(m[g+" "+k+" Blocks"]); ok && v > 0 {
				parts = append(parts, strings.ToLower(k)+"="+FmtCount(v))
			}
		}
		if len(parts) > 0 {
			groups = append(groups, strings.ToLower(g)+" "+strings.Join(parts, " "))
		}
	}
	return strings.Join(groups, ", ")
}

// num reads a JSON number (or a numeric string, which some drivers and
// MySQL's JSON format produce).
func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}
