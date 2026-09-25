//go:build linux

package clip

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLiveX11Clipboard drives the X11 owner end to end and reads every flavor
// back with xclip, the way a pasting app would. Opt-in like the macOS live
// test — DBC_CLIP_LIVE=1 — since it replaces the real clipboard; it needs
// $DISPLAY and xclip, and ignores WAYLAND_DISPLAY so it always tests X11.
func TestLiveX11Clipboard(t *testing.T) {
	if os.Getenv("DBC_CLIP_LIVE") == "" {
		t.Skip("set DBC_CLIP_LIVE=1 to write the real clipboard")
	}
	if os.Getenv("DISPLAY") == "" || !have("xclip") {
		t.Skip("needs $DISPLAY and xclip")
	}
	t.Setenv("WAYLAND_DISPLAY", "")

	read := func(target string) []byte {
		t.Helper()
		out, err := exec.Command("xclip", "-o", "-selection", "clipboard", "-t", target).Output()
		if err != nil {
			t.Fatalf("read %s: %v", target, err)
		}
		return out
	}

	html := `<meta charset="utf-8"><table><tr><td>café ✓</td></tr></table>`
	rich, err := Write(Content{Text: "café ✓", HTML: html})
	if err != nil || !rich {
		t.Fatalf("Write = %v, %v", rich, err)
	}

	targets := strings.Fields(string(read("TARGETS")))
	for _, want := range []string{"TARGETS", "TIMESTAMP", "text/html", "UTF8_STRING",
		"text/plain;charset=utf-8", "text/plain", "TEXT", "STRING"} {
		if !slices.Contains(targets, want) {
			t.Errorf("TARGETS = %v, missing %s", targets, want)
		}
	}
	if got := string(read("text/html")); got != html {
		t.Errorf("text/html = %q", got)
	}
	for _, tgt := range []string{"UTF8_STRING", "text/plain;charset=utf-8", "text/plain", "TEXT"} {
		if got := string(read(tgt)); got != "café ✓" {
			t.Errorf("%s = %q", tgt, got)
		}
	}
	// STRING is Latin-1: é is one byte, and ✓ has no Latin-1 form.
	if got := read("STRING"); !bytes.Equal(got, []byte("caf\xe9 ?")) {
		t.Errorf("STRING = %q", got)
	}

	// A copy bigger than one request goes out as INCR chunks; xclip -o
	// reassembles them, so equality proves the chunking and the terminator.
	big := strings.Repeat("<tr><td>row</td><td>ünïcode</td></tr>\n", 40000) // ~1.5 MB
	if rich, err = Write(Content{Text: big, HTML: big}); err != nil || !rich {
		t.Fatalf("big Write = %v, %v", rich, err)
	}
	if got := string(read("text/html")); got != big {
		t.Errorf("INCR text/html: got %d bytes, want %d", len(got), len(big))
	}

	// Another client copying must make every helper exit: that is the only
	// thing that ever ends one, so a bug here leaks a process per copy.
	cmd := exec.Command("xclip", "-selection", "clipboard", "-i")
	cmd.Stdin = strings.NewReader("someone else")
	if err = cmd.Run(); err != nil {
		t.Fatalf("xclip -i: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for n := helpers(t); n > 0; n = helpers(t) {
		if time.Now().After(deadline) {
			t.Fatalf("%d clipboard helper(s) still running after another client copied", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// helpers counts live x11 helper processes by their argument in /proc.
// Exited-but-unreaped ones read as an empty cmdline and do not count.
func helpers(t *testing.T) int {
	t.Helper()
	paths, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	n := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err == nil && bytes.Contains(b, []byte("\x00"+x11HelperArg)) {
			n++
		}
	}
	return n
}
