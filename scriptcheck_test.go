package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/scripts"
)

// checkScripts: compiler lines for text, one JSON array for json, and bad
// only for an error (a warning alone passes).
func TestCheckScripts(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const hdr = "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\n"
	clean := write("clean.go", hdr+"func Run(s *sdb.S) error { return nil }\n")
	warn := write("warn.go", hdr+"func Run(s *sdb.S) error {\n\tm := map[string]any{}\n\tvar v any\n\tm[\"k\"], _ = v.(int)\n\treturn nil\n}\n")
	broken := write("broken.go", hdr+"func Run(s *sdb.S) error {\n\ts.Nope()\n\treturn nil\n}\n")

	var out bytes.Buffer
	if bad, err := checkScripts(refs(clean), false, &out); bad || err != nil || out.Len() != 0 {
		t.Errorf("clean: bad=%v err=%v out=%q", bad, err, out.String())
	}

	out.Reset()
	if bad, err := checkScripts(refs(clean, warn), false, &out); bad || err != nil ||
		!strings.HasPrefix(out.String(), warn+":8:2: warning: ") {
		t.Errorf("warning only: bad=%v err=%v out=%q", bad, err, out.String())
	}

	out.Reset()
	bad, err := checkScripts(refs(warn, broken), false, &out)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !bad || err != nil || len(lines) != 2 || lines[1] != broken+":6:2: undefined selector: Nope" {
		t.Errorf("an error: bad=%v err=%v lines=%q", bad, err, lines)
	}

	out.Reset()
	if _, err = checkScripts(refs(clean), true, &out); err != nil || strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("json, clean: %v %q; want []", err, out.String())
	}
	out.Reset()
	if _, err = checkScripts(refs(broken), true, &out); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err = json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 ||
		rows[0]["file"] != broken || rows[0]["line"] != 6.0 || rows[0]["severity"] != "error" {
		t.Errorf("json, broken: %v %v", err, rows)
	}

	if _, err = checkScripts(refs(filepath.Join(dir, "gone.go")), false, &out); err == nil {
		t.Error("a missing file checked without error")
	}

	// a built-in example is checked from the binary's copy (clean, as its
	// own tests require), and a diag would name it example:NAME
	ex, ok := scripts.ExampleByName("loop_params.go")
	if !ok {
		t.Fatal("no loop_params.go example")
	}
	out.Reset()
	if bad, err := checkScripts([]config.ScriptRef{{Example: &ex}}, false, &out); bad || err != nil || out.Len() != 0 {
		t.Errorf("example: bad=%v err=%v out=%q", bad, err, out.String())
	}
}

// refs are files to check, as FindScript returns them.
func refs(paths ...string) []config.ScriptRef {
	out := make([]config.ScriptRef, len(paths))
	for i, p := range paths {
		out[i] = config.ScriptRef{Path: p}
	}
	return out
}
