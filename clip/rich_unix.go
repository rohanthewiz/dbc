//go:build linux || freebsd || openbsd || netbsd || dragonfly

package clip

import (
	"bytes"
	"os"
	"os/exec"
	"strings"

	"github.com/rohanthewiz/serr"
)

// Linux and the BSDs: the HTML flavor through wl-copy (Wayland) or dbc's own
// X11 selection owner, falling back to xclip — whichever the session has.
//
// WHAT THE TOOLS OFFER. Neither wl-copy nor xclip takes two payloads, so
// through them c.Text is never sent — which costs nothing today, because the
// one rich copy (HTML, see export.ClipContent) has Text == HTML: a terminal
// paste is meant to get the markup, as it does on macOS and Windows.
//
//   - wl-copy: given any text/* type it ALSO offers text/plain,
//     text/plain;charset=utf-8, TEXT, STRING and UTF8_STRING, all carrying
//     the same bytes (wl-copy.c, mime_type_is_text). A terminal paste gets
//     the markup. No gap.
//   - xclip: advertises only TARGETS and text/html. GTK and Qt clients read
//     TARGETS first, find no text flavor, and paste NOTHING. xclip's
//     -alt-text (adds a STRING target) exists only on its master branch,
//     not in 0.13, the last release distros ship.
//
// So on X11 dbc owns the CLIPBOARD selection itself (x11owner.go): a
// detached helper that answers text/html AND every text target, each with
// its own payload. xclip stays as the fallback for when the helper cannot
// start, which keeps the old, HTML-only behavior rather than none.
//
// Wayland is checked first because XWayland makes $DISPLAY present on most
// Wayland desktops too, and an X11 owner there writes a clipboard native
// apps see only if the compositor bridges it.
func platformWriteRich(c Content) error {
	var cmd *exec.Cmd
	switch {
	case os.Getenv("WAYLAND_DISPLAY") != "" && have("wl-copy"):
		cmd = exec.Command("wl-copy", "--type", "text/html")
	case os.Getenv("DISPLAY") != "":
		err := writeX11(c)
		if err == nil || !have("xclip") {
			return err
		}
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
