package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/ai/aitest"
)

// fakeAssistant makes the assistant start a scripted agent instead of a real
// one, for the duration of a test.
func fakeAssistant(t *testing.T, reply func(prompt string) []string) *aitest.Fake {
	t.Helper()
	f := &aitest.Fake{Reply: reply}
	prev := startChat
	startChat = func(a ai.Agent, opt ai.Options) *ai.Chat { return aitest.Start(a, opt, f) }
	t.Cleanup(func() { startChat = prev })
	return f
}

// pumpChat feeds the assistant's events to the model until cond holds.
// The harness does not re-arm the event pump by itself (it would block
// forever once the agent goes quiet), so tests pump it explicitly.
func pumpChat(t *testing.T, m *Model, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !cond() {
		ch := make(chan tea.Msg, 1)
		go func() { ch <- waitChat(m.chat.c, m.chat.gen)() }()
		select {
		case msg := <-ch:
			if msg == nil {
				t.Fatal("the assistant closed")
			}
			drive(t, m, msg)
		case <-deadline:
			t.Fatalf("assistant condition not met; transcript:\n%s", m.chat.transcriptText())
		}
	}
}

func lastPrompt(t *testing.T, f *aitest.Fake) string {
	t.Helper()
	p := f.Prompts()
	if len(p) == 0 {
		t.Fatal("no prompt reached the agent")
	}
	return p[len(p)-1]
}

// Ask a question about a query that has run: the query goes, the rows do
// not (this connection has not set ai_rows), and the transcript says so.
func TestAssistantSendsQueryButWithholdsRows(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	key(t, m, "ctrl+a")
	if !m.chat.open || m.focus != focusChat {
		t.Fatal("Ctrl+A opens the assistant with the keyboard in it")
	}
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })

	typeText(t, m, "why is Oliver first?")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })

	p := lastPrompt(t, f)
	for _, want := range []string{"SQL assistant inside dbc", `Connection "demo" uses the SQLite driver`,
		"FROM cats ORDER BY age", "columns: id, name, breed, age, adopted", "Question: why is Oliver first?"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Whiskers") {
		t.Error("row values went to the agent without ai_rows")
	}
	tr := m.chat.transcriptText()
	if !strings.Contains(tr, `rows not sent — set ai_rows = true on connection "demo"`) {
		t.Errorf("the transcript should say rows were withheld:\n%s", tr)
	}
	if !strings.Contains(tr, "echo: Question: why is Oliver first?") {
		t.Errorf("the answer should stream into the transcript:\n%s", tr)
	}
}

// With ai_rows on, up to ai_context_rows rows go along.
func TestAssistantSendsRowsWhenTheConnectionOptsIn(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	m.cfg.Connections[0].AIRows = true
	m.cfg.AIContextRows = 3
	key(t, m, "ctrl+r")
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	typeText(t, m, "summarize")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	p := lastPrompt(t, f)
	if !strings.Contains(p, "The first 3 of its 8 result rows") || strings.Count(p, "| Tabby |")+strings.Count(p, "| Siamese |")+strings.Count(p, "| Sphynx |") != 3 {
		t.Errorf("prompt:\n%s", p)
	}
}

// A question typed before the handshake finishes waits for it, not lost.
func TestAssistantQueuesAQuestionDuringTheHandshake(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	m.chat.open, m.focus = true, focusChat
	typeText(t, m, "hello")
	key(t, m, "enter") // starts the agent and queues the prompt
	pumpChat(t, m, func() bool { return m.chat.state == chatReady && !m.chat.streaming })
	if !strings.Contains(lastPrompt(t, f), "Question: hello") {
		t.Errorf("queued prompt = %q", lastPrompt(t, f))
	}
}

// SQL in an answer gets ⤓ insert, which drops it into the editor.
func TestAssistantInsertPutsSQLInTheEditor(t *testing.T) {
	fakeAssistant(t, func(string) []string {
		return []string{"Try this:\n```sql\nSELECT breed, ", "count(*) FROM cats GROUP BY breed\n```\nIt groups."}
	})
	m := newTestModel(t)
	m.editor.SetText("")
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	typeText(t, m, "count by breed")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })

	c := frame(m)
	x, y := findText(t, c, "⤓ insert")
	findText(t, c, "SELECT breed, count(*)") // the pane may wrap the rest
	findText(t, c, "⧉ copy reply")
	click(t, m, x+1, y)
	if got := m.editor.Text(); got != "SELECT breed, count(*) FROM cats GROUP BY breed" {
		t.Errorf("editor = %q", got)
	}
	if m.focus != focusEditor {
		t.Error("inserting hands the keyboard back to the editor")
	}
}

// Ctrl+K while an answer streams asks the agent to stop.
func TestAssistantStop(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	typeText(t, m, "SLOW please")
	key(t, m, "enter")
	if !m.chat.streaming {
		t.Fatal("the answer should be in flight")
	}
	findText(t, frame(m), "■ stop")
	key(t, m, "ctrl+k")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	if f.Cancels() != 1 || !strings.Contains(m.chat.transcriptText(), "— stopped") {
		t.Errorf("cancels=%d transcript:\n%s", f.Cancels(), m.chat.transcriptText())
	}
}

// The context chip toggles context off; then only the question goes.
func TestAssistantContextToggle(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	x, y := findText(t, frame(m), "[✓] with:")
	click(t, m, x, y)
	findText(t, frame(m), "[ ] context off")
	typeText(t, m, "general question")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	if p := lastPrompt(t, f); strings.Contains(p, "FROM cats") {
		t.Errorf("context was off, but the query went:\n%s", p)
	}
}

// The model menu lists the roster and switches the session's model.
func TestAssistantModelMenu(t *testing.T) {
	fakeAssistant(t, nil)
	m := newTestModel(t)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	x, y := findText(t, frame(m), "✦ Copilot · Fast Model ▾")
	click(t, m, x, y)
	mx, my := findText(t, frame(m), "Smart Model")
	click(t, m, mx, my)
	if m.chat.modelID != "smart" {
		t.Errorf("model = %q", m.chat.modelID)
	}
}

// "Ask about this result" from the grid menu drafts a question, unsent.
func TestAskAboutDraftsAQuestion(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	x, y := findText(t, frame(m), "Oliver")
	rightClick(t, m, x, y)
	mx, my := findText(t, frame(m), "Ask the assistant about this result")
	click(t, m, mx, my)
	if !m.chat.open || !strings.HasPrefix(m.chat.input.Text(), "Explain this result") {
		t.Errorf("open=%v input=%q", m.chat.open, m.chat.input.Text())
	}
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	if len(f.Prompts()) != 0 {
		t.Error("a drafted question must not be sent until the user sends it")
	}
}

// Code blocks, inline code and bold render without their markers.
func TestAgentMarkdownRendering(t *testing.T) {
	p := newChatPane()
	st := newStyles(themeDefault())
	rows := p.agentRows(st, "# Plan\nUse **an index** on `breed`.\n```\nCREATE INDEX i ON cats (breed);\n```", 60)
	var text []string
	for _, r := range rows {
		var b strings.Builder
		for _, s := range r.segs {
			b.WriteString(s.text)
		}
		text = append(text, b.String())
	}
	joined := strings.Join(text, "\n")
	for _, want := range []string{"Plan", "Use an index on breed.", "⤓ insert", " CREATE INDEX i ON cats (breed);"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "**") || strings.Contains(joined, "`") || strings.Contains(joined, "```") {
		t.Errorf("markers should be consumed:\n%s", joined)
	}
}

// The columns of the tables the query names go with the question — without
// ai_rows, since schema is not row data — and the note says so.
func TestAssistantSendsTheQuerysSchema(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })

	typeText(t, m, "add the owner")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })

	p := lastPrompt(t, f)
	if !strings.Contains(p, "- cats: id INTEGER, name TEXT, breed TEXT, age INTEGER, adopted BOOLEAN\n") {
		t.Errorf("prompt should describe cats:\n%s", p)
	}
	if strings.Contains(p, "Whiskers") {
		t.Error("schema must not bring rows with it")
	}
	if tr := m.chat.transcriptText(); !strings.Contains(tr, "sent: schema of cats, query") {
		t.Errorf("the note should name the schema:\n%s", tr)
	}
}

// With nothing in the editor, the question's own words pick the tables, and
// the context chip forecasts them while the question is being typed.
func TestAssistantFindsTablesNamedInTheQuestion(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	if _, err := m.mgr.Run("demo", "CREATE TABLE owners (id INTEGER PRIMARY KEY, cat_id INTEGER, email TEXT)"); err != nil {
		t.Fatal(err)
	}
	drive(t, m, nil, m.connectCmd("demo")) // reload the catalog
	m.editor.SetText("")
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })

	typeText(t, m, "join owners to cats")
	if fr := frame(m).Text(); !strings.Contains(fr, "with: schema of owners, cats") {
		t.Errorf("the chip should forecast the schema:\n%s", fr)
	}
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	p := lastPrompt(t, f)
	for _, want := range []string{"- owners: id INTEGER, cat_id INTEGER, email TEXT\n", "- cats: id INTEGER"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q:\n%s", want, p)
		}
	}
}

// Context off means the question alone: no schema either.
func TestAssistantContextOffSendsNoSchema(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	m.chat.attach = false
	typeText(t, m, "about cats")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	if p := lastPrompt(t, f); strings.Contains(p, "catalog") || !strings.HasSuffix(p, "editor.\n\nQuestion: about cats") {
		t.Errorf("prompt = %q", p)
	}
}

// A table the catalog listed at connect but can no longer describe (dropped
// since) is left out, and the note does not claim its schema went.
func TestAssistantSkipsATableThatIsGone(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	if _, err := m.mgr.Run("demo", "CREATE TABLE owners (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	drive(t, m, nil, m.connectCmd("demo"))
	if _, err := m.mgr.Run("demo", "DROP TABLE owners"); err != nil {
		t.Fatal(err)
	}
	m.editor.SetText("")
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	typeText(t, m, "who are the owners")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	if p := lastPrompt(t, f); strings.Contains(p, "catalog") {
		t.Errorf("a dropped table has no schema to send:\n%s", p)
	}
	if tr := m.chat.transcriptText(); !strings.Contains(tr, "sent: question only") {
		t.Errorf("note should not mention schema:\n%s", tr)
	}
}
