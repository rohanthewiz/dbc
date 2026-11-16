package sqlsplit

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

// editFrags is what the random edits type: every construct whose end the
// scan has to find (strings, comments, dollar bodies, BEGIN … END bodies,
// MySQL's DELIMITER and its terminators), the pieces that open or close
// one alone, a byte order mark, and bare newlines and semicolons.
var editFrags = []string{
	";", "\n", " ", "'", "\"", "`", "/*", "*/", "--", "-- x\n", "#", "$$", "$q$", "\\",
	"SELECT 1;", "SELECT 'a;b';\n", "BEGIN", "END;", "END", "(", ")", "ATOMIC",
	"CREATE TRIGGER t AFTER INSERT ON o FOR EACH ROW BEGIN\n  UPDATE s SET n = n + 1;\n",
	"CREATE FUNCTION f() RETURNS int BEGIN ATOMIC SELECT 1; END;\n",
	"CREATE RULE r AS ON INSERT TO t DO ALSO (INSERT INTO a VALUES (1); INSERT INTO b VALUES (2));",
	"DELIMITER //\n", "DELIMITER ;\n", "delimiter $$\n", "//", "\nDELIMITER ;",
	"\uFEFF", "\r\n", "/*!40101 SET x=1 */;", "SELECT $$x; y$$;", "E'it\\'s;'",
	"-- head\n\n", "; -- remark\n", "/* block\n*/ ",
}

// splitBase is a buffer with a little of everything, for the edits to
// break and mend.
func splitBase(rng *rand.Rand) string {
	var b strings.Builder
	for range 6 + rng.IntN(20) {
		switch rng.IntN(6) {
		case 0:
			b.WriteString(editFrags[rng.IntN(len(editFrags))])
		case 1:
			fmt.Fprintf(&b, "-- step %d\nCREATE TABLE t%d (id int, note text default 'x;y');\n", rng.IntN(9), rng.IntN(9))
		case 2:
			b.WriteString("DELIMITER //\nCREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\nEND //\nDELIMITER ;\n")
		case 3:
			b.WriteString("CREATE TRIGGER t AFTER DELETE ON o BEGIN\n  DELETE FROM i WHERE oid = old.id;\nEND;\n")
		case 4:
			b.WriteString("SELECT 1; /* a\nb */ SELECT 2;\n\n")
		default:
			b.WriteString("INSERT INTO t VALUES (1, 'a'), (2, $$b;c$$);\n")
		}
	}
	return b.String()
}

// checkSplitting holds s to a fresh split of sql: the same chunks (with
// the terminator each starts under, by its text: s's table may hold more
// than a fresh one, in another order) and the same statements.
func checkSplitting(t *testing.T, s *Splitting, sql, step string) {
	t.Helper()
	fresh := s.d.Splitting(sql)
	if got, want := spelled(s.chunks, s.delims), spelled(fresh.chunks, fresh.delims); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: chunks of %q\n got %+v\nwant %+v", step, sql, got, want)
	}
	if got, want := s.Stmts(), s.d.Split(sql); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: statements of %q\n got %+v\nwant %+v", step, sql, got, want)
	}
}

// A chain of random edits, each split by Edit from the one before, stays
// equal to a fresh split of the buffer it made, in every dialect. Edits
// land anywhere, mid-rune and mid-token included, and are mostly small (a
// keystroke's) with some large (a paste, a deleted block).
func TestSplittingEditMatchesSplit(t *testing.T) {
	for seed := range uint64(60) {
		rng := rand.New(rand.NewPCG(seed, 279))
		d := Dialect(seed % uint64(Bytdb+1))
		sql := splitBase(rng)
		s := d.Splitting(sql)
		checkSplitting(t, s, sql, fmt.Sprintf("seed %d base", seed))
		for i := range 300 {
			at := rng.IntN(len(sql) + 1)
			del := 0
			switch rng.IntN(4) {
			case 0:
				del = rng.IntN(len(sql) - at + 1) // anything up to the end
			case 1:
				del = min(rng.IntN(3), len(sql)-at)
			}
			ins := ""
			if rng.IntN(3) != 0 {
				ins = editFrags[rng.IntN(len(editFrags))]
			}
			next := sql[:at] + ins + sql[at+del:]
			s = s.Edit(next, at, at+del, at+len(ins))
			sql = next
			checkSplitting(t, s, sql, fmt.Sprintf("seed %d (%v) step %d: [%d,%d) → %q", seed, d, i, at, at+del, ins))
		}
	}
}

// spelledChunk is a chunk with its terminator as text.
type spelledChunk struct {
	c     chunk
	delim string
}

func spelled(cs []chunk, tab delimTable) []spelledChunk {
	out := make([]spelledChunk, len(cs))
	for i, c := range cs {
		out[i] = spelledChunk{delim: tab[c.delim]}
		c.delim = 0
		out[i].c = c
	}
	return out
}

// Two edits of one Splitting, each meeting a terminator the other does
// not: neither sees the other's in its table (Edit appends to a copy).
// The old table holds three terminators, so its slice has room for a
// fourth: shared, both edits would write their own into that one slot.
func TestSplittingEditsBranch(t *testing.T) {
	old := "DELIMITER //\nSELECT 1 //\nDELIMITER $$\nSELECT 5 $$\nDELIMITER ;\nSELECT 2;\n"
	s := MySQL.Splitting(old)
	if len(s.delims) != 3 || cap(s.delims) == len(s.delims) {
		t.Fatalf("the table is %q (cap %d): want three with room to spare", s.delims, cap(s.delims))
	}
	at := len(old)
	a := old + "DELIMITER @@\nSELECT 3 @@\n"
	b := old + "DELIMITER %%\nSELECT 4 %%\n"
	sa := s.Edit(a, at, at, len(a))
	sb := s.Edit(b, at, at, len(b))
	checkSplitting(t, sa, a, "branch a")
	checkSplitting(t, sb, b, "branch b")
	checkSplitting(t, s, old, "the original")
}

// An edit that does not fit the buffer s was made for is split whole
// rather than trusted.
func TestSplittingEditOfAnotherBuffer(t *testing.T) {
	s := Postgres.Splitting("SELECT 1; SELECT 2;")
	sql := "SELECT 3;"
	checkSplitting(t, s.Edit(sql, 0, 2, 2), sql, "length mismatch")
	checkSplitting(t, s.Edit(sql, 5, 2, 2), sql, "at past the ends")
}

// The kept statements' Text is sliced from the new buffer: one sliced from
// the old would hold that whole buffer alive for as long as the statement
// is kept — a version of a 3.5 MB buffer for every keystroke.
func TestSplittingEditSlicesTheNewBuffer(t *testing.T) {
	old := strings.Repeat("SELECT 1;\n", 8)
	s := Postgres.Splitting(old)
	// a statement in the middle grows: the ones before it are kept, the
	// ones after it moved
	at := 4*10 + 8
	sql := old[:at] + "2" + old[at:]
	s = s.Edit(sql, at, at, at+1)
	checkSplitting(t, s, sql, "middle edit")
	for i, st := range s.Stmts() {
		if !sameBacking(st.Text, sql) {
			t.Errorf("statement %d's Text %q is not a slice of the new buffer", i, st.Text)
		}
	}
}

// sameBacking reports whether part's bytes lie inside whole's.
func sameBacking(part, whole string) bool {
	if part == "" {
		return true
	}
	p, w := uintptr(unsafe.Pointer(unsafe.StringData(part))), uintptr(unsafe.Pointer(unsafe.StringData(whole)))
	return p >= w && p+uintptr(len(part)) <= w+uintptr(len(whole))
}

// BenchmarkSplitBig and BenchmarkSplittingEdit compare a whole split of a
// 3.5 MB migration with Edit after one typed byte in its middle.
func bigMigration(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "-- step %d\nCREATE TABLE t%d (id integer primary key, name text not null, note text default 'x;y');\n", i, i)
		fmt.Fprintf(&b, "INSERT INTO t%d (id, name) VALUES (1, 'a'), (2, 'b');\n", i)
	}
	return b.String()
}

func BenchmarkSplitBig(b *testing.B) {
	sql := bigMigration(3_500_000)
	for b.Loop() {
		Postgres.Split(sql)
	}
}

func BenchmarkSplittingEdit(b *testing.B) {
	sql := bigMigration(3_500_000)
	s := Postgres.Splitting(sql)
	at := strings.Index(sql[len(sql)/2:], "name text") + len(sql)/2
	typed := sql[:at] + "x" + sql[at:]
	for b.Loop() {
		s.Edit(typed, at, at, at+1)
	}
}

// FuzzSplitEdit holds Edit to a fresh split for any buffer and any one
// edit of it. Run at length by hand:
//
//	go test -run '^$' -fuzz '^FuzzSplitEdit$' -fuzztime 5m ./sqlsplit
func FuzzSplitEdit(f *testing.F) {
	for i, s := range []string{
		"SELECT 1; SELECT 2",
		"SELECT 1; -- remark\nSELECT 2;",
		"DELIMITER //\nCREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\nEND //\nDELIMITER ;\nSELECT 2;",
		"SELECT 1; DELIMITER //\nSELECT 2 //",
		"CREATE TRIGGER t AFTER DELETE ON o BEGIN\n  DELETE FROM i;\nEND;\nSELECT 1;",
		"\uFEFFSELECT 1;\r\nSELECT 2;",
		"SELECT $$x; y$$; /* a\n;b */ SELECT 'c;\nd';",
	} {
		for d := range Bytdb + 1 {
			f.Add(s, uint16(i*3), uint8(i%3), editFrags[i*5%len(editFrags)], uint8(d))
		}
	}
	f.Fuzz(func(t *testing.T, sql string, at uint16, del uint8, ins string, dn uint8) {
		d := Dialect(dn % uint8(Bytdb+1))
		a := int(at) % (len(sql) + 1)
		e := a + min(int(del), len(sql)-a)
		next := sql[:a] + ins + sql[e:]
		checkSplitting(t, d.Splitting(sql).Edit(next, a, e, a+len(ins)), next, fmt.Sprintf("%v [%d,%d) → %q", d, a, e, ins))
	})
}
