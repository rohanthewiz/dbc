package sqlsplit

import "testing"

func TestLexClassifies(t *testing.T) {
	sql := "SELECT id, 'a;b' AS s, \"Col\" -- note\nFROM t WHERE x = $1 AND y = ? AND z::int > 3.5 /* c */ AND n = :name; $$body$$"
	want := []struct {
		text string
		kind TokenKind
	}{
		{"SELECT", TokKeyword},
		{"'a;b'", TokString},
		{"AS", TokKeyword},
		{`"Col"`, TokIdent},
		{"-- note", TokComment},
		{"FROM", TokKeyword},
		{"WHERE", TokKeyword},
		{"$1", TokParam},
		{"AND", TokKeyword},
		{"?", TokParam},
		{"AND", TokKeyword},
		{"3.5", TokNumber},
		{"/* c */", TokComment},
		{"AND", TokKeyword},
		{":name", TokParam},
		{"$$body$$", TokString},
	}
	got := Lex(sql)
	if len(got) != len(want) {
		for _, tk := range got {
			t.Logf("%q %d", sql[tk.Start:tk.End], tk.Kind)
		}
		t.Fatalf("got %d tokens, want %d", len(got), len(want))
	}
	for i, w := range want {
		if s := sql[got[i].Start:got[i].End]; s != w.text || got[i].Kind != w.kind {
			t.Errorf("token %d = %q kind %d, want %q kind %d", i, s, got[i].Kind, w.text, w.kind)
		}
	}
}

// The highlighter and the splitter must agree on where a string ends: a
// semicolon inside quotes is drawn as string AND not split on.
func TestLexAgreesWithSplit(t *testing.T) {
	sql := "SELECT 'x;y'; SELECT 2"
	if n := len(Split(sql)); n != 2 {
		t.Fatalf("split = %d statements", n)
	}
	toks := Lex(sql)
	if sql[toks[1].Start:toks[1].End] != "'x;y'" {
		t.Errorf("string token = %q", sql[toks[1].Start:toks[1].End])
	}
}

// Digits inside identifiers are not numbers; unterminated constructs run to
// the end rather than panicking.
func TestLexEdges(t *testing.T) {
	for _, tk := range Lex("SELECT col2, t1.x FROM t9") {
		if tk.Kind == TokNumber {
			t.Errorf("identifier digits lexed as a number")
		}
	}
	for _, sql := range []string{"'open", "/* open", "$$open", `"open`, "--", ":", "$"} {
		_ = Lex(sql)
	}
}
