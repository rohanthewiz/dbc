package userdata

import (
	"encoding/json"
	"errors"
	"time"
)

// Jobs: the JSON specs in the jobs directory (config.Config.JobsDir,
// absolute) — DAGs of pipelines with triggers — kept by the specs store
// (specstore.go) exactly as pipelines are:
//
//	<jobs dir>/
//	    nightly.json
//	    .trash/
//	        weekly.1759769402123.json

// JobInfo is one job, as a list shows it.
type JobInfo struct {
	Name      string    `json:"name"`      // file name, with .json
	Desc      string    `json:"desc"`      // the spec's desc; "" if none or unreadable
	Pipelines int       `json:"pipelines"` // how many steps; 0 when the file does not parse
	Schedule  []string  `json:"schedule"`  // its cron lines, as written
	Webhook   bool      `json:"webhook"`
	Size      int64     `json:"size"`
	Mod       time.Time `json:"mod"`
	Rev       string    `json:"rev"`
}

// ErrJobExists is a create, rename or restore onto a name the directory
// already has; ErrBadJobName a name ValidJobName refuses.
var (
	ErrJobExists  = errors.New("a job by that name already exists")
	ErrBadJobName = errors.New("not a job name: letters, digits, . _ - ending in .json")
)

// MaxJobBytes bounds what a save writes.
const MaxJobBytes = 1 << 20

var jobStore = specStore{what: "job", exists: ErrJobExists, badName: ErrBadJobName, maxBytes: MaxJobBytes}

// ValidJobName reports whether name may name a job: the console-name rule
// for the stem, plus a required .json suffix.
func ValidJobName(name string) bool { return validSpecName(name) }

// jobHead is what a listing reads off a job, without package jobs
// (userdata is below it).
type jobHead struct {
	Desc      string            `json:"desc"`
	Pipelines []json.RawMessage `json:"pipelines"`
	Triggers  struct {
		Schedule []string `json:"schedule"`
		Webhook  bool     `json:"webhook"`
	} `json:"triggers"`
}

// ListJobs lists the jobs in dir by name. A missing dir is none.
func ListJobs(dir string) ([]JobInfo, error) {
	files, err := jobStore.list(dir)
	if err != nil {
		return nil, err
	}
	var out []JobInfo
	for _, f := range files {
		var head jobHead
		_ = json.Unmarshal(f.Src, &head)
		out = append(out, JobInfo{Name: f.Name, Desc: head.Desc, Pipelines: len(head.Pipelines),
			Schedule: head.Triggers.Schedule, Webhook: head.Triggers.Webhook, Size: f.Size, Mod: f.Mod, Rev: f.Rev})
	}
	return out, nil
}

// ReadJob returns a job's text and revision; a missing one errors.Is
// fs.ErrNotExist.
func ReadJob(dir, name string) (text, rev string, err error) { return jobStore.read(dir, name) }

// SaveJob writes text as job name, made from revision base (SavePipeline's
// rules).
func SaveJob(dir, name, text, base string) (rev string, conflict bool, err error) {
	return jobStore.save(dir, name, text, base)
}

// RenameJob renames job from to to, refusing to replace another.
func RenameJob(dir, from, to string) error { return jobStore.rename(dir, from, to) }

// TrashJob moves a job into .trash and returns its trash ID.
func TrashJob(dir, name string) (string, error) { return jobStore.trash(dir, name) }

// ListJobTrash lists the trashed jobs in dir, newest first.
func ListJobTrash(dir string) []SpecTrashInfo { return jobStore.listTrash(dir) }

// RestoreJob moves trashed job id back out of .trash, as to — or under its
// old name when to is "" — never replacing one.
func RestoreJob(dir, id, to string) (string, error) { return jobStore.restore(dir, id, to) }
