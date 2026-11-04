package sqlcomplete

import (
	"slices"
	"testing"
)

// The snippets whose name the word begins are listed after the first
// snippetSlot suggestions, shortest name first, with keys that keep that
// place in a client sorting by Sort; nothing is added after a ".", on an
// empty word, or for a body that does not parse.
func TestAddSnippets(t *testing.T) {
	snips := []Snippet{
		{Name: "update_tx", Body: "UPDATE $VAR1$"},
		{Name: "upd", Desc: "update rows", Body: "UPDATE $VAR1:t$ SET $END$"},
		{Name: "upbad", Body: "$VAR1"}, // does not parse
		{Name: "sel", Body: "SELECT 1"},
	}
	buf := "up"
	res := AddSnippets(Complete(Request{Driver: "postgres", Buffer: buf, Caret: len(buf)}), buf, snips)
	var labels []string
	at := -1
	for i, it := range res.Items {
		if it.Kind == KindSnippet {
			if at < 0 {
				at = i
			}
			labels = append(labels, it.Label)
		}
	}
	if want := []string{"upd", "update_tx"}; !slices.Equal(labels, want) {
		t.Fatalf("snippet rows = %v, want %v", labels, want)
	}
	if want := min(snippetSlot, len(res.Items)-2); at != want {
		t.Errorf("first snippet at %d, want %d", at, want)
	}
	if res.Items[0].Kind == KindSnippet {
		t.Error("a snippet took the first row: a half-typed keyword's Tab would expand it")
	}
	if !slices.IsSortedFunc(res.Items, func(a, b Item) int {
		switch {
		case a.Sort < b.Sort:
			return -1
		case a.Sort > b.Sort:
			return 1
		}
		return 0
	}) {
		t.Error("the merged list's Sort keys are out of order: Monaco would move the snippets")
	}
	upd := res.Items[at]
	if upd.Insert != "upd" || upd.Detail != "snippet · update rows" || upd.Doc == "" {
		t.Errorf("upd row = %+v", upd)
	}

	// no SQL suggestion at all: the snippets alone, in order
	buf = "SELECT 1 FROM t WHERE x = 1 se"
	if r := AddSnippets(Result{From: len(buf) - 2, To: len(buf), Prefix: "se"}, buf, snips); len(r.Items) != 1 || r.Items[0].Label != "sel" {
		t.Errorf("alone: %+v", r.Items)
	}
	// after a dot the word is a column, and an empty word names nothing
	buf = "SELECT o.up"
	if r := AddSnippets(Result{From: len(buf) - 2, To: len(buf), Prefix: "up"}, buf, snips); len(r.Items) != 0 {
		t.Errorf("after a dot: %+v", r.Items)
	}
	if r := AddSnippets(Result{From: 0, To: 0}, "", snips); len(r.Items) != 0 {
		t.Errorf("empty word: %+v", r.Items)
	}
	// case-insensitive
	buf = "UP"
	if r := AddSnippets(Result{From: 0, To: 2, Prefix: "UP"}, buf, snips); len(r.Items) != 2 {
		t.Errorf("UP: %+v", r.Items)
	}
}
