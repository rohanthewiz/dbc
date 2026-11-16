package tui

import (
	"strings"

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
// us. That fallback is plain text by protocol, so a table copy arrives as its
// plain flavor — the aligned text table, never the HTML markup (see
// export.ClipContent) — and the log line says that too.

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
	// failed is a copy that never reached the clipboard: its rendering
	// failed (a NaN for JSON), on the command goroutine that renders it
	failed error
}

// copyGrid copies the grid's selection (or cursor cell), or the whole result
// — as the grid shows it, so transposed it comes out on its side.
func (m *Model) copyGrid(f copyFormat, whole bool) tea.Cmd {
	return m.copyPiece(m.grid.selectedPiece(whole), f)
}

// copyRow copies the cursor's row (Y), as the grid shows it.
func (m *Model) copyRow(f copyFormat) tea.Cmd {
	return m.copyPiece(m.grid.rowPiece(), f)
}

// copyResult renders r in format f and copies it.
func (m *Model) copyResult(r *model.Result, what string, f copyFormat) tea.Cmd {
	if r == nil {
		m.log(logWarn, noResult)
		return nil
	}
	return renderCopyCmd(func() *model.Result { return r }, what, f)
}

// renderCopyCmd builds a Result, renders it in format f and copies it, all
// on a command goroutine (N-282). The copy's rendering used to run in
// Update, before its clipboard write went off the loop: a million rows as
// a table froze the screen for the render, and the projection before it.
// build is what to copy — a grid piece's projection, on its side when the
// grid is — and what names it.
func renderCopyCmd(build func() *model.Result, what string, f copyFormat) tea.Cmd {
	return func() tea.Msg {
		r := build()
		c, err := renderCopy(r, f)
		if err != nil {
			return clipDoneMsg{what: what, failed: err}
		}
		if f != copyText {
			what += " as " + f.name()
		}
		if r.Transposed && !strings.Contains(what, "transposed") {
			// dbc web's words: "the result (8 rows) as a table, transposed"
			what += ", transposed"
		}
		return writeClip(c, what)
	}
}

// renderCopy is r in format f as clipboard content.
func renderCopy(r *model.Result, f copyFormat) (clip.Content, error) {
	switch f {
	case copyText:
		return clip.Content{Text: export.PlainCells(r)}, nil
	case copyHTML:
		return export.ClipContent(r, export.HTML)
	case copyMarkdown:
		return export.ClipContent(r, export.Markdown)
	case copyCSV:
		return export.ClipContent(r, export.CSV)
	case copyTSV:
		return export.ClipContent(r, export.TSV)
	case copyJSON:
		return export.ClipContent(r, export.JSON)
	}
	return export.ClipContent(r, export.Text)
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
	return func() tea.Msg { return writeClip(c, what) }
}

// writeClip writes c to the clipboard, there and then: the body of a copy
// command.
func writeClip(c clip.Content, what string) clipDoneMsg {
	rich, err := clipWrite(c)
	return clipDoneMsg{what: what, text: c.Text, html: c.HTML != "", rich: rich, err: err}
}

// clipMax bounds an OSC 52 fallback: terminals drop oversized sequences
// silently, and a copy that loses its tail is worse than one that visibly
// did not happen.
const clipMax = 1 << 20

// clipDone reports a copy, falling back to OSC 52 when the local clipboard
// was unreachable.
func (m *Model) clipDone(msg clipDoneMsg) tea.Cmd {
	if msg.failed != nil {
		m.logf(logErr, "copy failed: %s", serr.StringFromErr(msg.failed))
		return nil
	}
	if msg.err == nil {
		how := ""
		switch {
		case msg.html && msg.rich:
			how = " — paste into Teams, Outlook or a doc for a formatted table"
		case msg.html:
			how = " — as aligned text (no HTML clipboard on this system)"
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
		note = " (as aligned text: a formatted table needs a local clipboard)"
	}
	m.logf(logOk, "copied %s via the terminal%s", msg.what, note)
	return tea.SetClipboard(msg.text)
}
