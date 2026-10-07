package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/userdata"
)

// `dbc scripts -t json` is for tooling: one array of objects, size a number
// and modified a timestamp, not the table's display strings. The user's
// scripts come first, then the examples, told apart by kind; an example
// has no file, so its modified is null.
func TestScriptsListJSON(t *testing.T) {
	mod := time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC)
	ex, _ := scripts.ExampleByName("loop_params.go")
	r := scriptsResult([]userdata.ScriptInfo{{Name: "copy_it.go", Desc: "Copy a table.", Size: 42, Mod: mod}},
		[]scripts.Example{ex})
	out := filepath.Join(t.TempDir(), "s.json")
	setFlag(t, &flagOut, out)
	emit([]*model.Result{r}, export.JSON)

	bs, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err = json.Unmarshal(bs, &got); err != nil {
		t.Fatalf("not one JSON array: %v\n%s", err, bs)
	}
	if len(got) != 2 || got[0]["name"] != "copy_it.go" || got[0]["size"] != float64(42) ||
		got[0]["modified"] != "2026-10-06T18:30:00Z" || got[0]["description"] != "Copy a table." ||
		got[0]["kind"] != "script" {
		t.Fatalf("got %v", got)
	}
	if e := got[1]; e["name"] != "loop_params.go" || e["kind"] != "example" || e["modified"] != nil ||
		e["size"] != float64(len(ex.Text)) || e["description"] != ex.Desc || ex.Desc == "" {
		t.Errorf("example row = %v", e)
	}
}

// An example a script of the user's shadows is not listed: `dbc script
// NAME` would run the user's, so the example's row could never be reached.
func TestReachableExamples(t *testing.T) {
	all := scripts.Examples()
	got := reachableExamples([]userdata.ScriptInfo{{Name: "loop_params.go"}, {Name: "mine.go"}})
	if len(got) != len(all)-1 {
		t.Fatalf("%d examples reachable, want %d (all but loop_params.go)", len(got), len(all)-1)
	}
	for _, ex := range got {
		if ex.Name == "loop_params.go" {
			t.Error("the shadowed loop_params.go example is listed")
		}
	}
}
