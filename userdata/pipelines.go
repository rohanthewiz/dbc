package userdata

import (
	"encoding/json"
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

// Pipelines: the JSON specs in the pipelines directory
// (config.Config.PipelinesDir, absolute). The store is the scripts store
// (scripts.go) for .json files: plain files on purpose, so a pipeline
// stays runnable as `dbc pipeline run NAME` from cron and diffable in
// git; a save names the revision it was made from and is refused as a
// conflict when the file changed since; a delete is a move into .trash.
//
//	<pipelines dir>/
//	    orders-nightly.json
//	    .trash/
//	        old-load.1759769402123.json
//
// Every function takes the directory and a NAME (the file name, with
// .json), never a path, and checks it with ValidPipelineName.

// PipelineInfo is one pipeline, as a list shows it.
type PipelineInfo struct {
	Name      string    `json:"name"`      // file name, with .json
	Desc      string    `json:"desc"`      // the spec's desc; "" if none or unreadable
	Fragments int       `json:"fragments"` // how many; 0 when the file does not parse
	Size      int64     `json:"size"`
	Mod       time.Time `json:"mod"`
	Rev       string    `json:"rev"`
}

// ValidPipelineName reports whether name may name a pipeline: the
// console-name rule for the stem, plus a required .json suffix.
func ValidPipelineName(name string) bool {
	stem, ok := strings.CutSuffix(name, ".json")
	return ok && ValidConsoleName(stem)
}

// ErrPipelineExists is a create, rename or restore onto a name the
// directory already has; ErrBadPipelineName a name ValidPipelineName
// refuses.
var (
	ErrPipelineExists  = errors.New("a pipeline by that name already exists")
	ErrBadPipelineName = errors.New("not a pipeline name: letters, digits, . _ - ending in .json")
)

// MaxPipelineBytes bounds what a save writes.
const MaxPipelineBytes = 1 << 20

func pipelinePath(dir, name string) (string, error) {
	if dir == "" {
		return "", serr.New("no pipelines directory")
	}
	if !ValidPipelineName(name) {
		return "", serr.Wrap(ErrBadPipelineName, "name", name)
	}
	return filepath.Join(dir, name), nil
}

// pipelineHead is what a listing reads off a spec: its description and
// fragment count, without package pipeline (userdata is below it).
type pipelineHead struct {
	Desc      string            `json:"desc"`
	Fragments []json.RawMessage `json:"fragments"`
}

// ListPipelines lists the pipelines in dir by name. A missing dir is none
// and no error.
func ListPipelines(dir string) ([]PipelineInfo, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, serr.Wrap(err, "op", "list pipelines", "dir", dir)
	}
	var out []PipelineInfo
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var head pipelineHead
		_ = json.Unmarshal(src, &head)
		out = append(out, PipelineInfo{Name: e.Name(), Desc: head.Desc, Fragments: len(head.Fragments),
			Size: st.Size(), Mod: st.ModTime(), Rev: textRev(string(src), true)})
	}
	slices.SortFunc(out, func(a, b PipelineInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ReadPipeline returns a pipeline's text and revision. A missing one is
// an error that errors.Is fs.ErrNotExist.
func ReadPipeline(dir, name string) (text, rev string, err error) {
	p, err := pipelinePath(dir, name)
	if err != nil {
		return "", "", err
	}
	bs, err := os.ReadFile(p)
	if err != nil {
		return "", "", serr.Wrap(err, "op", "read pipeline", "name", name)
	}
	return string(bs), textRev(string(bs), true), nil
}

// SavePipeline writes text as pipeline name, made from revision base, by
// SaveScript's rules: "" creates and never overwrites; a changed file is a
// conflict (curRev is its revision now), not an error.
func SavePipeline(dir, name, text, base string) (rev string, conflict bool, err error) {
	p, err := pipelinePath(dir, name)
	if err != nil {
		return "", false, err
	}
	if len(text) > MaxPipelineBytes {
		return "", false, serr.New("pipeline too large", "name", name,
			"bytes", strconv.Itoa(len(text)), "max", strconv.Itoa(MaxPipelineBytes))
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	perm := os.FileMode(0o600)
	cur, readErr := os.ReadFile(p)
	exists := readErr == nil
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return "", false, serr.Wrap(readErr, "op", "save pipeline", "name", name)
	}
	if curRev := textRev(string(cur), exists); curRev != base {
		if base == "" {
			return curRev, false, serr.Wrap(ErrPipelineExists, "name", name)
		}
		return curRev, true, nil
	}
	if exists {
		if target, err := filepath.EvalSymlinks(p); err == nil {
			p = target
		}
		if st, err := os.Stat(p); err == nil {
			perm = st.Mode().Perm()
		}
	}
	if err = writeAtomic(p, []byte(text), perm); err != nil {
		return "", false, serr.Wrap(err, "op", "save pipeline", "name", name)
	}
	return textRev(text, true), false, nil
}

// RenamePipeline renames pipeline from to to, refusing to replace another.
func RenamePipeline(dir, from, to string) error {
	src, err := pipelinePath(dir, from)
	if err != nil {
		return err
	}
	dst, err := pipelinePath(dir, to)
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
		return serr.Wrap(err, "op", "rename pipeline", "name", from)
	}
	if dstSt, err := os.Lstat(dst); err == nil && !os.SameFile(srcSt, dstSt) {
		return serr.Wrap(ErrPipelineExists, "name", to)
	}
	if err = os.Rename(src, dst); err != nil {
		return serr.Wrap(err, "op", "rename pipeline", "from", from, "to", to)
	}
	return nil
}

// pipelineTrashRe splits a trash file name into the stem and the Unix
// milliseconds it was trashed at.
var pipelineTrashRe = regexp.MustCompile(`^(.+)\.([0-9]{1,19})\.json$`)

// PipelineTrashInfo is one trashed pipeline.
type PipelineTrashInfo struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Trashed time.Time `json:"trashed"`
}

func parsePipelineTrashID(id string) (name string, at time.Time, ok bool) {
	m := pipelineTrashRe.FindStringSubmatch(id)
	if m == nil || !ValidPipelineName(m[1]+".json") {
		return "", time.Time{}, false
	}
	ms, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return m[1] + ".json", time.UnixMilli(ms), true
}

// TrashPipeline moves a pipeline into .trash and returns its trash ID.
func TrashPipeline(dir, name string) (id string, err error) {
	src, err := pipelinePath(dir, name)
	if err != nil {
		return "", err
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	if _, err = os.Lstat(src); err != nil {
		return "", serr.Wrap(err, "op", "trash pipeline", "name", name)
	}
	td := filepath.Join(dir, trashDir)
	if err = os.MkdirAll(td, 0o755); err != nil {
		return "", serr.Wrap(err, "op", "trash pipeline", "name", name)
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
		return "", serr.Wrap(err, "op", "trash pipeline", "name", name)
	}
	items := listPipelineTrash(td)
	for _, it := range items[min(len(items), trashMax):] {
		_ = os.Remove(filepath.Join(td, it.ID))
	}
	return id, nil
}

// ListPipelineTrash lists the trashed pipelines in dir, newest first.
func ListPipelineTrash(dir string) []PipelineTrashInfo {
	if dir == "" {
		return nil
	}
	return listPipelineTrash(filepath.Join(dir, trashDir))
}

func listPipelineTrash(td string) []PipelineTrashInfo {
	ents, err := os.ReadDir(td)
	if err != nil {
		return nil
	}
	var out []PipelineTrashInfo
	for _, e := range ents {
		if name, at, ok := parsePipelineTrashID(e.Name()); ok && e.Type().IsRegular() {
			out = append(out, PipelineTrashInfo{ID: e.Name(), Name: name, Trashed: at})
		}
	}
	slices.SortFunc(out, func(a, b PipelineTrashInfo) int {
		if c := b.Trashed.Compare(a.Trashed); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// RestorePipeline moves trashed pipeline id back out of .trash, as to —
// or under its old name when to is "" — never replacing one.
func RestorePipeline(dir, id, to string) (string, error) {
	name, _, ok := parsePipelineTrashID(id)
	if !ok {
		return "", serr.New("not a trashed pipeline", "id", id)
	}
	if to == "" {
		to = name
	}
	dst, err := pipelinePath(dir, to)
	if err != nil {
		return "", err
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	if _, err = os.Lstat(dst); err == nil {
		return "", serr.Wrap(ErrPipelineExists, "name", to)
	}
	if err = os.Rename(filepath.Join(dir, trashDir, id), dst); err != nil {
		return "", serr.Wrap(err, "op", "restore pipeline", "id", id)
	}
	return to, nil
}
