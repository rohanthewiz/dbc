package explain

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// ParsePostgresText reads EXPLAIN's text format — what psql prints, and what
// bytdb emits (it speaks the same format, without costs). engine names which
// of the two it came from.
//
// The format's structure is its indentation. A step's own detail lines sit
// two columns right of where its name starts, and so do the arrows of its
// children; a subplan is announced by a label line at the detail column just
// before its arrow:
//
//	col 0   Hash Join  (cost=…)                  ◄ root; its text starts at 0
//	col 2     Hash Cond: (o.user_id = u.id)      ◄ detail of the node at text col 0
//	col 2     ->  Seq Scan on orders o (…)       ◄ child; its text starts at col 6
//	col 8           Filter: (…)                  ◄ detail of the node at text col 6
//	col 2     SubPlan 1                          ◄ label for the next child
//	col 2     ->  Aggregate (…)
//	col 0   Planning Time: 0.4 ms                ◄ summary, once back at col 0
//
// So each line is attached by column alone: a detail or label line at column
// i belongs to the most recent step whose text starts left of i, and an arrow
// at column c is a child of the most recent step whose text starts left of c.
// A stack of open steps, popped past anything at or right of the column,
// finds both in one pass.
func ParsePostgresText(lines []string, engine string) (*Plan, error) {
	type open struct {
		n    *Node
		text int // the column its text starts at
	}
	var stack []open
	p := &Plan{Engine: engine, Raw: strings.Join(lines, "\n")}
	pendingRel := "" // a "SubPlan 1" / "CTE x" label waiting for its child
	summary := false // past the plan, into Planning Time and friends

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		ind := len(line) - len(strings.TrimLeft(line, " "))
		body := strings.TrimSpace(line)

		// the root: the first line of all
		if p.Root == nil {
			n := parsePGHead(body)
			p.Root = n
			stack = append(stack, open{n, ind})
			continue
		}
		// back at column 0 after the root: the summary lines
		if ind == 0 {
			summary = true
		}
		if summary {
			k, v, _ := strings.Cut(body, ":")
			switch strings.TrimSpace(k) {
			case "Planning Time", "Planning time":
				p.PlanningMs = leadingFloat(v)
			case "Execution Time", "Execution time", "Total runtime":
				p.ExecutionMs = leadingFloat(v)
			default:
				if strings.HasPrefix(body, "Trigger ") {
					p.Notes = append(p.Notes, body)
				}
			}
			continue
		}

		if strings.HasPrefix(body, "->") {
			text := strings.TrimSpace(strings.TrimPrefix(body, "->"))
			col := ind // the arrow's column
			for len(stack) > 1 && stack[len(stack)-1].text >= col {
				stack = stack[:len(stack)-1]
			}
			parent := stack[len(stack)-1].n
			n := parsePGHead(text)
			n.Relationship, pendingRel = pendingRel, ""
			parent.Children = append(parent.Children, n)
			stack = append(stack, open{n, len(line) - len(text)})
			continue
		}

		for len(stack) > 1 && stack[len(stack)-1].text >= ind {
			stack = stack[:len(stack)-1]
		}
		owner := stack[len(stack)-1].n
		if isSubplanLabel(body) {
			pendingRel = body
			continue
		}
		applyPGDetail(owner, body)
	}
	if p.Root == nil {
		return nil, serr.New("empty EXPLAIN output")
	}
	p.Analyzed = anyNode(p.Root, func(n *Node) bool { return n.HasActual || n.NeverExecuted })
	p.Finalize()
	return p, nil
}

// isSubplanLabel recognizes the lines that name the child after them.
func isSubplanLabel(s string) bool {
	for _, pre := range []string{"SubPlan ", "InitPlan ", "CTE "} {
		if strings.HasPrefix(s, pre) && !strings.Contains(s, ": ") {
			return true
		}
	}
	return false
}

var (
	pgCostRe   = regexp.MustCompile(`\(cost=([\d.]+)\.\.([\d.]+) rows=(\d+) width=(\d+)\)`)
	pgActualRe = regexp.MustCompile(`\(actual time=([\d.]+)\.\.([\d.]+) rows=([\d.]+) loops=(\d+)\)`)
	// PG 18 dropped "time" when TIMING OFF: (actual rows=5 loops=1)
	pgActualNoTimeRe = regexp.MustCompile(`\(actual rows=([\d.]+) loops=(\d+)\)`)
)

// parsePGHead parses a step line: its name and target, then its estimate and
// actual groups.
func parsePGHead(text string) *Node {
	n := &Node{}
	head := text
	if i := strings.Index(head, "  ("); i >= 0 {
		head = head[:i]
	} else if i := strings.Index(head, " (actual"); i >= 0 {
		head = head[:i]
	} else if i := strings.Index(head, " (never executed)"); i >= 0 {
		head = head[:i]
	}
	head = strings.TrimSpace(head)
	if m := pgCostRe.FindStringSubmatch(text); m != nil {
		n.HasCost, n.HasEst = true, true
		n.StartupCost, _ = strconv.ParseFloat(m[1], 64)
		n.TotalCost, _ = strconv.ParseFloat(m[2], 64)
		n.EstRows, _ = strconv.ParseFloat(m[3], 64)
		n.Width, _ = strconv.ParseFloat(m[4], 64)
	}
	switch {
	case strings.Contains(text, "(never executed)"):
		n.NeverExecuted = true
	default:
		if m := pgActualRe.FindStringSubmatch(text); m != nil {
			n.HasActual = true
			n.ActualStartMs, _ = strconv.ParseFloat(m[1], 64)
			n.ActualMs, _ = strconv.ParseFloat(m[2], 64)
			n.ActualRows, _ = strconv.ParseFloat(m[3], 64)
			n.Loops, _ = strconv.ParseFloat(m[4], 64)
		} else if m := pgActualNoTimeRe.FindStringSubmatch(text); m != nil {
			n.HasActual = true
			n.ActualRows, _ = strconv.ParseFloat(m[1], 64)
			n.Loops, _ = strconv.ParseFloat(m[2], 64)
		}
	}
	splitPGTarget(n, head)
	return n
}

// splitPGTarget separates "Index Scan using users_pkey on users u" into the
// operation and what it works on. Two words carry the grammar: "using" names
// the index, "on" the table (or, for a Bitmap Index Scan, which scans only an
// index, the index).
func splitPGTarget(n *Node, head string) {
	op, rest := head, ""
	if i := strings.Index(head, " using "); i >= 0 {
		op, rest = head[:i], head[i+len(" using "):]
		idx, rel, _ := strings.Cut(rest, " on ")
		n.Index = strings.TrimSpace(idx)
		setRelAlias(n, rel)
	} else if i := strings.Index(head, " on "); i >= 0 {
		op, rest = head[:i], head[i+len(" on "):]
		if strings.HasPrefix(op, "Bitmap Index Scan") {
			n.Index = strings.TrimSpace(rest)
		} else {
			setRelAlias(n, rest)
		}
	}
	n.Op = strings.TrimSpace(op)
}

// setRelAlias reads "orders o" / "public.orders" / "\"*VALUES*\"".
func setRelAlias(n *Node, s string) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) > 0 {
		n.Relation = f[0]
	}
	if len(f) > 1 {
		n.Alias = f[1]
	}
}

var (
	pgBatchesRe = regexp.MustCompile(`Batches: (\d+)`)
	pgDiskRe    = regexp.MustCompile(`Disk(?: Usage)?: (\d+)kB`)
	pgBufRe     = regexp.MustCompile(`(shared|local|temp)((?: (?:hit|read|dirtied|written)=\d+)+)`)
	pgBufKVRe   = regexp.MustCompile(`(hit|read|dirtied|written)=(\d+)`)
)

// applyPGDetail records one "Key: value" line on its step, lifting the ones
// the rules read into typed fields.
func applyPGDetail(n *Node, body string) {
	k, v, ok := strings.Cut(body, ": ")
	if !ok {
		k, v = body, ""
	}
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)
	switch {
	case strings.HasPrefix(k, "Rows Removed by"):
		n.RowsRemoved += leadingFloat(v)
	case k == "Workers Planned":
		n.WorkersPlanned = leadingFloat(v)
	case k == "Workers Launched":
		n.WorkersLaunched = leadingFloat(v)
	case k == "Sort Method" || k == "Buckets" || strings.HasPrefix(k, "Worker ") || k == "Batches":
		if strings.Contains(v, "external") || strings.Contains(body, "Disk:") {
			n.SortSpill = true
		}
	case k == "Buffers":
		for _, g := range pgBufRe.FindAllStringSubmatch(v, -1) {
			for _, kv := range pgBufKVRe.FindAllStringSubmatch(g[2], -1) {
				f, _ := strconv.ParseFloat(kv[2], 64)
				switch g[1] + " " + kv[1] {
				case "shared hit":
					n.SharedHit = f
				case "shared read":
					n.SharedRead = f
				case "temp written":
					n.TempBlocks = f
				}
			}
		}
	}
	if m := pgBatchesRe.FindStringSubmatch(body); m != nil {
		b, _ := strconv.ParseFloat(m[1], 64)
		n.Batches = max(n.Batches, b)
	}
	if m := pgDiskRe.FindStringSubmatch(body); m != nil {
		kb, _ := strconv.ParseFloat(m[1], 64)
		n.SpillKB = max(n.SpillKB, kb)
		if kb > 0 && k != "Sort Method" && !strings.HasPrefix(k, "Worker ") {
			n.Batches = max(n.Batches, 2) // a hash aggregate that used disk spilled
		}
	}
	n.addProp(k, v)
}

// leadingFloat reads the number at the start of s ("0.383 ms" → 0.383).
func leadingFloat(s string) float64 {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && (s[end] == '.' || s[end] == '-' || s[end] == 'e' || s[end] == '+' || (s[end] >= '0' && s[end] <= '9')) {
		end++
	}
	f, _ := strconv.ParseFloat(s[:end], 64)
	return f
}
