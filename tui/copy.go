package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/clip"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
)

// Copying out of dbc.
//
// Every copy runs as a command, off the event loop: the rich writer shells
// out to osascript on macOS, which takes a noticeable fraction of a second,
// and the UI must not freeze for it. The outcome comes back as clipDoneMsg,
// and the log line says exactly how it landed — "as a table", "as plain
// text", or "via the terminal" — because the user is about to paste into a
// chat window and those three look very different there.
//
// When nothing reaches the local clipboard (SSH, a container), the text goes
// out through the terminal with OSC 52 instead, which Bubble Tea emits for
// us. That fallback is plain text by protocol, so an HTML copy arrives as its
// markup, and the log line says that too.

// copyFormat is what a copy renders as.
type copyFormat int

const (
	// copyText is the plain, keyboard-shortcut copy: a single cell's value
	// as-is, or cells tab-separated with no header — what pastes cleanly
	// into a spreadsheet or back into a query.
	copyText copyFormat = iota
	copyHTML
	copyMarkdown
	copyCSV
	copyTSV
	copyJSON
	copyAligned // the aligned plain-text table, export's "text" format
)

// copyFormatFor maps an export format onto the copy that renders it.
func copyFormatFor(f export.Format) copyFormat {
	switch f {
	case export.HTML:
		return copyHTML
	case export.Markdown:
		return copyMarkdown
	case export.CSV:
		return copyCSV
	case export.TSV:
		return copyTSV
	case export.JSON:
		return copyJSON
	}
	return copyAligned
}

func (f copyFormat) name() string {
	switch f {
	case copyHTML:
		return "a table"
	case copyMarkdown:
		return "Markdown"
	case copyCSV:
		return "CSV"
	case copyTSV:
		return "TSV"
	case copyJSON:
		return "JSON"
	case copyAligned:
		return "an aligned text table"
	}
	return "text"
}

// clipDoneMsg reports a finished clipboard write.
type clipDoneMsg struct {
	what string
	text string // the plain flavor, for the OSC 52 fallback
	html bool   // an HTML flavor was offered
	rich bool   // and it landed
	err  error
}

// copyGrid copies the grid's selection (or cursor cell), or the whole result.
func (m *Model) copyGrid(f copyFormat, whole bool) tea.Cmd {
	r, what := m.grid.Selected(whole)
	if r == nil {
		m.log(logWarn, noResult)
		return nil
	}
	return m.copyResult(r, what, f)
}

// copyResult renders r in format f and copies it.
func (m *Model) copyResult(r *model.Result, what string, f copyFormat) tea.Cmd {
	if r == nil {
		m.log(logWarn, noResult)
		return nil
	}
	var c clip.Content
	var err error
	switch f {
	case copyText:
		c.Text = export.PlainCells(r)
	case copyHTML:
		c, err = export.ClipContent(r, export.HTML)
	case copyMarkdown:
		c, err = export.ClipContent(r, export.Markdown)
	case copyCSV:
		c, err = export.ClipContent(r, export.CSV)
	case copyTSV:
		c, err = export.ClipContent(r, export.TSV)
	case copyJSON:
		c, err = export.ClipContent(r, export.JSON)
	case copyAligned:
		c, err = export.ClipContent(r, export.Text)
	}
	if err != nil {
		m.logf(logErr, "copy failed: %s", serr.StringFromErr(err))
		return nil
	}
	if f != copyText {
		what += " as " + f.name()
	}
	return clipCmd(c, what)
}

// copyString copies plain text.
func (m *Model) copyString(text, what string) tea.Cmd {
	if text == "" {
		m.log(logWarn, "nothing to copy")
		return nil
	}
	return clipCmd(clip.Content{Text: text}, what)
}

// clipWrite is clip.Write, as a var so tests never touch the real clipboard.
var clipWrite = clip.Write

// clipCmd writes c to the clipboard on a command goroutine.
func clipCmd(c clip.Content, what string) tea.Cmd {
	return func() tea.Msg {
		rich, err := clipWrite(c)
		return clipDoneMsg{what: what, text: c.Text, html: c.HTML != "", rich: rich, err: err}
	}
}

// clipMax bounds an OSC 52 fallback: terminals drop oversized sequences
// silently, and a copy that loses its tail is worse than one that visibly
// did not happen.
const clipMax = 1 << 20

// clipDone reports a copy, falling back to OSC 52 when the local clipboard
// was unreachable.
func (m *Model) clipDone(msg clipDoneMsg) tea.Cmd {
	if msg.err == nil {
		how := ""
		switch {
		case msg.html && msg.rich:
			how = " — paste into Teams, Outlook or a doc for a formatted table"
		case msg.html:
			how = " — as plain text (no HTML clipboard on this system)"
		}
		m.logf(logOk, "copied %s%s", msg.what, how)
		return nil
	}
	if len(msg.text) > clipMax {
		m.logf(logErr, "copy failed: %s", serr.StringFromErr(msg.err))
		return nil
	}
	note := ""
	if msg.html {
		note = " (HTML source: a formatted table needs a local clipboard)"
	}
	m.logf(logOk, "copied %s via the terminal%s", msg.what, note)
	return tea.SetClipboard(msg.text)
}
