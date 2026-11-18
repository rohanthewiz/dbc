package sqlsplit

import (
	"strings"
	"testing"
)

// BenchmarkLexBig and BenchmarkFoldsBig are the editor's other two passes
// over a whole buffer (N-303): the highlighter's token stream and the
// fold ranges, on the 3.5 MB migration BenchmarkSplitBig splits. Measured
// 2026-11-10 at 20 and 45 ms, before the RPU plan's phase 3.
func BenchmarkLexBig(b *testing.B) {
	sql := bigMigration(3_500_000)
	var n int
	for b.Loop() {
		n = len(Postgres.Lex(sql))
	}
	b.ReportMetric(float64(n), "tokens")
}

func BenchmarkFoldsBig(b *testing.B) {
	sql := bigMigration(3_500_000)
	var n int
	for b.Loop() {
		n = len(Postgres.Folds(sql))
	}
	b.ReportMetric(float64(n), "folds")
}

// TestScanCostsWhatItHolds is a budget (N-303): the allocations of Split,
// Lex and Folds per statement or per token of a migration, counted rather
// than timed, so it runs in the ordinary suite and under -race. Measured
// 2026-11-18 on 50 KB, 200 KB and 800 KB:
//
//	Split  0.50 allocs a statement   (the statement list's growth, a text per statement)
//	Lex    0.001 a token             (the token list's growth alone, since N-338;
//	                                  0.31 before, a lowered copy of each capitalized word)
//	Folds  0.52 a statement          (its Split's, and the few spans it keeps; 4.0
//	                                  before N-338, when it built every token too)
//
// Each budget is about 1.5× that (Lex's, a count that only grows with the
// log of the buffer, has more room): enough for noise and small changes,
// none for a pass that starts allocating per token, per line or per byte.
func TestScanCostsWhatItHolds(t *testing.T) {
	sql := bigMigration(200_000)
	stmts := float64(len(Postgres.Split(sql)))
	toks := float64(len(Postgres.Lex(sql)))
	for _, c := range []struct {
		name   string
		per    float64 // what the count is divided by
		unit   string
		budget float64
		run    func()
	}{
		{"Split", stmts, "statement", 0.75, func() { Postgres.Split(sql) }},
		{"Lex", toks, "token", 0.01, func() { Postgres.Lex(sql) }},
		{"Folds", stmts, "statement", 0.8, func() { Postgres.Folds(sql) }},
	} {
		if got := testing.AllocsPerRun(3, c.run) / c.per; got > c.budget {
			t.Errorf("%s: %.2f allocations a %s, over its budget of %.2f", c.name, got, c.unit, c.budget)
		}
	}
}

// isKeyword lowers a word into a fixed buffer (N-338); every keyword must
// fit it, or that keyword would never be found.
func TestKeywordsFitTheLookup(t *testing.T) {
	for w := range keywords {
		if !isKeyword(w) || !isKeyword(strings.ToUpper(w)) {
			t.Errorf("keyword %q is not found by isKeyword", w)
		}
	}
	if isKeyword("selects") || isKeyword("") {
		t.Error("a word off the list read as a keyword")
	}
}
