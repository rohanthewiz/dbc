//go:build linux || freebsd || openbsd || netbsd || dragonfly

package clip

import (
	"bytes"
	"os"
	"os/exec"
	"strings"

	"github.com/rohanthewiz/serr"
)

// Linux and the BSDs: the HTML flavor through wl-copy (Wayland) or xclip
// (X11), whichever the session has.
//
// WHAT EACH TOOL ACTUALLY OFFERS. Neither takes two payloads, so c.Text is
// never sent — which costs nothing today, because the one rich copy (HTML,
// see export.ClipContent) has Text == HTML: a terminal paste is meant to get
// the markup, as it does on macOS and Windows.
//
//   - wl-copy: given any text/* type it ALSO offers text/plain,
//     text/plain;charset=utf-8, TEXT, STRING and UTF8_STRING, all carrying
//     the same bytes (wl-copy.c, mime_type_is_text). A terminal paste gets
//     the markup. No gap.
//   - xclip: advertises only TARGETS and text/html. GTK and Qt clients read
//     TARGETS first, find no text flavor, and paste NOTHING. This is the
//     one real gap. xclip's -alt-text (adds a STRING target) exists only on
//     its master branch, not in 0.13, the last release distros ship.
//
// Closing the X11 gap — or ever sending a Text that differs from HTML —
// means owning the CLIPBOARD selection ourselves: a detached X11 client that
// answers every target until another app takes the selection. Not built;
// tracked as N-030 in ai_docs/todo/next-list.md.
//
// Wayland is checked first because XWayland makes $DISPLAY present on most
// Wayland desktops too, and xclip there writes a clipboard native apps may
// not see.
func platformWriteRich(c Content) error {
	var cmd *exec.Cmd
	switch {
	case os.Getenv("WAYLAND_DISPLAY") != "" && have("wl-copy"):
		cmd = exec.Command("wl-copy", "--type", "text/html")
	case os.Getenv("DISPLAY") != "" && have("xclip"):
		cmd = exec.Command("xclip", "-selection", "clipboard", "-t", "text/html", "-i")
	default:
		return errNoRich
	}
	cmd.Stdin = strings.NewReader(c.HTML)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return serr.Wrap(err, "op", "clipboard-rich", "tool", cmd.Args[0],
			"stderr", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// have reports whether a tool is on PATH.
func have(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}
