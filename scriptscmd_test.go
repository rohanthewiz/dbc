package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/userdata"
)

// `dbc scripts -t json` is for tooling: one array of objects, size a number
// and modified a timestamp, not the table's display strings.
func TestScriptsListJSON(t *testing.T) {
	mod := time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC)
	r := scriptsResult([]userdata.ScriptInfo{{Name: "copy_it.go", Desc: "Copy a table.", Size: 42, Mod: mod}})
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
	if len(got) != 1 || got[0]["name"] != "copy_it.go" || got[0]["size"] != float64(42) ||
		got[0]["modified"] != "2026-10-06T18:30:00Z" || got[0]["description"] != "Copy a table." {
		t.Errorf("got %v", got)
	}
}
