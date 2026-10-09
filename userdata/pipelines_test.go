package userdata

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The pipelines store: list with the spec's own description and fragment
// count, save with the revision protocol, rename, trash and restore.
func TestPipelinesStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pipelines")
	spec := `{"name": "x", "desc": "Does X.", "fragments": [{"name": "a", "nodes": []}, {"name": "b", "nodes": []}]}`
	rev, conflict, err := SavePipeline(dir, "x.json", spec, "")
	if err != nil || conflict || rev == "" {
		t.Fatalf("create: rev %q conflict %v err %v", rev, conflict, err)
	}
	if _, _, err = SavePipeline(dir, "x.json", spec, ""); !errors.Is(err, ErrPipelineExists) {
		t.Errorf("second create: %v", err)
	}
	if _, _, err = SavePipeline(dir, "../x.json", spec, ""); !errors.Is(err, ErrBadPipelineName) {
		t.Errorf("bad name: %v", err)
	}
	infos, err := ListPipelines(dir)
	if err != nil || len(infos) != 1 || infos[0].Desc != "Does X." || infos[0].Fragments != 2 || infos[0].Rev != rev {
		t.Fatalf("list: %+v %v", infos, err)
	}
	// a save from the current revision writes; from a stale one conflicts
	rev2, conflict, err := SavePipeline(dir, "x.json", spec+"\n", rev)
	if err != nil || conflict || rev2 == rev {
		t.Fatalf("save: rev %q conflict %v err %v", rev2, conflict, err)
	}
	cur, conflict, err := SavePipeline(dir, "x.json", "other", rev)
	if err != nil || !conflict || cur != rev2 {
		t.Errorf("stale save: cur %q conflict %v err %v", cur, conflict, err)
	}
	text, got, err := ReadPipeline(dir, "x.json")
	if err != nil || got != rev2 || text != spec+"\n" {
		t.Errorf("read: %q %v", got, err)
	}
	if err = RenamePipeline(dir, "x.json", "y.json"); err != nil {
		t.Fatal(err)
	}
	id, err := TrashPipeline(dir, "y.json")
	if err != nil {
		t.Fatal(err)
	}
	if infos, _ = ListPipelines(dir); len(infos) != 0 {
		t.Errorf("after trash: %+v", infos)
	}
	if tr := ListPipelineTrash(dir); len(tr) != 1 || tr[0].ID != id || tr[0].Name != "y.json" {
		t.Errorf("trash: %+v", tr)
	}
	name, err := RestorePipeline(dir, id, "")
	if err != nil || name != "y.json" {
		t.Fatalf("restore: %q %v", name, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "y.json")); err != nil {
		t.Error(err)
	}
	// a missing directory lists nothing
	if infos, err = ListPipelines(filepath.Join(dir, "nope")); err != nil || len(infos) != 0 {
		t.Errorf("missing dir: %v %v", infos, err)
	}
}
