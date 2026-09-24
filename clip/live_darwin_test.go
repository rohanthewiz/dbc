//go:build darwin

package clip

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestLiveRichClipboard writes the REAL clipboard and reads both flavors
// back, so it only runs when asked: DBC_CLIP_LIVE=1 go test ./clip. It
// restores the plain text that was there before, but anything richer the
// user had copied (an image, a styled selection) is lost — which is why it
// is opt-in rather than part of every run.
func TestLiveRichClipboard(t *testing.T) {
	if os.Getenv("DBC_CLIP_LIVE") == "" {
		t.Skip("set DBC_CLIP_LIVE=1 to write the real clipboard")
	}
	saved, _ := exec.Command("pbpaste").Output()
	t.Cleanup(func() {
		cmd := exec.Command("pbcopy")
		cmd.Stdin = strings.NewReader(string(saved))
		_ = cmd.Run()
	})

	html := `<meta charset="utf-8"><table><tr><td>café</td></tr></table>`
	rich, err := Write(Content{Text: "café", HTML: html})
	if err != nil || !rich {
		t.Fatalf("Write = %v, %v", rich, err)
	}
	read := func(typ string) string {
		out, err := exec.Command("osascript", "-l", "JavaScript", "-e",
			`ObjC.import("AppKit"); ObjC.unwrap($.NSPasteboard.generalPasteboard.stringForType($.`+typ+`))`).Output()
		if err != nil {
			t.Fatalf("read %s: %v", typ, err)
		}
		return strings.TrimRight(string(out), "\n")
	}
	if got := read("NSPasteboardTypeHTML"); got != html {
		t.Errorf("HTML flavor = %q", got)
	}
	if got := read("NSPasteboardTypeString"); got != "café" {
		t.Errorf("text flavor = %q", got)
	}
}
