package userdata

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// Run records: one JSON file per run of a job or a pipeline, written by the
// process that runs it (package jobs) and read by any — dbc web's Runs
// view, `dbc runs` from a shell, a cron job's own `dbc run show`.
//
//	<runs dir>/
//	    job/
//	        nightly/
//	            20261009-020000-7f3a.json
//	            20261010-020000-c401.json
//	    pipeline/
//	        clean-and-load/
//	            20261009-114502-0b9e.json
//
// WHY FILES, NOT A runs.bytdb. Several processes write and read runs at
// once — dbc web firing a schedule, cron running `dbc job run`, a shell
// listing — and bytdb's lock is exclusive per process, so a shared file
// would belong to whichever opened it first. One file per run has no
// contention: each run writes only its own, atomically (writeAtomic), and
// a listing is a directory walk. A crash leaves the last write standing —
// a record still saying "running", which the next process to look turns
// into "interrupted" (package jobs: it judges by the file's age, since a
// live run rewrites its record every few seconds).
//
// The records are the runs' own business; this file only stores bytes and
// reads the header fields a listing needs (RunHead), because userdata sits
// below package jobs, which owns the record's shape.

// RunKinds are the kinds of run a record may be.
var RunKinds = []string{"job", "pipeline"}

// runIDRe is a run id: the start to the second, then four hex digits.
var runIDRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{4}$`)

// ValidRunID reports whether id is shaped like a run id, so it may name a
// file.
func ValidRunID(id string) bool { return runIDRe.MatchString(id) }

// RunHead is what a listing reads off a record.
type RunHead struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Trigger string    `json:"trigger"`
	By      string    `json:"by,omitempty"`
	Status  string    `json:"status"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	Error   string    `json:"error,omitempty"`
	// Rows is every fragment's committed rows, over all its pipelines.
	Rows int64 `json:"rows"`
	// Mod is when the file was last written: a "running" record that has
	// not been rewritten for a while belongs to a process that is gone.
	Mod time.Time `json:"mod"`
}

// runFile is the part of a record RunHead is read from.
type runFile struct {
	RunHead
	Pipelines []struct {
		Fragments []struct {
			Rows int64 `json:"rows"`
		} `json:"fragments"`
	} `json:"pipelines"`
}

// runNameRe is what a job or pipeline may be called (pipeline.ValidName's
// rule, which userdata cannot import): the record's directory is named so.
var runNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,99}$`)

func runDir(dir, kind, name string) (string, error) {
	if dir == "" {
		return "", serr.New("no runs directory")
	}
	if !slices.Contains(RunKinds, kind) || !runNameRe.MatchString(name) || strings.HasSuffix(name, ".") {
		return "", serr.New("not a run's kind and name", "kind", kind, "name", name)
	}
	return filepath.Join(dir, kind, name), nil
}

// SaveRun writes a run's record, replacing the last one written. The
// directories are 0700 and the file 0600: a record holds error texts,
// which may quote data.
func SaveRun(dir, kind, name, id string, data []byte) error {
	d, err := runDir(dir, kind, name)
	if err != nil {
		return err
	}
	if !ValidRunID(id) {
		return serr.New("not a run id", "id", id)
	}
	if err = os.MkdirAll(d, 0o700); err != nil {
		return serr.Wrap(err, "op", "save run", "dir", d)
	}
	if err = writeAtomic(filepath.Join(d, id+".json"), data, 0o600); err != nil {
		return serr.Wrap(err, "op", "save run", "id", id)
	}
	return nil
}

// RunFilter narrows ListRuns. Zero values match everything; Limit 0 is
// no limit.
type RunFilter struct {
	Kind   string
	Name   string
	Status string
	Since  time.Time // started at or after
	Limit  int       // the newest this many, after the other filters
}

// ListRuns lists the records in dir that pass f, newest first. A record
// that does not parse is left out; a missing dir is none.
func ListRuns(dir string, f RunFilter) ([]RunHead, error) {
	if dir == "" {
		return nil, nil
	}
	kinds := RunKinds
	if f.Kind != "" {
		kinds = []string{f.Kind}
	}
	var out []RunHead
	for _, kind := range kinds {
		names, err := os.ReadDir(filepath.Join(dir, kind))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, serr.Wrap(err, "op", "list runs", "dir", dir)
		}
		for _, n := range names {
			if !n.IsDir() || (f.Name != "" && n.Name() != f.Name) {
				continue
			}
			heads, err := readHeads(filepath.Join(dir, kind, n.Name()), f)
			if err != nil {
				return nil, err
			}
			out = append(out, heads...)
		}
	}
	slices.SortFunc(out, func(a, b RunHead) int {
		if c := b.Started.Compare(a.Started); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// readHeads reads the records of one kind/name directory that pass f.
// The id is the start to the second, so a Since filter skips a file by its
// name before reading it.
func readHeads(d string, f RunFilter) ([]RunHead, error) {
	ents, err := os.ReadDir(d)
	if err != nil {
		return nil, serr.Wrap(err, "op", "list runs", "dir", d)
	}
	var out []RunHead
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !ValidRunID(id) || !e.Type().IsRegular() {
			continue
		}
		if !f.Since.IsZero() {
			if t, err := time.ParseInLocation("20060102-150405", id[:15], time.Local); err == nil &&
				t.Before(f.Since.Add(-time.Second)) {
				continue
			}
		}
		h, err := readHead(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		if (f.Status != "" && h.Status != f.Status) || (!f.Since.IsZero() && h.Started.Before(f.Since)) {
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

func readHead(p string) (RunHead, error) {
	bs, err := os.ReadFile(p)
	if err != nil {
		return RunHead{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return RunHead{}, err
	}
	var rf runFile
	if err = json.Unmarshal(bs, &rf); err != nil {
		return RunHead{}, err
	}
	h := rf.RunHead
	for _, p := range rf.Pipelines {
		for _, fr := range p.Fragments {
			h.Rows += fr.Rows
		}
	}
	h.Mod = fi.ModTime()
	return h, nil
}

// ReadRun is one record's bytes, found by id under any kind and name, and
// its file's modification time. A missing one errors.Is fs.ErrNotExist.
func ReadRun(dir, id string) ([]byte, time.Time, error) {
	if dir == "" || !ValidRunID(id) {
		return nil, time.Time{}, serr.Wrap(fs.ErrNotExist, "id", id)
	}
	// the glob's pattern holds nothing from the request but a checked id
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*", id+".json"))
	if len(matches) == 0 {
		return nil, time.Time{}, serr.Wrap(fs.ErrNotExist, "op", "read run", "id", id)
	}
	bs, err := os.ReadFile(matches[0])
	if err != nil {
		return nil, time.Time{}, serr.Wrap(err, "op", "read run", "id", id)
	}
	fi, err := os.Stat(matches[0])
	if err != nil {
		return nil, time.Time{}, serr.Wrap(err, "op", "read run", "id", id)
	}
	return bs, fi.ModTime(), nil
}

// PruneRuns keeps the newest keep records of one kind and name, removing
// the older ones — except a record live elsewhere (live reports it: a
// "running" record a process is still writing), which is never taken from
// under its writer. It returns how many it removed.
func PruneRuns(dir, kind, name string, keep int, live func(RunHead) bool) (int, error) {
	d, err := runDir(dir, kind, name)
	if err != nil {
		return 0, err
	}
	if keep <= 0 {
		return 0, nil
	}
	ents, err := os.ReadDir(d)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, serr.Wrap(err, "op", "prune runs", "dir", d)
	}
	var ids []string
	for _, e := range ents {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && ValidRunID(id) && e.Type().IsRegular() {
			ids = append(ids, id)
		}
	}
	// ids sort by start: the oldest first
	slices.Sort(ids)
	removed := 0
	for _, id := range ids[:max(len(ids)-keep, 0)] {
		p := filepath.Join(d, id+".json")
		if h, err := readHead(p); err == nil && live != nil && live(h) {
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	return removed, nil
}
