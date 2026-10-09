package sdbapi

import (
	"strings"
	"testing"
)

// The assistant's summary: every S method a script calls, by signature with
// its first sentence; the host-only ones left out; an alias named with its
// target; and small enough to send once per conversation.
func TestSummary(t *testing.T) {
	s := Summary()
	for _, want := range []string{
		"package sdb — ",
		"func Run(s *sdb.S) error",
		"  func (s *S) Query(conn, query string, args ...any) (*Result, error) — Query runs",
		"type CopyOpts = etl.CopyOptions — ",
		"  To string — ",
		"  func (r *Reader) Next() bool\n", // other types' methods: signature only
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q", want)
		}
	}
	for _, name := range hostOnly {
		if strings.Contains(s, ") "+name+"(") {
			t.Errorf("summary offers host-only %s", name)
		}
	}
	// ~3k tokens; a doc comment growing a paragraph should not make it 5k.
	// (12 KB until the pipeline API — a Batch's helpers and the builder's
	// calls, which a script writes against — added a tier of its own.)
	if len(s) > 14<<10 {
		t.Errorf("summary is %d bytes; keep it under 14 KB", len(s))
	}
}
