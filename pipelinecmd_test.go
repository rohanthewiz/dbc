package main

import (
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/pipeline"
)

// runText shows no time for a skipped fragment (N-204): one --fragment left
// out (never started), and an action a preview skipped (started, at once
// skipped). A run fragment keeps its time.
func TestRunTextSkippedHasNoTime(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	st := &pipeline.RunStats{Fragments: []pipeline.FragmentStats{
		{Name: "left", Status: pipeline.Skipped},
		{Name: "act", Status: pipeline.Skipped, Started: t0, Ended: t0.Add(time.Microsecond)},
		{Name: "load", Status: pipeline.Succeeded, Rows: 4, Started: t0, Ended: t0.Add(1500 * time.Millisecond)},
	}}
	lines := strings.Split(strings.TrimRight(runText(st), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("runText:\n%s", runText(st))
	}
	for _, l := range lines[:2] {
		if f := strings.Fields(l); f[len(f)-1] != "-" {
			t.Errorf("a skipped fragment's line %q, want its time shown as -", l)
		}
	}
	if !strings.Contains(lines[2], "1.5s") {
		t.Errorf("the run fragment's line %q, want 1.5s", lines[2])
	}
}
