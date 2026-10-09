package userdata

import (
	"errors"
	"path/filepath"
	"testing"
)

// The jobs store is the pipelines store under its own words: its listing
// reads the steps, the schedule and the webhook switch.
func TestJobsStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jobs")
	spec := `{"name": "n", "desc": "Nightly.", "pipelines": [{"id": "a", "pipeline": "x"}, {"id": "b", "pipeline": "y", "after": ["a"]}],
	  "triggers": {"schedule": ["0 2 * * *"], "webhook": true}}`
	rev, _, err := SaveJob(dir, "n.json", spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = SaveJob(dir, "n.json", spec, ""); !errors.Is(err, ErrJobExists) {
		t.Errorf("second create: %v", err)
	}
	if _, _, err = SaveJob(dir, "n.go", spec, ""); !errors.Is(err, ErrBadJobName) {
		t.Errorf("bad name: %v", err)
	}
	infos, err := ListJobs(dir)
	if err != nil || len(infos) != 1 || infos[0].Pipelines != 2 || len(infos[0].Schedule) != 1 || !infos[0].Webhook || infos[0].Rev != rev {
		t.Fatalf("list: %+v %v", infos, err)
	}
	if err = RenameJob(dir, "n.json", "m.json"); err != nil {
		t.Fatal(err)
	}
	id, err := TrashJob(dir, "m.json")
	if err != nil {
		t.Fatal(err)
	}
	if tr := ListJobTrash(dir); len(tr) != 1 || tr[0].ID != id {
		t.Errorf("trash: %+v", tr)
	}
	if name, err := RestoreJob(dir, id, ""); err != nil || name != "m.json" {
		t.Errorf("restore: %q %v", name, err)
	}
}
