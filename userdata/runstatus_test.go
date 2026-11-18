package userdata

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// fakeRecord is a record shaped as jobs writes one: the head's keys, then
// pipelines whose fragments carry a long log.
func fakeRecord(id, status string, logLines int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `{
  "id": %q,
  "kind": "job",
  "name": "nightly",
  "trigger": "cron",
  "by": "0 2 * * *",
  "params": {"day": "2026-11-10"},
  "started": "2026-11-10T02:00:00Z",
  "ended": "0001-01-01T00:00:00Z",
  "status": %q,
  "pipelines": [{"id": "p1", "fragments": [{"rows": 7, "log": [`, id, status)
	for i := range logLines {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n%q", fmt.Sprintf("02:00:%02d copied 1000 rows from orders into the warehouse, batch %d", i%60, i))
	}
	b.WriteString("]}]}]}\n")
	return []byte(b.String())
}

// RunStatuses reads each record's status from its front, agreeing with the
// full head ListRuns parses; RunIndex finds every record.
func TestRunStatuses(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{}
	for i, st := range []string{"succeeded", "running", "failed", "queued"} {
		id := fmt.Sprintf("20261110-0200%02d-ab%02d", i, i)
		if err := SaveRun(dir, "job", "nightly", id, fakeRecord(id, st, 50)); err != nil {
			t.Fatal(err)
		}
		want[id] = st
	}
	// a record whose status comes late (not as jobs writes one) is read whole
	late := "20261110-020009-ffff"
	if err := SaveRun(dir, "pipeline", "p", late, []byte(`{"id":"`+late+`","pipelines":[],"status":"running"}`)); err != nil {
		t.Fatal(err)
	}
	want[late] = "running"
	// and a file that is not a record is skipped
	if err := os.WriteFile(dir+"/job/nightly/20261110-020010-0000.json", []byte("[1,2]"), 0o644); err != nil {
		t.Fatal(err)
	}

	sts, err := RunStatuses(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range sts {
		got[s.ID] = s.Status
		if s.Path == "" || (s.Kind != "job" && s.Kind != "pipeline") {
			t.Errorf("%+v", s)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
	heads, err := ListRuns(dir, RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range heads {
		if got[h.ID] != h.Status {
			t.Errorf("%s: RunStatuses says %q, its head %q", h.ID, got[h.ID], h.Status)
		}
	}
	idx, err := RunIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for id := range idx {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if len(ids) != 6 { // the five records, and the non-record by its name
		t.Errorf("index = %v", ids)
	}
	if bs, _, err := ReadRunAt(idx[late]); err != nil || !strings.Contains(string(bs), late) {
		t.Errorf("ReadRunAt: %v", err)
	}
}

// What a headless run's Recover reads: every record's status (now) against
// every record's head (before), over 200 records of 2,000 log lines.
func BenchmarkRecoverRead(b *testing.B) {
	dir := b.TempDir()
	for i := range 200 {
		id := fmt.Sprintf("20261110-%06d-%04x", i, i)
		if err := SaveRun(dir, "job", "nightly", id, fakeRecord(id, "succeeded", 2000)); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("statuses", func(b *testing.B) {
		for b.Loop() {
			if _, err := RunStatuses(dir); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("heads", func(b *testing.B) {
		for b.Loop() {
			heads.dirs = map[string]map[string]cachedHead{} // a one-shot process: no head cache
			if _, err := ListRuns(dir, RunFilter{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
