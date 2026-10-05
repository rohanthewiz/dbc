package web

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/rohanthewiz/dbc/sqlcomplete"
)

// The editor's go to definition / usages / rename, over HTTP: the resolver's
// answers (tested in sqlcomplete) with their offsets in UTF-16 units.
func TestSymbolAndRename(t *testing.T) {
	e := newTestEnv(t)
	id := decodeData[wsState](t, e.api("POST", "/api/v1/ws", "", 200)).ID

	// the emoji is two UTF-16 units and four bytes, so every offset past
	// it differs between the two: the o of "o.id" is at unit 13, byte 15
	buf := "SELECT '😀', o.id FROM orders o WHERE o.id > 1"
	body, _ := json.Marshal(symbolReq{Buffer: buf, Caret: 13})
	sym := decodeData[sqlcomplete.Symbol](t, e.api("POST", "/api/v1/ws/"+id+"/symbol", string(body), 200))
	if sym.Kind != sqlcomplete.SymAlias || sym.Name != "o" {
		t.Fatalf("symbol = %q %q, want alias o", sym.Kind, sym.Name)
	}
	want := []sqlcomplete.Span{{From: 13, To: 14}, {From: 30, To: 31}, {From: 38, To: 39}}
	if len(sym.Uses) != len(want) {
		t.Fatalf("uses = %v, want %v", sym.Uses, want)
	}
	for i := range want {
		if sym.Uses[i] != want[i] {
			t.Errorf("use %d = %v, want %v", i, sym.Uses[i], want[i])
		}
	}
	if sym.At != want[0] || sym.Def != want[1] {
		t.Errorf("at = %v, def = %v; want %v and %v", sym.At, sym.Def, want[0], want[1])
	}

	// nothing under the caret is an empty answer, not an error
	body, _ = json.Marshal(symbolReq{Buffer: buf, Caret: 1})
	if sym := decodeData[sqlcomplete.Symbol](t, e.api("POST", "/api/v1/ws/"+id+"/symbol", string(body), 200)); sym.Kind != "" {
		t.Errorf("on SELECT: %q, want nothing", sym.Kind)
	}

	// a rename's edits, applied from the end as Monaco would, in units
	renamed := "SELECT '😀', ord.id FROM orders ord WHERE ord.id > 1"
	body, _ = json.Marshal(symbolReq{Buffer: buf, Caret: 13, Name: "ord"})
	r := decodeData[struct{ Edits []sqlcomplete.Edit }](t, e.api("POST", "/api/v1/ws/"+id+"/rename", string(body), 200))
	units := utf16.Encode([]rune(buf))
	for i := len(r.Edits) - 1; i >= 0; i-- {
		ed := r.Edits[i]
		units = slices.Concat(units[:ed.From], utf16.Encode([]rune(ed.Text)), units[ed.To:])
	}
	if got := string(utf16.Decode(units)); got != renamed {
		t.Errorf("renamed:\n got %s\nwant %s", got, renamed)
	}

	// a refusal is a 400 that says why
	body, _ = json.Marshal(symbolReq{Buffer: "SELECT orders.id FROM orders", Caret: 8, Name: "x"})
	env := e.api("POST", "/api/v1/ws/"+id+"/rename", string(body), 400)
	if !strings.Contains(env.Error, "is a table") {
		t.Errorf("refusal = %q", env.Error)
	}
}
