// Package clip puts text on the local system clipboard, and HTML alongside it
// when the caller has some, so a chat app or document editor pastes a real
// table rather than the markup that describes one.
//
// WHY TWO FLAVORS. A clipboard is not one string: it holds the same content in
// several representations and the PASTING app picks the richest one it
// understands. Teams, Outlook, Slack and Google Docs take the HTML flavor and
// render it; a terminal or a code editor takes the plain-text flavor. Writing
// only plain text — which is all github.com/atotto/clipboard can do — means a
// copied HTML table arrives in Teams as a wall of <td> tags.
//
// Each platform names the HTML flavor differently and none of the stock
// command-line tools set two flavors at once, so each gets its own writer:
//
//	macOS    osascript -l JavaScript → NSPasteboard: public.html + public.utf8-plain-text
//	Windows  user32 SetClipboardData: "HTML Format" (CF_HTML) + CF_UNICODETEXT
//	Linux    Wayland: wl-copy -t text/html (one payload; see rich_unix.go)
//	         X11: dbc owns the selection, every target (see x11owner.go)
//
// Plain-text writes keep going through atotto, which has been dbc's path all
// along and whose behavior on every platform is already known.
//
// WHAT THIS PACKAGE DOES NOT DO: reach a clipboard on another machine. Over
// SSH the local clipboard is the server's (or none), and the only way to the
// user's is OSC 52 through their terminal — which carries plain text only.
// That fallback needs the terminal handle, so it lives with the UI; Write
// reports failure and the UI decides what to do next.
package clip

import (
	"errors"

	"github.com/atotto/clipboard"
	"github.com/rohanthewiz/serr"
)

// Content is what one copy offers. Text is required: it is what every paste
// target can take, and what a rich write falls back to. HTML is optional; when
// set it should be a fragment (see export.HTMLFragment), not a whole page.
type Content struct {
	Text string
	HTML string
}

// errNoRich is what a platform writer returns when this system has no way to
// set an HTML flavor at all — as opposed to having one that failed. Write
// treats both the same way (fall back to plain text), but tests and log lines
// can tell them apart.
var errNoRich = errors.New("no HTML clipboard support on this system")

// The writers, as vars so tests can stand in for the real clipboard: a test
// that wrote the developer's actual clipboard would clobber whatever they had
// copied, every time the suite ran.
var (
	writeRich = platformWriteRich // both flavors at once
	writeText = clipboard.WriteAll
)

// Write places c on the local system clipboard.
//
// With HTML set it tries the platform's rich writer first and reports
// rich=true when both flavors landed. If the rich write is unavailable or
// fails, it falls back to c.Text alone, so the user still gets SOMETHING on
// the clipboard, and rich=false lets the caller say so ("copied as plain
// text") instead of promising a table that will not appear.
//
// A non-nil error means nothing reached the local clipboard at all — the case
// where the UI should try OSC 52.
func Write(c Content) (rich bool, err error) {
	if c.HTML != "" {
		if rerr := writeRich(c); rerr == nil {
			return true, nil
		}
		// The rich failure is deliberately not returned: a plain-text copy
		// that works is a success from the user's point of view, and the
		// rich=false result already carries the "not as a table" part.
	}
	if err = writeText(c.Text); err != nil {
		return false, serr.Wrap(err, "op", "clipboard")
	}
	return false, nil
}
