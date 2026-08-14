package ui

import (
	"fmt"
	"os"
	"strings"
)

// Telling the terminal who and where we are: the window/tab title, and the
// working directory.
//
// This is a TIER-0 feature — it works in any terminal that understands the
// sequences, and cats is simply the host that does the most with them (the
// tab label, and the directory a new pane inherits). It lives beside the cats
// files because it is the same act of identifying ourselves to a host, one
// layer down.
//
// WHAT IS EMITTED, AND WHAT IS NOT:
//
//	title   tview's Application.SetTitle, which reaches tcell's own OSC 2
//	        emitter. tcell also SAVES the terminal's existing title when it
//	        starts and RESTORES it on exit (XTPUSHTITLE/XTPOPTITLE, its
//	        saveTitle/restoreTitle), so dbc neither pushes nor pops: doing it
//	        again would push a second copy and leave one behind.
//	cwd     OSC 7, written to /dev/tty directly. tcell has no cwd API, and
//	        this is the one sequence we have to spell out ourselves.
//
// Why /dev/tty rather than through tcell: OSC 7 moves no cursor and paints no
// cell, so interleaving it with a frame is harmless, and going around the
// screen keeps it out of tcell's output buffering entirely. It is emitted
// once — dbc's working directory does not change while it runs.
//
// EVERYTHING HERE IS OPTIONAL. A terminal that ignores the sequences, or a
// process with no controlling terminal, costs one failed open and nothing
// else.

// hostIdentInit emits the one-shot identity: where we are. The title follows
// from hostIdentSync as state changes.
func (a *App) hostIdentInit() {
	if a.ttyWrite == nil {
		return // no controlling terminal, or a test
	}
	if cwd, err := os.Getwd(); err == nil {
		_ = a.ttyWrite(osc7CwdSeq(hostname(), cwd))
	}
	a.hostIdentSync()
}

// hostIdentSync puts the current state in the terminal's title, if it has
// changed.
//
// The comparison is on the INPUTS rather than on the built string: the idle
// path runs on every run-state transition, and rebuilding a title to discover
// it is the same one allocates for nothing.
func (a *App) hostIdentSync() {
	a.runMu.Lock()
	tag := a.runTag
	a.runMu.Unlock()
	if !a.busy.Load() {
		tag = ""
	}

	key := a.active + "\x00" + tag
	if a.identSent && key == a.identKey {
		return
	}
	a.identSent, a.identKey = true, key
	a.app.SetTitle(hostIdentTitle(a.active, tag))
}

// hostIdentTitle is what the terminal tab says. The running tag leads,
// because a title is read at a glance from a tab bar the pane is not in — the
// question it answers there is "is this thing still going?", and the
// connection name is the context for that answer rather than the answer.
func hostIdentTitle(conn, tag string) string {
	conn, tag = titleSafe(conn), titleSafe(tag)
	switch {
	case tag != "" && conn != "":
		return tag + " · " + conn + " — dbc"
	case tag != "":
		return tag + " — dbc"
	case conn != "":
		return conn + " — dbc"
	}
	return "dbc"
}

// titleSafe strips what must never reach a terminal's parser. Connection
// names come from the user's TOML and run tags carry script filenames, so
// both are user-controlled text: an embedded ESC or BEL would terminate the
// escape sequence early and leave the remainder to be interpreted as
// commands.
func titleSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}

// osc7CwdSeq reports the working directory as a file URL. Hosts use it to
// open a new tab or split in the same place — which is the whole payoff, and
// why it is worth emitting even though nothing in dbc reads it back.
func osc7CwdSeq(host, dir string) string {
	return fmt.Sprintf("\x1b]7;file://%s%s\x07", host, fileURLPath(dir))
}

// fileURLPath percent-encodes a path for a file:// URL. net/url is
// deliberately not used: url.URL escapes for a generic URL and leaves
// characters this sequence has to encode, and the terminating BEL means a
// stray control byte in a directory name would truncate the sequence rather
// than corrupt one field of it.
func fileURLPath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '/' || c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// hostname is the URL's authority. An unknown host is spelled as the empty
// authority rather than "localhost": a file URL with no host means "this
// machine", which is exactly what we would be guessing at anyway.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return titleSafe(h)
}

// ttyWriteReal writes an escape sequence straight to the controlling
// terminal. Opened per emission — there are at most a handful in a session,
// and holding the descriptor open would be a resource kept for nothing.
func ttyWriteReal(seq string) error {
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(seq)
	return err
}
