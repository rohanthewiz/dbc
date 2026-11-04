package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/clip"
)

// Pasting from the editor menu.
//
// A terminal app does not usually read the clipboard itself: the terminal's
// own paste key (⌘V, Ctrl+Shift+V) types the text in as a bracketed paste,
// which arrives as tea.PasteMsg (Model.paste). A menu row has no such key
// behind it, so the editor menu's Paste — dbc web's row, ported — reads the
// clipboard for itself, the way copy writes it (copy.go):
//
//	menu Paste ──► clipReader (command goroutine: pbpaste, wl-paste, …)
//	                 │ ok                     │ no local clipboard (SSH …)
//	                 ▼                        ▼
//	          clipReadMsg ──► insert    tea.ReadClipboard (OSC 52 query)
//	                                          │ the terminal answers, if it
//	                                          ▼ allows reads at all
//	                                    tea.ClipboardMsg ──► insert
//
// WHICH EDITOR. Both reads are asynchronous and the user may switch tabs
// before one lands, so the text goes to the editor the menu was opened on —
// carried in clipReadMsg, and held in Model.pasteInto across the OSC 52
// round trip — never to whatever editor is current by then.
//
// OSC 52 READS are off by default in many terminals (iTerm2, tmux) or ask
// first (kitty, Ghostty); one that refuses simply never answers. So before
// asking, the log says what is happening and what to do instead, and the
// pending read is dropped by the next reply — a silent row would read as a
// click the terminal lost (the menu grammar, menu.go).

// clipReader is clip.Read, as a var so tests never touch the real clipboard.
var clipReader = clip.Read

// clipReadMsg is a finished local clipboard read.
type clipReadMsg struct {
	ed   *editor // the editor the menu was opened on
	text string
	err  error // non-nil: no local clipboard — try the terminal
}

// menuPaste is the editor menu's Paste: read the clipboard off the event
// loop (pbpaste and its kin are subprocesses) and insert what comes back.
func (m *Model) menuPaste() tea.Cmd {
	ed := m.editor
	return func() tea.Msg {
		s, err := clipReader()
		return clipReadMsg{ed: ed, text: s, err: err}
	}
}

// pasteRead lands a local read, or falls back to asking the terminal.
func (m *Model) pasteRead(msg clipReadMsg) tea.Cmd {
	if msg.err == nil {
		m.pasteText(msg.ed, msg.text)
		return nil
	}
	m.pasteInto = msg.ed
	m.log(logMuted, "paste: no local clipboard — asking the terminal (OSC 52); "+
		"if nothing arrives, the terminal does not allow reads: use its own paste key")
	return tea.ReadClipboard
}

// clipboardReply is the terminal's answer to an OSC 52 read. One nobody
// asked for (no Paste pending) is ignored.
func (m *Model) clipboardReply(msg tea.ClipboardMsg) tea.Cmd {
	ed := m.pasteInto
	m.pasteInto = nil
	if ed != nil {
		m.pasteText(ed, msg.Content)
	}
	return nil
}

// pasteText inserts s into ed at its caret, replacing its selection, as a
// terminal paste into the editor does (Model.paste).
func (m *Model) pasteText(ed *editor, s string) {
	if s == "" {
		m.log(logWarn, "paste: the clipboard holds no text")
		return
	}
	ed.Insert(s)
	if ed == m.editor {
		m.drag.follow = true
		m.compl = nil // its word and range are not the buffer's any more
	}
}
