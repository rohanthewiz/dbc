package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Frame cost. Bubble Tea calls View after every message, and the TUI asks
// for every pointer motion (MouseModeAllMotion), so what one frame costs is
// paid on each mouse move, each streamed chat delta and each s.Print. These
// set up the sizes N-278 named — a 3.5 MB buffer, a full 2,000-line log, a
// long assistant transcript — and time a message plus its frame.

// bigSQL is a migration-sized buffer: about n bytes of short statements,
// with comments, quoted strings and a semicolon inside a literal, so the
// splitter and lexer have real work per byte.
func bigSQL(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "-- step %d\nCREATE TABLE t%d (id integer primary key, name text not null, note text default 'x;y');\n", i, i)
		fmt.Fprintf(&b, "INSERT INTO t%d (id, name) VALUES (1, 'a'), (2, 'b');\n", i)
	}
	return b.String()
}

// fillLog writes logMaxLines lines to the shown log, one in four long
// enough to wrap at any pane width.
func fillLog(m *Model) {
	for i := range logMaxLines {
		text := fmt.Sprintf("line %d: ran statement %d in 1.2ms", i, i)
		if i%4 == 0 {
			text += " — " + strings.Repeat("a fairly long notice from the server ", 8)
		}
		m.log(logInfo, text)
	}
}

// fillChat opens the assistant with a transcript of n questions and
// answers, each answer a paragraph and a 30-line SQL block.
func fillChat(m *Model, n int) {
	m.chat.open = true
	var code strings.Builder
	for i := range 30 {
		fmt.Fprintf(&code, "SELECT c%d, count(*) FROM orders WHERE note = 'it''s %d' GROUP BY 1;\n", i, i)
	}
	for i := range n {
		m.chat.add(roleUser, fmt.Sprintf("why is query %d slow when the table is large?", i))
		m.chat.add(roleAgent, "It reads **every** row: the `WHERE` has no index to use. "+
			strings.Repeat("Add one and the planner can seek instead of scanning. ", 6)+
			"\n\n```sql\n"+code.String()+"```\n")
	}
}

// frameModel is a test model at 200×50 with the editor holding buf.
func frameModel(tb testing.TB, buf string) *Model {
	m := newTestModel(tb)
	drive(tb, m, tea.WindowSizeMsg{Width: 200, Height: 50})
	m.editor.SetText(buf)
	m.focus = focusEditor
	return m
}

// motion is one pointer move and the frame it draws: what hovering costs.
func motion(m *Model, i int) {
	m.Update(tea.MouseMotionMsg{X: 40 + i%20, Y: 20})
	m.View()
}

// BenchmarkFrameBigBuffer is a mouse move over a 3.5 MB buffer with a full
// log: before N-278 each frame joined the buffer into one string about four
// times and split it into statements twice.
func BenchmarkFrameBigBuffer(b *testing.B) {
	m := frameModel(b, bigSQL(3_500_000))
	fillLog(m)
	motion(m, 0)
	i := 0
	for b.Loop() {
		motion(m, i)
		i++
	}
}

// BenchmarkFrameLog is a mouse move with only the full log on screen (the
// editor holds the test's one-line query): each frame re-wrapped all 2,000
// lines and formatted each timestamp.
func BenchmarkFrameLog(b *testing.B) {
	m := frameModel(b, "SELECT 1")
	fillLog(m)
	motion(m, 0)
	i := 0
	for b.Loop() {
		motion(m, i)
		i++
	}
}

// BenchmarkFrameChat is a mouse move with the assistant open on a long
// transcript: each frame re-wrapped every message and re-lexed every code
// block.
func BenchmarkFrameChat(b *testing.B) {
	m := frameModel(b, "SELECT 1")
	fillChat(m, 60)
	motion(m, 0)
	i := 0
	for b.Loop() {
		motion(m, i)
		i++
	}
}

// BenchmarkFrameChatDelta is one streamed chunk of an answer landing on a
// long transcript, and its frame.
func BenchmarkFrameChatDelta(b *testing.B) {
	m := frameModel(b, "SELECT 1")
	fillChat(m, 60)
	m.chat.streaming = true
	m.chat.appendAgent("Starting an answer. ")
	m.View()
	for b.Loop() {
		m.chat.appendAgent("more words ")
		m.Update(tea.MouseMotionMsg{X: 40, Y: 20}) // any message: the delta's own is chatEventMsg
		m.View()
	}
}

// BenchmarkFrameLogLine is one log line landing on a full log, and its
// frame — what each s.Print cost before prints were coalesced.
func BenchmarkFrameLogLine(b *testing.B) {
	m := frameModel(b, "SELECT 1")
	fillLog(m)
	m.View()
	i := 0
	for b.Loop() {
		m.Update(tea.MouseMotionMsg{X: 40, Y: 20})
		m.log(logInfo, fmt.Sprintf("printed %d", i))
		m.View()
		i++
	}
}

// BenchmarkFrameChatBigBuffer is a mouse move with the assistant open over
// a 3.5 MB buffer: its context chip asks the workspace for the chat context
// on every frame, which picks the statement under the caret.
func BenchmarkFrameChatBigBuffer(b *testing.B) {
	m := frameModel(b, bigSQL(3_500_000))
	fillChat(m, 60)
	motion(m, 0)
	i := 0
	for b.Loop() {
		motion(m, i)
		i++
	}
}

// BenchmarkFrameInspect is a mouse move over the Inspect dialog showing a
// 1 MB value: each frame (and each hover's hit test, through modalRect)
// split and wrapped the whole value (N-325).
func BenchmarkFrameInspect(b *testing.B) {
	m := frameModel(b, "SELECT 1")
	m.openModal(&inspectModal{value: strings.Repeat(`{"key": "a value", "n": 12345},`+"\n", 32_000), hoverBtn: -1})
	motion(m, 0)
	i := 0
	for b.Loop() {
		motion(m, i)
		i++
	}
}
