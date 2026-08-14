package ui

import (
	"github.com/atotto/clipboard"
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
