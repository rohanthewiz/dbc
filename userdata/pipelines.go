package userdata

import (
	"encoding/json"
	"errors"
	"time"
)

// Pipelines: the JSON specs in the pipelines directory
// (config.Config.PipelinesDir, absolute), kept by the specs store
// (specstore.go), which jobs share: the scripts store (scripts.go) for
// .json files — plain files on purpose, so a pipeline stays runnable as
// `dbc pipeline run NAME` from cron and diffable in git; a save names the
// revision it was made from and is refused as a conflict when the file
// changed since; a delete is a move into .trash.
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
func ValidPipelineName(name string) bool { return validSpecName(name) }

// ErrPipelineExists is a create, rename or restore onto a name the
// directory already has; ErrBadPipelineName a name ValidPipelineName
// refuses.
var (
	ErrPipelineExists  = errors.New("a pipeline by that name already exists")
	ErrBadPipelineName = errors.New("not a pipeline name: letters, digits, . _ - ending in .json")
)

// MaxPipelineBytes bounds what a save writes.
const MaxPipelineBytes = 1 << 20

// pipelineStore is the specs store (specstore.go) in a pipeline's words.
var pipelineStore = specStore{what: "pipeline", exists: ErrPipelineExists, badName: ErrBadPipelineName,
	maxBytes: MaxPipelineBytes}

// pipelineHead is what a listing reads off a spec: its description and
// fragment count, without package pipeline (userdata is below it).
type pipelineHead struct {
	Desc      string            `json:"desc"`
	Fragments []json.RawMessage `json:"fragments"`
}

// ListPipelines lists the pipelines in dir by name. A missing dir is none
// and no error.
func ListPipelines(dir string) ([]PipelineInfo, error) {
	files, err := pipelineStore.list(dir)
	if err != nil {
		return nil, err
	}
	var out []PipelineInfo
	for _, f := range files {
		var head pipelineHead
		_ = json.Unmarshal(f.Src, &head)
		out = append(out, PipelineInfo{Name: f.Name, Desc: head.Desc, Fragments: len(head.Fragments),
			Size: f.Size, Mod: f.Mod, Rev: f.Rev})
	}
	return out, nil
}

// ReadPipeline returns a pipeline's text and revision. A missing one is
// an error that errors.Is fs.ErrNotExist.
func ReadPipeline(dir, name string) (text, rev string, err error) {
	return pipelineStore.read(dir, name)
}

// SavePipeline writes text as pipeline name, made from revision base, by
// SaveScript's rules: "" creates and never overwrites; a changed file is a
// conflict (curRev is its revision now), not an error.
func SavePipeline(dir, name, text, base string) (rev string, conflict bool, err error) {
	return pipelineStore.save(dir, name, text, base)
}

// RenamePipeline renames pipeline from to to, refusing to replace another.
func RenamePipeline(dir, from, to string) error {
	return pipelineStore.rename(dir, from, to)
}

// PipelineTrashInfo is one trashed pipeline.
type PipelineTrashInfo = SpecTrashInfo

// TrashPipeline moves a pipeline into .trash and returns its trash ID.
func TrashPipeline(dir, name string) (id string, err error) {
	return pipelineStore.trash(dir, name)
}

// ListPipelineTrash lists the trashed pipelines in dir, newest first.
func ListPipelineTrash(dir string) []PipelineTrashInfo {
	return pipelineStore.listTrash(dir)
}

// RestorePipeline moves trashed pipeline id back out of .trash, as to —
// or under its old name when to is "" — never replacing one.
func RestorePipeline(dir, id, to string) (string, error) {
	return pipelineStore.restore(dir, id, to)
}
