package main

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
)

// N-334: the config's CSV options reach every way the CLI writes CSV — a
// lone statement's streamed rows, a run collected into -o, a run's blocks
// streamed to stdout — and each writes what the collected export with the
// same options writes: one BOM at the very start, formulas guarded in the
// rows (never the header), numbers left alone.
func TestHeadlessCSVOptions(t *testing.T) {
	resetFlags(t)
	cfg := liteCfg(t)
	mgr := db.NewManager(cfg)
	defer mgr.Close()
	for _, s := range []string{
		`CREATE TABLE t (id INTEGER, note TEXT)`,
		`INSERT INTO t VALUES (-5, '=HYPERLINK("http://x")'), (3, '-5'), (7, NULL)`,
	} {
		if _, err := mgr.Run("lite", s); err != nil {
			t.Fatal(err)
		}
	}
	opts := export.CSVOptions{BOM: true, FormulaGuard: true}
	old := csvOut
	csvOut = opts
	t.Cleanup(func() { csvOut = old })

	const q = `SELECT id, note FROM t ORDER BY id`
	check := func(what, got string, blocks int) {
		t.Helper()
		if !strings.HasPrefix(got, export.BOM) || strings.Count(got, export.BOM) != 1 {
			t.Errorf("%s: want one BOM, at the start:\n%q", what, got)
		}
		if strings.Count(got, `'=HYPERLINK`) != blocks || strings.Contains(got, "'-5") {
			t.Errorf("%s: the guard is wrong:\n%q", what, got)
		}
	}

	// a lone statement: its rows streamed (rowStream), byte for byte the
	// collected export with the same options
	res, err := mgr.Run("lite", q)
	if err != nil {
		t.Fatal(err)
	}
	want, err := export.RenderWith(res, export.CSV, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := streamed(t, cfg, mgr, q, export.CSV); got != want {
		t.Errorf("streamed rows:\n got %q\nwant %q", got, want)
	}
	check("streamed rows", want, 1)

	// two statements into -o: collected (emitRun)
	got, _ := streamed(t, cfg, mgr, q+"; "+q, export.CSV)
	check("a run into -o", got, 2)

	// two statements to stdout: streamed a block at a time (blockStream)
	setFlag(t, &flagOut, "")
	out := captureStdout(t, func() {
		captureStderr(t, func() { runQueryHeadless(cfg, mgr, q+"; "+q, export.CSV) })
	})
	check("a run's blocks on stdout", out, 2)
	if out != got {
		t.Errorf("stdout and -o differ:\nstdout %q\n    -o %q", out, got)
	}
}
