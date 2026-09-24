package ui

import (
	"github.com/atotto/clipboard"

	"github.com/rohanthewiz/dbc/clip"
)

// Getting a copy out of dbc and onto the user's clipboard, from wherever dbc
// happens to be running.
//
// THE PROBLEM THIS SOLVES. github.com/atotto/clipboard shells out to
// pbcopy/xclip/wl-copy, which reach the clipboard of the machine dbc's
// PROCESS is on. That is the right answer on a laptop and the wrong one over
// SSH or in a container, where those tools are missing or are talking to a
// display nobody is looking at. In those places every y, Y and clipboard
// export failed with a message about a missing binary.
//
// THE FALLBACK. OSC 52 asks the TERMINAL to set the clipboard, and the
// terminal is by definition on the machine the user is sitting at. tcell
// emits it for any xterm-like terminal, so the fallback is one call on the
// screen handle rather than an escape sequence spelled out here.
//
// It is a FALLBACK rather than the primary path because OSC 52 is
// write-and-hope: there is no reply, no acknowledgement, and no way to learn
// that the terminal ignored it or that the user's emulator has it switched
// off (many do, since a program that can print bytes could otherwise
// overwrite the clipboard silently). A local pbcopy that returns success has
// actually succeeded, so it goes first.
//
// A NOTE ON WHERE THE TEXT LANDS INSIDE CATS: cats delivers OSC 52 to the
// BROWSER's clipboard, which is the user's machine — the same place a local
// pbcopy would have reached in a desktop session, and the RIGHT place when
// catway is running on a server. That is the case this whole file exists for.

// sysClipWrite is the local clipboard tool. A var so a test can make it fail
// on demand: the fallback is the interesting path and it is unreachable while
// pbcopy works.
var sysClipWrite = clipboard.WriteAll

// clipMax bounds what goes out over OSC 52. Terminals impose their own caps
// (commonly around 100 KB) and a sequence over the limit is dropped or
// truncated with no error, so past this size the honest answer is the failure
// the local tool already gave us: a copy that silently loses its tail is
// worse than one that visibly did not happen.
const clipMax = 1 << 20 // 1 MiB

// clipWrite puts text on the user's clipboard and names where it went, for
// the log line the caller writes.
func (a *App) clipWrite(text string) (dest string, err error) {
	localErr := sysClipWrite(text)
	if localErr == nil {
		return "clipboard", nil
	}
	if a.scr == nil || len(text) > clipMax {
		return "", localErr
	}
	a.scr.SetClipboard([]byte(text))
	return "clipboard (via the terminal)", nil
}

// sysClipWriteRich is the local writer for a copy that carries an HTML flavor.
// A var for the same reason as sysClipWrite: tests must not write the real
// clipboard.
var sysClipWriteRich = clip.Write

// clipWriteContent is clipWrite for a copy that may carry an HTML flavor as
// well as text — "copy as HTML" meant for pasting into Teams as a table.
//
// The destination it names is more specific than clipWrite's, because the
// same keystroke can land three different ways and the user pastes into a
// chat window expecting one of them:
//
//	local rich writer worked        → a table
//	local tool took plain text only → the markup (e.g. no xclip on this box)
//	no local clipboard (SSH)        → OSC 52, which is text-only by protocol
//
// Saying which in the log line is the difference between "why did Teams get
// tags?" and knowing before pasting.
func (a *App) clipWriteContent(c clip.Content) (dest string, err error) {
	if c.HTML == "" {
		return a.clipWrite(c.Text)
	}
	rich, localErr := sysClipWriteRich(c)
	if localErr == nil {
		if rich {
			return "clipboard as a table", nil
		}
		return "clipboard as plain text (no HTML clipboard on this system)", nil
	}
	if a.scr == nil || len(c.Text) > clipMax {
		return "", localErr
	}
	a.scr.SetClipboard([]byte(c.Text))
	return "clipboard via the terminal, as HTML source (a table needs a local clipboard)", nil
}
