package codehl

import (
	"strings"
	"testing"
)

// classes renders src with each span's text tagged by its kind, so a test
// reads as the highlighted line: K[func] main() S["x"].
func classes(tag, src string) string {
	var b strings.Builder
	at := 0
	for _, s := range Lex(tag, src) {
		b.WriteString(src[at:s.Start])
		b.WriteString("KSNCIP"[s.Kind-1 : s.Kind])
		b.WriteString("[" + src[s.Start:s.End] + "]")
		at = s.End
	}
	b.WriteString(src[at:])
	return b.String()
}

func TestLex(t *testing.T) {
	cases := []struct{ tag, src, want string }{
		// SQL goes through sqlsplit, so idents and params keep their kinds
		{"sql", `SELECT "id" FROM t WHERE x = $1 -- hi`,
			`K[SELECT] I["id"] K[FROM] t K[WHERE] x = P[$1] C[-- hi]`},
		{"", `select 1`, `K[select] N[1]`}, // untagged is SQL

		{"go", "func main() { s := \"a\\\"b\" // c\n}",
			"K[func] main() { s := S[\"a\\\"b\"] C[// c]\n}"},
		{"golang", "x := `raw\n\\n` + 'r' + 0x1F + 1.5e-3",
			"x := S[`raw\n\\n`] + S['r'] + N[0x1F] + N[1.5e-3]"},
		{"go", "/* a\nb */ return nil", "C[/* a\nb */] K[return] K[nil]"},
		{"go", "v2 := x.type", "v2 := x.type"}, // digits in a word; member access

		{"python", "def f(x):  # c\n    return None", "K[def] f(x):  C[# c]\n    K[return] K[None]"},
		{"py", `r"\d+" + b'x' + f"{y}"`, `S[r"\d+"] + S[b'x'] + S[f"{y}"]`},
		{"python", "s = '''a\n'b'\n''' + 3j", "s = S['''a\n'b'\n'''] + N[3j]"},
		{"python", "rb = 1", "rb = N[1]"}, // a prefix-shaped name with no quote

		{"js", "const $a = `t ${x}\n` // c", "K[const] $a = S[`t ${x}\n`] C[// c]"},
		{"javascript", "p.catch(e => [...this]); 10n", "p.catch(e => [...K[this]]); N[10n]"},
		{"js", "x = 'open\ny = 2", "x = S['open]\ny = N[2]"}, // unterminated stops at the newline

		{"json", `{"a": [1, -2.5e3, true, null], "b": "x\"y"} // c`,
			`{S["a"]: [N[1], -N[2.5e3], K[true], K[null]], S["b"]: S["x\"y"]} C[// c]`},
		{"", "\n  [{\"id\": 1}]", "\n  [{S[\"id\"]: N[1]}]"}, // untagged JSON is sniffed
		{"", `select '{"a":1}'`, `K[select] S['{"a":1}']`},   // …but not inside SQL

		{"bash", "if [ -f \"$F\" ]; then echo $HOME ${X:-1} $? # c\nfi",
			"K[if] [ -f S[\"$F\"] ]; K[then] echo P[$HOME] P[${X:-1}] P[$?] C[# c]\nK[fi]"},
		{"sh", "psql --if-exists ./for.sh a#b ${#arr} 2>&1 -p 5432",
			"psql --if-exists ./for.sh a#b P[${#arr}] 2>&1 -p 5432"},
		{"shell", "psql -c \"\n  SELECT 1;\n\" 'it''s'", "psql -c S[\"\n  SELECT 1;\n\"] S['it']S['s']"},
		{"console", "$ psql\nit doesn't work\nok", "$ psql\nit doesnS['t work]\nok"}, // stray apostrophe: one line
		{"bash", "psql <<'SQL' | tee out\nSELECT 'x';\n  SQL\necho done", "psql <<S['SQL'] | tee out\nS[SELECT 'x';\n  SQL]\necho K[done]"},
		{"zsh", "x=$((1<<2)); cat <<<word", "x=$((1<<2)); cat <<<word"},

		{"rust", "fn main() {}", "fn main() {}"}, // not highlighted
	}
	for _, c := range cases {
		if got := classes(c.tag, c.src); got != c.want {
			t.Errorf("Lex(%q, %q)\n got %q\nwant %q", c.tag, c.src, got, c.want)
		}
	}
}

// Spans must start and end on rune boundaries and never overlap: the TUI
// slices wrapped rows by them, and a cut mid-rune would draw garbage.
func TestLexSpansAreWellFormed(t *testing.T) {
	src := "naïve := \"ünï\" // ✓\nπ2 = 'é' # ü\nconst ñ = `ç`"
	for _, tag := range []string{"go", "python", "js", "sql", "json", "shell"} {
		end := 0
		for _, s := range Lex(tag, src) {
			if s.Start < end || s.End <= s.Start || s.End > len(src) {
				t.Fatalf("%s: bad span %+v after %d", tag, s, end)
			}
			for _, o := range []int{s.Start, s.End} {
				if o < len(src) && src[o]&0xC0 == 0x80 {
					t.Fatalf("%s: span %+v cuts a rune", tag, s)
				}
			}
			end = s.End
		}
	}
}

func TestLang(t *testing.T) {
	for tag, want := range map[string]string{
		"": "sql", "PostgreSQL": "sql", "golang": "go", "py": "python",
		"jsx": "js", "Python3": "python", "text": "", "ts": "",
		"JSONC": "json", "bash": "shell", "console": "shell",
	} {
		if got := Lang(tag); got != want {
			t.Errorf("Lang(%q) = %q, want %q", tag, got, want)
		}
	}
}
