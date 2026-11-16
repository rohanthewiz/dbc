package sqlsplit

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// Deep inputs for the formatter's cost (N-281): the shapes generated SQL
// takes, nested n levels.

// leftOr is an ORM's left-nested disjunction: ((a = 0 OR a = 1) OR a = 2) …
func leftOr(n int) string {
	var b strings.Builder
	b.WriteString("SELECT * FROM t WHERE ")
	b.WriteString(strings.Repeat("(", n))
	b.WriteString("a = 0")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, " OR a = %d)", i)
	}
	return b.String()
}

// rightOr nests the other way: a = 0 OR (a = 1 OR (a = 2 OR …)).
func rightOr(n int) string {
	var b strings.Builder
	b.WriteString("SELECT * FROM t WHERE ")
	for i := range n {
		fmt.Fprintf(&b, "a = %d OR (", i)
	}
	fmt.Fprintf(&b, "a = %d", n)
	b.WriteString(strings.Repeat(")", n))
	return b.String()
}

// parens is n brackets around one value: SELECT ((((1)))).
func parens(n int) string {
	return "SELECT " + strings.Repeat("(", n) + "1" + strings.Repeat(")", n)
}

// calls is n nested two-argument calls, which break into blocks:
// SELECT f(0, f(1, f(2, …))).
func calls(n int) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	for i := range n {
		fmt.Fprintf(&b, "f(%d, ", i)
	}
	b.WriteString("0")
	b.WriteString(strings.Repeat(")", n))
	return b.String()
}

// BenchmarkFormatDeep formats generated SQL nested n levels. Before N-281
// each level that did not fit measured its whole subtree on one line (try),
// so the cost was the input's size times its depth.
func BenchmarkFormatDeep(b *testing.B) {
	for _, c := range []struct {
		name string
		gen  func(int) string
		ns   []int
	}{
		{"left-or", leftOr, []int{1000, 5000, 20000}},
		{"right-or", rightOr, []int{1000, 5000}},
		{"parens", parens, []int{10000, 100000}},
		{"calls", calls, []int{1000}},
	} {
		for _, n := range c.ns {
			sql := c.gen(n)
			b.Run(fmt.Sprintf("%s-%d", c.name, n), func(b *testing.B) {
				b.SetBytes(int64(len(sql)))
				for b.Loop() {
					Format(sql)
				}
			})
		}
	}
}

// Formatting costs what the input holds, however deeply it nests (N-281).
// Counted in bytes allocated per byte of input, which unlike time does not
// vary with the machine. Before, a level that did not fit measured its whole
// subtree, so the bytes grew with size × depth: 31 MB for a 1,000-deep
// left-nested OR chain, 774 MB at 5,000 (11,000 bytes per input byte, and
// growing with the depth). Now the measurement stops at the line's end.
func TestFormatCostsWhatItHolds(t *testing.T) {
	const depth = 8000
	const perByte = 2000 // the bound; a few hundred now
	for _, c := range []struct {
		name string
		gen  func(int) string
	}{{"left-or", leftOr}, {"right-or", rightOr}, {"parens", parens}} {
		sql := c.gen(depth)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		Format(sql)
		runtime.ReadMemStats(&after)
		got := float64(after.TotalAlloc-before.TotalAlloc) / float64(len(sql))
		if got > perByte {
			t.Errorf("%s %d deep: %.0f bytes allocated per input byte, want at most %d", c.name, depth, got, perByte)
		}
		t.Logf("%s %d deep (%d KB): %.0f bytes per input byte", c.name, depth, len(sql)/1024, got)
	}
}
