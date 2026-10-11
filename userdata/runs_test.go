package userdata

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// RunsStamp moves when a record is written and when one is rewritten in
// place (the atomic write renames a file into the directory), and is zero
// for a runs dir with nothing in it.
func TestRunsStamp(t *testing.T) {
	dir := t.TempDir()
	if s := RunsStamp(dir); !s.IsZero() {
		t.Fatalf("empty dir: %v", s)
	}
	write := func(id string) time.Time {
		t.Helper()
		// past the file system's clock grain, so each write is later
		time.Sleep(20 * time.Millisecond)
		if err := SaveRun(dir, "job", "nightly", id, []byte(`{"id": "`+id+`"}`)); err != nil {
			t.Fatal(err)
		}
		return RunsStamp(dir)
	}
	first := write("20261009-020000-aaaa")
	if first.IsZero() {
		t.Fatal("no stamp after a write")
	}
	again := write("20261009-020000-aaaa")
	if !again.After(first) {
		t.Errorf("a rewrite did not move the stamp: %v then %v", first, again)
	}
	if s := RunsStamp(dir); !s.Equal(again) {
		t.Errorf("an unchanged dir's stamp moved: %v then %v", again, s)
	}
}

// A listing parses a record only when its file changed since this process
// last read it: a rewrite through SaveRun is seen at once, even with the
// same size in the same clock tick; a record pruned leaves the cache; and
// a file changed under the cache's nose — same file, size and time, which
// no writer of records does — still reads as the cached head, which is
// how the test sees the cache answering.
func TestRunHeadsCache(t *testing.T) {
	dir := t.TempDir()
	const id = "20261009-020000-aaaa"
	save := func(id, status string) {
		t.Helper()
		bs := []byte(`{"id": "` + id + `", "kind": "job", "name": "nightly", "status": "` + status + `"}`)
		if err := SaveRun(dir, "job", "nightly", id, bs); err != nil {
			t.Fatal(err)
		}
	}
	status := func() string {
		t.Helper()
		hs, err := ListRuns(dir, RunFilter{Kind: "job", Name: "nightly"})
		if err != nil || len(hs) == 0 {
			t.Fatalf("list: %v %v", hs, err)
		}
		return hs[len(hs)-1].Status
	}
	save(id, "running")
	if s := status(); s != "running" {
		t.Fatalf("first read: %q", s)
	}
	// "failed!" is as long as "running": only the file's identity differs
	save(id, "failed!")
	if s := status(); s != "failed!" {
		t.Errorf("a rewrite of the same size was not re-read: %q", s)
	}

	p := filepath.Join(dir, "job", "nightly", id+".json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	bs, _ := os.ReadFile(p)
	_, err = f.WriteAt([]byte(strings.Replace(string(bs), "failed!", "zzzzzzz", 1)), 0)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(p, fi.ModTime(), fi.ModTime())
	}
	if err != nil {
		t.Fatal(err)
	}
	if s := status(); s != "failed!" {
		t.Errorf("an unchanged file was parsed again: %q", s)
	}

	save("20261008-020000-bbbb", "succeeded")
	_ = status()
	d := filepath.Join(dir, "job", "nightly")
	if n := len(heads.dirs[d]); n != 2 {
		t.Fatalf("cached heads: %d, want 2", n)
	}
	if _, err = PruneRuns(dir, "job", "nightly", 1, nil); err != nil {
		t.Fatal(err)
	}
	_ = status()
	if _, ok := heads.dirs[d]["20261008-020000-bbbb.json"]; ok || len(heads.dirs[d]) != 1 {
		t.Errorf("a pruned record stayed cached: %v", heads.dirs[d])
	}
}
