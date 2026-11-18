package sqlsplit

import "testing"

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
// than timed, so it runs in the ordinary suite and under -race. The
// ratios are flat in the buffer's size — measured 2026-11-18 on 50 KB,
// 200 KB and 800 KB:
//
//	Split  0.50 allocs a statement   (the statement list's growth, a text per statement)
//	Lex    0.31 allocs a token
//	Folds  4.0  allocs a statement   (a Split and a Lex of its own, then its scan)
//
// Each budget is about 1.5× that: room for noise and small changes, none
// for a pass that starts allocating per token, per line or per byte.
// Folds' is the loose one; N-338 is to make it cheaper, and then this.
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
		{"Lex", toks, "token", 0.45, func() { Postgres.Lex(sql) }},
		{"Folds", stmts, "statement", 6, func() { Postgres.Folds(sql) }},
	} {
		if got := testing.AllocsPerRun(3, c.run) / c.per; got > c.budget {
			t.Errorf("%s: %.2f allocations a %s, over its budget of %.2f", c.name, got, c.unit, c.budget)
		}
	}
}
