package userdata

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// specStore is the store for a directory of JSON specs — pipelines
// (pipelines.go) and jobs (jobs.go) — which is the scripts store
// (scripts.go) for .json files: plain files on purpose, so a spec stays
// runnable from cron by name and diffable in git; a save names the
// revision it was made from and is refused as a conflict when the file
// changed since; a delete is a move into .trash, which keeps the most
// recent trashMax.
//
//	<dir>/
//	    orders-nightly.json
//	    .trash/
//	        old-load.1759769402123.json   ◄─ old-load.json, trashed at that Unix ms
//
// The two kinds differ only in their words (what a message calls them,
// their errors) and in what a listing reads off a file, which each kind
// does over specFile. Every method takes a NAME (the file name, with
// .json), never a path, and checks it with validSpecName first.
type specStore struct {
	what     string // "pipeline", "job": for messages and serr ops
	exists   error  // a create, rename or restore onto a name the dir has
	badName  error  // a name validSpecName refuses
	maxBytes int
}

// validSpecName is the console-name rule for the stem, plus .json.
func validSpecName(name string) bool {
	stem, ok := strings.CutSuffix(name, ".json")
	return ok && ValidConsoleName(stem)
}

func (st specStore) path(dir, name string) (string, error) {
	if dir == "" {
		return "", serr.New("no " + st.what + "s directory")
	}
	if !validSpecName(name) {
		return "", serr.Wrap(st.badName, "name", name)
	}
	return filepath.Join(dir, name), nil
}

// specFile is one spec file as a listing reads it: the kind turns src
// into its own info.
type specFile struct {
	Name string
	Size int64
	Mod  time.Time
	Rev  string
	Src  []byte
}

// list reads every spec in dir, by name. A missing dir is none.
func (st specStore) list(dir string) ([]specFile, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, serr.Wrap(err, "op", "list "+st.what+"s", "dir", dir)
	}
	var out []specFile
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		out = append(out, specFile{Name: e.Name(), Size: fi.Size(), Mod: fi.ModTime(), Rev: textRev(string(src), true), Src: src})
	}
	slices.SortFunc(out, func(a, b specFile) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// read is a spec's text and revision; a missing one errors.Is
// fs.ErrNotExist.
func (st specStore) read(dir, name string) (text, rev string, err error) {
	p, err := st.path(dir, name)
	if err != nil {
		return "", "", err
	}
	bs, err := os.ReadFile(p)
	if err != nil {
		return "", "", serr.Wrap(err, "op", "read "+st.what, "name", name)
	}
	return string(bs), textRev(string(bs), true), nil
}

// save writes text as name, made from revision base, by SaveScript's
// rules: "" creates and never overwrites; a changed file is a conflict
// (curRev is its revision now), not an error.
func (st specStore) save(dir, name, text, base string) (rev string, conflict bool, err error) {
	p, err := st.path(dir, name)
	if err != nil {
		return "", false, err
	}
	if len(text) > st.maxBytes {
		return "", false, serr.New(st.what+" too large", "name", name,
			"bytes", strconv.Itoa(len(text)), "max", strconv.Itoa(st.maxBytes))
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	perm := os.FileMode(0o600)
	cur, readErr := os.ReadFile(p)
	exists := readErr == nil
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return "", false, serr.Wrap(readErr, "op", "save "+st.what, "name", name)
	}
	if curRev := textRev(string(cur), exists); curRev != base {
		if base == "" {
			return curRev, false, serr.Wrap(st.exists, "name", name)
		}
		return curRev, true, nil
	}
	if exists {
		if target, err := filepath.EvalSymlinks(p); err == nil {
			p = target
		}
		if fi, err := os.Stat(p); err == nil {
			perm = fi.Mode().Perm()
		}
	}
	if err = writeAtomic(p, []byte(text), perm); err != nil {
		return "", false, serr.Wrap(err, "op", "save "+st.what, "name", name)
	}
	return textRev(text, true), false, nil
}

// rename renames from to to, refusing to replace another.
func (st specStore) rename(dir, from, to string) error {
	src, err := st.path(dir, from)
	if err != nil {
		return err
	}
	dst, err := st.path(dir, to)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	srcSt, err := os.Lstat(src)
	if err != nil {
		return serr.Wrap(err, "op", "rename "+st.what, "name", from)
	}
	if dstSt, err := os.Lstat(dst); err == nil && !os.SameFile(srcSt, dstSt) {
		return serr.Wrap(st.exists, "name", to)
	}
	if err = os.Rename(src, dst); err != nil {
		return serr.Wrap(err, "op", "rename "+st.what, "from", from, "to", to)
	}
	return nil
}

// specTrashRe splits a trash file name into the stem and the Unix
// milliseconds it was trashed at.
var specTrashRe = regexp.MustCompile(`^(.+)\.([0-9]{1,19})\.json$`)

// SpecTrashInfo is one trashed spec (a pipeline or a job).
type SpecTrashInfo struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Trashed time.Time `json:"trashed"`
}

func parseSpecTrashID(id string) (name string, at time.Time, ok bool) {
	m := specTrashRe.FindStringSubmatch(id)
	if m == nil || !validSpecName(m[1]+".json") {
		return "", time.Time{}, false
	}
	ms, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return m[1] + ".json", time.UnixMilli(ms), true
}

// trash moves name into .trash and returns its trash ID.
func (st specStore) trash(dir, name string) (id string, err error) {
	src, err := st.path(dir, name)
	if err != nil {
		return "", err
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	if _, err = os.Lstat(src); err != nil {
		return "", serr.Wrap(err, "op", "trash "+st.what, "name", name)
	}
	td := filepath.Join(dir, trashDir)
	if err = os.MkdirAll(td, 0o755); err != nil {
		return "", serr.Wrap(err, "op", "trash "+st.what, "name", name)
	}
	stem := strings.TrimSuffix(name, ".json")
	ms := time.Now().UnixMilli()
	for {
		id = fmt.Sprintf("%s.%d.json", stem, ms)
		if _, err := os.Lstat(filepath.Join(td, id)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		ms++
	}
	if err = os.Rename(src, filepath.Join(td, id)); err != nil {
		return "", serr.Wrap(err, "op", "trash "+st.what, "name", name)
	}
	items := listSpecTrash(td)
	for _, it := range items[min(len(items), trashMax):] {
		_ = os.Remove(filepath.Join(td, it.ID))
	}
	return id, nil
}

// listTrash is the trashed specs in dir, newest first.
func (st specStore) listTrash(dir string) []SpecTrashInfo {
	if dir == "" {
		return nil
	}
	return listSpecTrash(filepath.Join(dir, trashDir))
}

func listSpecTrash(td string) []SpecTrashInfo {
	ents, err := os.ReadDir(td)
	if err != nil {
		return nil
	}
	var out []SpecTrashInfo
	for _, e := range ents {
		if name, at, ok := parseSpecTrashID(e.Name()); ok && e.Type().IsRegular() {
			out = append(out, SpecTrashInfo{ID: e.Name(), Name: name, Trashed: at})
		}
	}
	slices.SortFunc(out, func(a, b SpecTrashInfo) int {
		if c := b.Trashed.Compare(a.Trashed); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// restore moves trashed id back out of .trash, as to — or under its old
// name when to is "" — never replacing one.
func (st specStore) restore(dir, id, to string) (string, error) {
	name, _, ok := parseSpecTrashID(id)
	if !ok {
		return "", serr.New("not a trashed "+st.what, "id", id)
	}
	if to == "" {
		to = name
	}
	dst, err := st.path(dir, to)
	if err != nil {
		return "", err
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	if _, err = os.Lstat(dst); err == nil {
		return "", serr.Wrap(st.exists, "name", to)
	}
	if err = os.Rename(filepath.Join(dir, trashDir, id), dst); err != nil {
		return "", serr.Wrap(err, "op", "restore "+st.what, "id", id)
	}
	return to, nil
}
