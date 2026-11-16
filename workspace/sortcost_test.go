package workspace

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
)

// bigSortable is n rows of a numeric column, a mixed-case text column (the
// case that makes a case-insensitive compare allocate), and a column of
// mostly NULLs. The values are a fixed scramble of the row number, so runs
// compare and the sort has real work to do.
func bigSortable(n int) *model.Result {
	r := &model.Result{Columns: []string{"id", "name", "note"}}
	r.Rows = make([][]string, n)
	r.Raw = make([][]any, n)
	for i := range n {
		k := (i*7919 + 13) % n
		name := "Customer " + strconv.Itoa(k%5000) + " of Region " + strconv.Itoa(k%37)
		var note any
		noteText := "NULL"
		if k%5 == 0 {
			noteText = "Note " + strconv.Itoa(k)
			note = noteText
		}
		r.Rows[i] = []string{strconv.Itoa(k), name, noteText}
		r.Raw[i] = []any{int64(k), name, note}
	}
	return r
}

// BenchmarkSortRows is one header click on a large result: the TUI runs
// it on its event loop, dbc web on every request for a sorted page.
func BenchmarkSortRows(b *testing.B) {
	for _, n := range []int{100_000, 1_000_000} {
		r := bigSortable(n)
		numeric := export.NumericColumns(r)
		order := make([]int, n)
		for _, c := range []struct {
			name string
			col  int
		}{{"numeric", 0}, {"text", 1}, {"nulls", 2}} {
			b.Run(strconv.Itoa(n)+"/"+c.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					SortRows(order, r, c.col, false, numeric)
				}
			})
		}
	}
}

// oldSortRows is SortRows as it was before N-282: a stable sort comparing
// lower-cased text and reflected numbers on every comparison. The new one
// must put every row in the same place.
func oldSortRows(order []int, r *model.Result, col int, desc bool, numeric []bool) {
	for i := range order {
		order[i] = i
	}
	if r == nil || col < 0 || col >= len(r.Columns) {
		return
	}
	num := col < len(numeric) && numeric[col]
	raw := func(ri int) any {
		if ri < len(r.Raw) && col < len(r.Raw[ri]) {
			return r.Raw[ri][col]
		}
		return nil
	}
	slices.SortStableFunc(order, func(a, b int) int {
		ra, rb := raw(a), raw(b)
		switch {
		case ra == nil && rb == nil:
			return 0
		case ra == nil:
			return 1
		case rb == nil:
			return -1
		}
		var c int
		if num {
			c = cmp.Compare(toFloat(ra), toFloat(rb))
		} else {
			c = cmp.Compare(strings.ToLower(r.Rows[a][col]), strings.ToLower(r.Rows[b][col]))
		}
		if desc {
			c = -c
		}
		return c
	})
}

// The keyed sort orders every row as the old stable one did: ties (many,
// from a small alphabet), NULLs, mixed case, numbers of several Go types,
// NaN, and rows whose Raw stops short.
func TestSortRowsMatchesOld(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	words := []string{"apple", "Apple", "APPLE", "banana", "Banana", "ß", "SS", "é", "É", "", "z"}
	for trial := range 40 {
		n := 1 + rng.IntN(400)
		r := &model.Result{Columns: []string{"i", "f", "t", "mixed"}}
		for range n {
			var vals []any
			switch rng.IntN(4) {
			case 0:
				vals = []any{nil, nil, nil, nil}
			default:
				f := float64(rng.IntN(7)) / 2
				if rng.IntN(20) == 0 {
					f = math.NaN()
				}
				var m any = int32(rng.IntN(5))
				if rng.IntN(2) == 0 {
					m = float32(rng.IntN(5)) / 4
				}
				vals = []any{int64(rng.IntN(9) - 4), f, words[rng.IntN(len(words))], m}
				if rng.IntN(6) == 0 {
					vals[rng.IntN(4)] = nil
				}
			}
			text := make([]string, len(vals))
			for i, v := range vals {
				text[i] = "NULL"
				if v != nil {
					text[i] = fmt.Sprint(v)
				}
			}
			if trial%5 == 0 && rng.IntN(10) == 0 {
				vals = vals[:2] // Raw stops short: the rest read as NULL
			}
			r.Rows = append(r.Rows, text)
			r.Raw = append(r.Raw, vals)
		}
		numeric := export.NumericColumns(r)
		for col := -1; col < 5; col++ {
			for _, desc := range []bool{false, true} {
				want, got := make([]int, n), make([]int, n)
				oldSortRows(want, r, col, desc, numeric)
				SortRows(got, r, col, desc, numeric)
				if !slices.Equal(got, want) {
					t.Fatalf("trial %d col %d desc %v:\n got %v\nwant %v", trial, col, desc, got, want)
				}
			}
		}
	}
}
