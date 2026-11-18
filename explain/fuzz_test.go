package explain

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The explain parsers read text a server wrote, or a result that only looks
// like a plan; a panic in one is a panic in the TUI's event loop or a dbc
// web handler (N-302). Each target hands a parser fuzzed input and wants no
// panic from it nor from what then reads the plan: the text tree (with its
// findings), Mermaid and the JSON document. The picture is left out: it
// draws an image per run, too slow for a fuzz loop, and reads only what
// Text does.
//
// Run at length by hand, one at a time:
//
//	go test -run '^$' -fuzz '^FuzzParsePostgresJSON$' -fuzztime 5m ./explain
//
// and likewise FuzzParsePostgresText, FuzzParseMySQLTree and
// FuzzParseTabular.

// watchdog panics — which ends the fuzz worker, so the fuzzer keeps the
// input as a failure — when one input takes longer than limit. Go's
// fuzzer has no per-input timeout: an input that hangs a parser only
// shows as the exec rate falling to zero.
func watchdog(limit time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(limit):
			panic(fmt.Sprintf("one input took over %s", limit))
		}
	}()
	return func() { close(done) }
}

// watchLimit is generous: an input is microseconds, and the fuzzer's own
// minimizing and a loaded machine stretch some; only a real hang gets near.
const watchLimit = 10 * time.Second

// usePlan runs what a parsed plan goes through after the parser.
func usePlan(t *testing.T, p *Plan) {
	t.Helper()
	if p == nil {
		return
	}
	p.Finalize()
	_ = p.Text(TextOptions{Width: 80, Insights: true})
	_ = p.Text(TextOptions{Width: 10, Color: true})
	_ = p.Mermaid()
	_, _ = p.Findings()
	if _, err := p.JSON(); err != nil {
		t.Fatalf("a parsed plan does not encode: %v", err)
	}
	p.ResolveAliases("SELECT * FROM orders o JOIN users u ON u.id = o.user_id")
}

// seeds adds every testdata fixture whose name matches one of the globs.
func seeds(f *testing.F, globs ...string) [][]byte {
	var out [][]byte
	for _, g := range globs {
		names, _ := filepath.Glob(filepath.Join("testdata", g))
		for _, n := range names {
			if b, err := os.ReadFile(n); err == nil {
				out = append(out, b)
			}
		}
	}
	return out
}

func FuzzParsePostgresJSON(f *testing.F) {
	for _, b := range seeds(f, "pg_*.json", "my_json_*.json") {
		f.Add(b)
	}
	f.Add([]byte(`[{"Plan": {"Node Type": "Seq Scan", "Plans": [{}]}}]`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		defer watchdog(watchLimit)()
		p, err := ParsePostgresJSON(data)
		if err == nil {
			usePlan(t, p)
		}
	})
}

func FuzzParsePostgresText(f *testing.F) {
	for _, b := range seeds(f, "pg_*.txt") {
		f.Add(string(b), false)
		f.Add(string(b), true)
	}
	f.Add("Seq Scan on t  (cost=0.00..1.00 rows=1 width=4)\n  ->  Index Scan", false)
	f.Fuzz(func(t *testing.T, text string, bytdb bool) {
		defer watchdog(watchLimit)()
		eng := Postgres
		if bytdb {
			eng = Bytdb
		}
		p, err := ParsePostgresText(strings.Split(text, "\n"), eng)
		if err == nil {
			usePlan(t, p)
		}
	})
}

func FuzzParseMySQLTree(f *testing.F) {
	for _, b := range seeds(f, "my_tree_*.txt", "my_analyze_*.txt") {
		f.Add(string(b))
	}
	f.Add("-> Filter: (t.a > 1)  (cost=1 rows=2)\n    -> Table scan on t")
	f.Fuzz(func(t *testing.T, text string) {
		defer watchdog(watchLimit)()
		p, err := ParseMySQLTree(text)
		if err == nil {
			usePlan(t, p)
		}
	})
}

// FuzzParseTabular feeds the two parsers of a plan as a result table —
// MySQL's classic EXPLAIN and SQLite's EXPLAIN QUERY PLAN — the same
// fuzzed table: lines of tab-separated cells, the first line the columns.
func FuzzParseTabular(f *testing.F) {
	for _, b := range seeds(f, "my_tabular_*.tsv") {
		f.Add(string(b))
	}
	f.Add("id\tparent\tnotused\tdetail\n2\t0\t0\tSCAN t\n3\t2\t0\tSEARCH u USING INDEX i (a=?)")
	f.Fuzz(func(t *testing.T, text string) {
		defer watchdog(watchLimit)()
		lines := strings.Split(text, "\n")
		cols := strings.Split(lines[0], "\t")
		var rows [][]string
		for _, l := range lines[1:] {
			rows = append(rows, strings.Split(l, "\t"))
		}
		if p, err := ParseMySQLTable(cols, rows); err == nil {
			usePlan(t, p)
		}
		if p, err := ParseSQLite(cols, rows); err == nil {
			usePlan(t, p)
		}
	})
}
