package script

import (
	"os"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/scripts"
)

// The templates that need no particular table run as they are, filled with
// the test's SQLite connections: a new script works before it is edited.
// (copy names public.orders, which has to exist; Check covers it in
// scripts' own tests.)
func TestTemplatesRun(t *testing.T) {
	t.Chdir(t.TempDir()) // export writes report.csv and report.html here
	for _, name := range []string{"blank", "query", "loop", "export"} {
		src, ok := scripts.Fill(name, []string{"a", "b"})
		if !ok {
			t.Fatalf("no template %s", name)
		}
		_, printed, err := runScript(t, src)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		t.Logf("%s: %s", name, strings.Join(printed, " | "))
	}
	for _, f := range []string{"report.csv", "report.html"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("export template wrote no %s", f)
		}
	}
}
