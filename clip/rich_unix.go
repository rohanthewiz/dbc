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
// A LIMITATION WORTH KNOWING. Both tools offer exactly one MIME type per
// invocation, so the rich copy is text/html ONLY — a paste into a terminal
// right after it finds no text/plain and pastes nothing (xclip) or the
// markup (some Wayland compositors convert). Serving two types at once would
// mean owning the selection ourselves, a long-lived X11/Wayland client, which
// is far more than a copy key is worth. Rich copy is the thing the user asked
// for when they pick "HTML", and the plain formats are one menu row away.
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
