package jobs

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/dbc/pipeline"
)

// Tree is a run as indented text, the drilldown the Runs view draws:
// the run, its pipelines, each pipeline's fragments, each fragment's
// nodes — what `dbc run show ID` prints, and `dbc job run` after its run.
//
//	run 20261009-020000-7f3a · job nightly · schedule (0 2 * * *) · ✓ succeeded · 3.21s
//	  ✓ copy     copy-cats          8 rows    22ms
//	      ✓ cats      8 rows    22ms   src 8→8 · dst 8→8
//	  ✓ clean    clean-and-load     8 rows     5ms   after copy
//	      ✓ clean     7 rows     3ms   src 8→8 · tidy 8→8 · keep 8→7 · dst 7→7 · peek 7→7
//	      ✓ stamp     1 rows     0s    log 0→1
//	  ↷ report   cats-report                         after clean, breeds
//	  error: breeds: …
func (r *Run) Tree() string {
	var sb strings.Builder
	head := []string{"run " + r.ID, r.Kind + " " + r.Name}
	trig := r.Trigger
	if r.By != "" {
		trig += " (" + r.By + ")"
	}
	head = append(head, trig, glyph(r.Status)+" "+string(r.Status), took(r.Started, r.Ended))
	if r.Preview > 0 {
		head = append(head, fmt.Sprintf("preview of %d rows", r.Preview))
	}
	sb.WriteString(strings.Join(head, " · ") + "\n")
	if len(r.Params) > 0 {
		var ps []string
		for _, k := range sortedKeys(r.Params) {
			ps = append(ps, k+"="+r.Params[k])
		}
		sb.WriteString("  params: " + strings.Join(ps, " ") + "\n")
	}
	idW, nameW := 4, 8
	for _, p := range r.Pipelines {
		idW, nameW = max(idW, len(p.ID)), max(nameW, len(p.Pipeline))
	}
	for _, p := range r.Pipelines {
		line := fmt.Sprintf("  %s %-*s", glyph(p.Status), idW, p.ID)
		if r.Kind == KindJob {
			line += fmt.Sprintf(" %-*s", nameW, p.Pipeline)
		}
		if p.Status == pipeline.Queued || p.Status == pipeline.Skipped {
			line += fmt.Sprintf(" %10s %8s", "", "")
		} else {
			line += fmt.Sprintf(" %5d rows %8s", p.Rows(), took(p.Started, p.Ended))
		}
		if len(p.After) > 0 {
			line += "   after " + strings.Join(p.After, ", ")
		}
		sb.WriteString(strings.TrimRight(line, " ") + "\n")
		for _, f := range p.Fragments {
			fl := fmt.Sprintf("      %s %-10s", glyph(f.Status), f.Name)
			if f.Status != pipeline.Queued && f.Status != pipeline.Skipped {
				fl += fmt.Sprintf(" %5d rows %8s", f.Rows, took(f.Started, f.Ended))
				var nodes []string
				for _, n := range f.Nodes {
					nodes = append(nodes, fmt.Sprintf("%s %d→%d", n.ID, n.In, n.Out))
				}
				if f.Direct {
					fl += "   direct COPY"
				}
				if len(nodes) > 0 {
					fl += "   " + strings.Join(nodes, " · ")
				}
			}
			sb.WriteString(strings.TrimRight(fl, " ") + "\n")
			if f.Error != "" {
				sb.WriteString("          " + f.Error + "\n")
			}
		}
		if p.Error != "" && !fragmentSaid(p) {
			sb.WriteString("      " + p.Error + "\n")
		}
	}
	if r.Error != "" {
		sb.WriteString("  error: " + r.Error + "\n")
	}
	return sb.String()
}

// fragmentSaid reports whether a fragment's line already carries the
// pipeline's error (the runner's error is its failing fragment's).
func fragmentSaid(p PipelineRun) bool {
	for _, f := range p.Fragments {
		if f.Error != "" && strings.Contains(p.Error, f.Error) {
			return true
		}
	}
	return false
}

// took is a span as a run prints it: to the millisecond under a minute,
// to the second above; "" when it never started.
func took(from, to time.Time) string {
	if from.IsZero() {
		return ""
	}
	if to.IsZero() {
		to = time.Now()
	}
	d := to.Sub(from)
	if d >= time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Millisecond).String()
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
