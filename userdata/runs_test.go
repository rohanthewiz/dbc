package userdata

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Run records: saved by kind and name, listed newest first through the
// filters, read back by id alone, pruned to the newest few — never a live
// one.
func TestRunRecords(t *testing.T) {
	dir := t.TempDir()
	save := func(kind, name, id, status string, started time.Time, rows int64) {
		t.Helper()
		rec := map[string]any{"id": id, "kind": kind, "name": name, "trigger": "manual", "status": status,
			"started": started, "pipelines": []any{map[string]any{"fragments": []any{map[string]any{"rows": rows}}}}}
		bs, _ := json.Marshal(rec)
		if err := SaveRun(dir, kind, name, id, bs); err != nil {
			t.Fatal(err)
		}
	}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 2, 0, 0, 0, time.Local) }
	save("job", "nightly", "20261007-020000-aaaa", "failed", day(7), 0)
	save("job", "nightly", "20261008-020000-bbbb", "succeeded", day(8), 12)
	save("job", "nightly", "20261009-020000-cccc", "running", day(9), 3)
	save("pipeline", "copy-cats", "20261008-110000-dddd", "succeeded", day(8).Add(9*time.Hour), 9)

	all, err := ListRuns(dir, RunFilter{})
	if err != nil || len(all) != 4 || all[0].ID != "20261009-020000-cccc" || all[1].ID != "20261008-110000-dddd" {
		t.Fatalf("all: %+v %v", all, err)
	}
	if all[0].Rows != 3 || all[0].Mod.IsZero() {
		t.Errorf("head: %+v", all[0])
	}
	for f, want := range map[*RunFilter]int{
		{Kind: "job"}:                  3,
		{Name: "copy-cats"}:            1,
		{Status: "failed"}:             1,
		{Since: day(8)}:                3,
		{Kind: "job", Limit: 2}:        2,
		{Kind: "pipeline", Name: "zz"}: 0,
	} {
		got, err := ListRuns(dir, *f)
		if err != nil || len(got) != want {
			t.Errorf("filter %+v: %d runs (%v), want %d", *f, len(got), err, want)
		}
	}
	bs, mod, err := ReadRun(dir, "20261008-110000-dddd")
	if err != nil || mod.IsZero() || !json.Valid(bs) {
		t.Fatalf("read: %v", err)
	}
	if _, _, err = ReadRun(dir, "20261008-110000-ffff"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, _, err = ReadRun(dir, "../../etc"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("bad id: %v", err)
	}
	if err = SaveRun(dir, "job", "../x", "20261008-110000-dddd", bs); err == nil {
		t.Error("a name that climbs out was saved")
	}
	if fi, err := os.Stat(filepath.Join(dir, "job", "nightly", "20261008-020000-bbbb.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode: %v %v", fi.Mode(), err)
	}

	// keep 1: the two older go — but the running one, live, is kept even
	// though it is the newest anyway; make an older one live to see it kept
	n, err := PruneRuns(dir, "job", "nightly", 1, func(h RunHead) bool { return h.ID == "20261007-020000-aaaa" })
	if err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	left, _ := ListRuns(dir, RunFilter{Kind: "job"})
	if len(left) != 2 || left[0].ID != "20261009-020000-cccc" || left[1].ID != "20261007-020000-aaaa" {
		t.Errorf("after prune: %+v", left)
	}
}
