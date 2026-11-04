package sqlcomplete

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rohanthewiz/dbc/snippet"
)

// Live snippets in the completion list (N-228). A snippet is reached by its
// exact name then Tab, or from the snippets dialog; this also lists the
// snippets whose name the word being typed begins, so a name half
// remembered is found where the other suggestions are.
//
// The schema-driven suggestions are Complete's; snippets are the user's,
// kept in a store this package does not know (userdata.Snippets). So they
// are merged afterwards, by the caller that holds the store — the TUI's
// popup and dbc web's /complete — through the one function here, so both
// lists agree on which snippets show and where.
//
// A snippet row inserts nothing itself: its Insert is the name, and picking
// it is the caller's to turn into an expansion (the TUI's insertSnippet,
// Monaco's expand command), exactly as Tab on the typed name would.

// KindSnippet is a live snippet's row (AddSnippets).
const KindSnippet Kind = "snippet"

// Snippet is a live snippet as the completion list needs it.
type Snippet struct {
	Name, Desc, Body string
}

// snippetSlot is how many of the SQL suggestions are listed before the
// snippets. A snippet is an extra, not the likely pick: "up" offers UPDATE
// before the snippet upd, so Tab on a word half typed still picks what it
// picked before snippets were listed — yet the snippets stay on the
// popup's first page (8 rows in the TUI) rather than below hundreds of
// columns.
const snippetSlot = 3

// AddSnippets adds to res a row per snippet whose name begins with the word
// being typed (case-insensitively), after the first snippetSlot items.
// buf is the buffer res answers for.
//
// No row is added:
//   - with nothing typed (res.Prefix ""): Ctrl+Space on an empty word, a
//     caret in a string or a comment, a number — not a snippet's name;
//   - after a "." (o.se▮ is a column), the rule Tab's expansion follows;
//   - for a snippet whose body does not parse: picking it would only fail;
//     the dialog lists it to be fixed.
func AddSnippets(res Result, buf string, snips []Snippet) Result {
	if res.Prefix == "" || len(snips) == 0 {
		return res
	}
	if res.From > 0 && res.From <= len(buf) && buf[res.From-1] == '.' {
		return res
	}
	lp := strings.ToLower(res.Prefix)
	var rows []Item
	for _, sn := range snips {
		if !strings.HasPrefix(strings.ToLower(sn.Name), lp) {
			continue
		}
		if _, err := snippet.Parse(sn.Body); err != nil {
			continue
		}
		detail := "snippet"
		if d := strings.TrimSpace(sn.Desc); d != "" {
			detail = "snippet · " + d
		}
		rows = append(rows, Item{Label: sn.Name, Kind: KindSnippet, Detail: detail,
			Doc: sn.Body, Insert: sn.Name, Cursor: -1})
	}
	if len(rows) == 0 {
		return res
	}
	// shortest name first: the closest to what is typed
	slices.SortStableFunc(rows, func(a, b Item) int {
		if d := len(a.Label) - len(b.Label); d != 0 {
			return d
		}
		return strings.Compare(a.Label, b.Label)
	})

	// Sort keys: Monaco orders by its own match score, then by Sort, so
	// the rows need keys that place them at the slot. The item before the
	// slot's key plus "\x01…" sorts after it and before any longer or
	// greater key — every later item's, but for one tied with it exactly,
	// which only a different kind of the same label and rank can be.
	at := min(snippetSlot, len(res.Items))
	prev := ""
	if at > 0 {
		prev = res.Items[at-1].Sort
	}
	for i := range rows {
		rows[i].Sort = fmt.Sprintf("%s\x01%03d", prev, i)
	}
	items := make([]Item, 0, len(res.Items)+len(rows))
	items = append(items, res.Items[:at]...)
	items = append(items, rows...)
	items = append(items, res.Items[at:]...)
	res.Items = items
	return res
}
