package userdata

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// What a one-shot process needs of the run records, read cheaply (N-285).
//
// A headless `dbc job run` or `dbc pipeline run` settles the records a
// dead process left running (jobs Engine.Recover) before it starts. That
// used to list every head (ListRuns), and a head is parsed from the whole
// file — up to runs_keep (200) records a name, each with up to 2,000 log
// lines — since its Rows sum lies deep in the record; a one-shot process
// has no head cache to save it. Recover needs only each record's status,
// which a record writes near its start:
//
//	{
//	  "id": "20261009-020000-7f3a", "kind": "job", "name": "nightly",
//	  "trigger": "cron", …, "started": …, "ended": …,
//	  "status": "running",           ◄── RunStatuses stops reading here
//	  …
//	  "pipelines": [ … thousands of log lines … ]
//	}
//
// so RunStatuses decodes each file's top-level keys in order and stops at
// "status": a few hundred bytes of a record that may be megabytes. A file
// whose status is not among its first keys (none is written so) is read
// whole, as before.

// RunStatus is a record's identity and status, without the rest of it.
type RunStatus struct {
	ID     string
	Kind   string
	Name   string
	Status string
	Path   string // the record's file
}

// statusScanKeys is how many top-level keys RunStatuses reads looking for
// "status" before it gives up and parses the record whole. A record has
// a dozen or so before it.
const statusScanKeys = 32

// RunStatuses lists every record's status under dir, reading only the
// start of each file. Unreadable or malformed files are skipped, as
// ListRuns skips them.
func RunStatuses(dir string) ([]RunStatus, error) {
	if dir == "" {
		return nil, nil
	}
	var out []RunStatus
	err := eachRunFile(dir, func(kind, name, id, path string) {
		st, err := readStatus(path)
		if err != nil {
			return
		}
		out = append(out, RunStatus{ID: id, Kind: kind, Name: name, Status: st, Path: path})
	})
	return out, err
}

// RunIndex maps every record's id to its file, in one pass over the runs
// directory: what a caller reading many records by id uses instead of
// ReadRun's glob per id.
func RunIndex(dir string) (map[string]string, error) {
	out := map[string]string{}
	if dir == "" {
		return out, nil
	}
	err := eachRunFile(dir, func(_, _, id, path string) { out[id] = path })
	return out, err
}

// ReadRunAt is ReadRun for a record whose file is known (RunIndex,
// RunStatuses): its bytes and modification time, with no glob.
func ReadRunAt(path string) ([]byte, time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, serr.Wrap(err, "op", "read run", "path", path)
	}
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, serr.Wrap(err, "op", "read run", "path", path)
	}
	return bs, fi.ModTime(), nil
}

// eachRunFile calls fn for every record file under dir: <kind>/<name>/<id>.json.
func eachRunFile(dir string, fn func(kind, name, id, path string)) error {
	for _, kind := range RunKinds {
		names, err := os.ReadDir(filepath.Join(dir, kind))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return serr.Wrap(err, "op", "list runs", "dir", dir)
		}
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			d := filepath.Join(dir, kind, n.Name())
			ents, err := os.ReadDir(d)
			if err != nil {
				continue
			}
			for _, e := range ents {
				id, ok := strings.CutSuffix(e.Name(), ".json")
				if !ok || !ValidRunID(id) || !e.Type().IsRegular() {
					continue
				}
				fn(kind, n.Name(), id, filepath.Join(d, e.Name()))
			}
		}
	}
	return nil
}

// readStatus is a record's "status", read from the front of its file: the
// top-level keys are decoded in order, each value skipped as raw JSON,
// until "status". The reader is buffered small, so a record's long tail
// is never read.
func readStatus(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 2048))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", serr.New("not a run record", "path", path)
	}
	for range statusScanKeys {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		key, ok := tok.(string)
		if !ok {
			break // the object ended with no status among its keys
		}
		if key == "status" {
			var st string
			if err := dec.Decode(&st); err != nil {
				return "", err
			}
			return st, nil
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", err
		}
	}
	// not among the first keys: the whole head, as ListRuns reads it
	h, err := readHead(path)
	if err != nil {
		return "", err
	}
	return h.Status, nil
}
