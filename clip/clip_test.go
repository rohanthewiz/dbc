package clip

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// stubWriters replaces both platform writers for one test, so the suite never
// touches the developer's real clipboard, and returns what each one saw.
func stubWriters(t *testing.T, richErr, textErr error) (rich *[]Content, text *[]string) {
	t.Helper()
	origRich, origText := writeRich, writeText
	t.Cleanup(func() { writeRich, writeText = origRich, origText })

	var r []Content
	var x []string
	writeRich = func(c Content) error { r = append(r, c); return richErr }
	writeText = func(s string) error { x = append(x, s); return textErr }
	return &r, &x
}

func TestWriteRichLandsBothFlavors(t *testing.T) {
	rich, text := stubWriters(t, nil, nil)
	ok, err := Write(Content{Text: "a\tb", HTML: "<table></table>"})
	if err != nil || !ok {
		t.Fatalf("Write = %v, %v; want rich success", ok, err)
	}
	if len(*rich) != 1 || (*rich)[0].HTML != "<table></table>" || (*rich)[0].Text != "a\tb" {
		t.Errorf("rich writer got %+v", *rich)
	}
	if len(*text) != 0 {
		t.Errorf("plain writer should not run after a rich success, got %q", *text)
	}
}

func TestWriteFallsBackToTextWhenRichFails(t *testing.T) {
	for _, richErr := range []error{errNoRich, errors.New("osascript exploded")} {
		_, text := stubWriters(t, richErr, nil)
		ok, err := Write(Content{Text: "plain", HTML: "<b>x</b>"})
		if err != nil {
			t.Fatalf("%v: a working plain fallback is a success, got %v", richErr, err)
		}
		if ok {
			t.Errorf("%v: rich must be false so the UI can say 'as plain text'", richErr)
		}
		if len(*text) != 1 || (*text)[0] != "plain" {
			t.Errorf("%v: plain writer got %q", richErr, *text)
		}
	}
}

func TestWritePlainOnlySkipsRich(t *testing.T) {
	rich, text := stubWriters(t, nil, nil)
	if ok, err := Write(Content{Text: "x"}); err != nil || ok {
		t.Fatalf("Write = %v, %v", ok, err)
	}
	if len(*rich) != 0 {
		t.Error("no HTML means no rich attempt")
	}
	if len(*text) != 1 {
		t.Error("plain text should have been written")
	}
}

func TestWriteReportsTotalFailure(t *testing.T) {
	stubWriters(t, errNoRich, errors.New("no pbcopy"))
	if _, err := Write(Content{Text: "x", HTML: "<i>x</i>"}); err == nil {
		t.Fatal("nothing reached the clipboard; the caller needs an error to try OSC 52")
	}
}

// TestCFHTMLOffsets reads the header back the way a pasting app does and
// checks each offset lands exactly where it claims — including with
// multi-byte UTF-8 in the fragment, where counting runes would be wrong.
func TestCFHTMLOffsets(t *testing.T) {
	for _, frag := range []string{"<b>x</b>", "<td>café ☕</td>", ""} {
		doc := cfHTML(frag)
		off := func(key string) int {
			i := strings.Index(doc, key+":")
			if i < 0 {
				t.Fatalf("%q: header has no %s", frag, key)
			}
			n, err := strconv.Atoi(doc[i+len(key)+1 : i+len(key)+11])
			if err != nil {
				t.Fatalf("%q: %s is not ten digits: %v", frag, key, err)
			}
			return n
		}
		sh, eh, sf, ef := off("StartHTML"), off("EndHTML"), off("StartFragment"), off("EndFragment")
		if !strings.HasPrefix(doc[sh:], "<html>") {
			t.Errorf("%q: StartHTML points at %q", frag, doc[sh:min(sh+10, len(doc))])
		}
		if eh != len(doc) || !strings.HasSuffix(doc[:eh], "</html>") {
			t.Errorf("%q: EndHTML = %d, doc is %d bytes", frag, eh, len(doc))
		}
		if got := doc[sf:ef]; got != frag {
			t.Errorf("fragment slice = %q, want %q", got, frag)
		}
	}
}
