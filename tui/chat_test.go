package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/ai/aitest"
	"github.com/rohanthewiz/dbc/codehl"
	"github.com/rohanthewiz/dbc/userdata"
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
	for _, want := range []string{"SQL assistant inside dbc", `Connection "demo-sqlite" uses the SQLite driver`,
		"FROM cats ORDER BY age", "columns: id, name, breed, age, adopted", "Question: why is Oliver first?"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Whiskers") {
		t.Error("row values went to the agent without ai_rows")
	}
	tr := m.chat.transcriptText()
	if !strings.Contains(tr, `rows not sent — set ai_rows = true on connection "demo-sqlite"`) {
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

// A column hidden in the grid is left out of the rows the assistant gets, as
// copies leave it out; the model is told its name, and the chip says a
// column is missing before anything is sent.
func TestAssistantLeavesHiddenColumnsOut(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	m.cfg.Connections[0].AIRows = true
	m.cfg.AIContextRows = 3
	key(t, m, "ctrl+r")
	if m.grid.Hide(2, 2) != 1 || m.grid.colName(2) != "age" { // breed
		t.Fatal("setup: breed should be hidden")
	}
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	// the chip wraps rather than cutting the end of its note off
	findText(t, frame(m), "(1 column hidden)")

	typeText(t, m, "summarize")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	p := lastPrompt(t, f)
	for _, leak := range []string{"Tabby", "Siamese", "Sphynx", "| breed |"} {
		if strings.Contains(p, leak) {
			t.Errorf("hidden column's %q went to the agent:\n%s", leak, p)
		}
	}
	if tr := m.chat.transcriptText(); !strings.Contains(tr, "3 of 8 rows (1 column hidden)") {
		t.Errorf("transcript should say what went:\n%s", tr)
	}
	for _, want := range []string{"| id | name | age | adopted |",
		"The user hid this column in the grid, so it is left out above: breed."} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q:\n%s", want, p)
		}
	}

	// shown again, it goes again
	m.grid.ShowAll()
	typeText(t, m, "and now?")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	if p := lastPrompt(t, f); !strings.Contains(p, "| breed |") || strings.Contains(p, "The user hid") {
		t.Errorf("after show all:\n%s", p)
	}
}

// After a header sort the assistant gets the rows the user sees at the top,
// and hears that the order is the grid's.
func TestAssistantFollowsTheGridSort(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	m.cfg.Connections[0].AIRows = true
	m.cfg.AIContextRows = 3
	key(t, m, "ctrl+r") // … ORDER BY age: Oliver, Luna, Cleo first
	m.grid.Sort(3)
	m.grid.Sort(3) // age, descending
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	findText(t, frame(m), "(sorted by age desc)")

	typeText(t, m, "why are these the oldest?")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	p := lastPrompt(t, f)
	milo, simba, bella := strings.Index(p, "Milo"), strings.Index(p, "Simba"), strings.Index(p, "Bella")
	if milo < 0 || !(milo < simba && simba < bella) || strings.Contains(p, "Oliver") {
		t.Errorf("rows should be the grid's top 3 (Milo, Simba, Bella):\n%s", p)
	}
	if !strings.Contains(p, "sorted the result in the grid by age, descending") {
		t.Errorf("prompt should say the order is the grid's:\n%s", p)
	}

	// A context taken before a re-sort keeps its order: applySort rewrites
	// the grid's order in place, and a submitted question's context waits
	// on its schema lookup before it becomes a prompt. (The harness runs
	// that lookup synchronously, so this is checked on the context itself.)
	ctx, _ := m.chatContext("")
	m.grid.Sort(3) // back to the query's order
	if q := ai.Build("", ctx, false).Text; strings.Index(q, "Milo") < 0 || strings.Contains(q, "Oliver") {
		t.Errorf("a re-sort reached into an earlier context:\n%s", q)
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

// A fenced block is drawn in the editor's syntax colors on the code
// background, even where a multi-line construct (a Go raw string) and a
// wrapped row split it, and an unknown language stays plain.
func TestAgentCodeIsHighlighted(t *testing.T) {
	p := newChatPane()
	st := newStyles(themeDefault())
	rows := p.agentRows(st, "```go\nfunc f() string { return `a\nb` } // done\n```\n```text\nfunc\n```", 30)
	styleOf := func(text string) (Style, bool) {
		for _, r := range rows {
			for _, s := range r.segs {
				if s.text == text {
					return s.st, true
				}
			}
		}
		return Style{}, false
	}
	want := map[string]Style{
		"func":           codeStyle(st, codehl.Keyword),
		"return":         codeStyle(st, codehl.Keyword),
		"`a":             codeStyle(st, codehl.String), // the raw string's first line…
		"b`":             codeStyle(st, codehl.String), // …and its second
		"// done":        codeStyle(st, codehl.Comment),
		" f() string { ": st.chatCode,
	}
	for text, ws := range want {
		got, ok := styleOf(text)
		if !ok {
			t.Errorf("no segment %q", text)
			continue
		}
		if got != ws {
			t.Errorf("%q drawn %+v, want %+v", text, got, ws)
		}
		if got.Bg != st.chatCode.Bg {
			t.Errorf("%q left the code background", text)
		}
	}
	// the text block's lone "func" is the last row's only word: plain
	last := rows[len(rows)-1].segs
	if got := last[len(last)-1]; got.text != "func" || got.st != st.chatCode {
		t.Errorf("an untagged-language block should be plain, got %+v", got)
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
	if _, err := m.mgr.Run("demo-sqlite", "CREATE TABLE owners (id INTEGER PRIMARY KEY, cat_id INTEGER, email TEXT)"); err != nil {
		t.Fatal(err)
	}
	drive(t, m, nil, m.connectCmd("demo-sqlite")) // reload the catalog
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
	if _, err := m.mgr.Run("demo-sqlite", "CREATE TABLE owners (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	drive(t, m, nil, m.connectCmd("demo-sqlite"))
	if _, err := m.mgr.Run("demo-sqlite", "DROP TABLE owners"); err != nil {
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

// keepChats points the assistant's archive at a temp dir for one test.
func keepChats(t *testing.T, m *Model) string {
	t.Helper()
	m.chat.dir = t.TempDir()
	return m.chat.dir
}

func savedChats(t *testing.T, dir string) []userdata.ChatMeta {
	t.Helper()
	metas, err := userdata.ListChats(dir)
	if err != nil {
		t.Fatal(err)
	}
	return metas
}

// A conversation is saved as it goes, survives ⟲ new, is offered back in the
// empty pane, and reopens into a fresh session that says it remembers
// nothing — carrying on in the same file rather than a copy.
func TestAssistantKeepsConversations(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	dir := keepChats(t, m)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	typeText(t, m, "why is Oliver first?")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })

	saved := savedChats(t, dir)
	if len(saved) != 1 || saved[0].Title != "why is Oliver first?" || saved[0].Conn != "demo-sqlite" {
		t.Fatalf("after one answer, saved = %+v", saved)
	}
	id := saved[0].ID

	x, y := findText(t, frame(m), "⟲ new")
	click(t, m, x+1, y)
	if len(m.chat.msgs) != 0 {
		t.Fatalf("⟲ new should clear the pane:\n%s", m.chat.transcriptText())
	}
	if !strings.Contains(logText(m), "saved the assistant conversation") {
		t.Error("⟲ new should say the conversation was kept")
	}
	c := frame(m)
	findText(t, c, "recent conversations")
	x, y = findText(t, c, "◷ why is Oliver first?")

	click(t, m, x+2, y)
	tr := m.chat.transcriptText()
	if !strings.Contains(tr, "> why is Oliver first?") || !strings.Contains(tr, "echo: Question: why is Oliver first?") {
		t.Errorf("the saved transcript should be back:\n%s", tr)
	}
	if !strings.Contains(tr, "reopened conversation") || !strings.Contains(tr, "no memory of it") {
		t.Errorf("the pane should say the agent does not remember it:\n%s", tr)
	}
	if m.chat.archiveID != id {
		t.Errorf("reopened conversation should continue file %s, has %q", id, m.chat.archiveID)
	}

	// the follow-up opens a fresh session, so it carries the preamble again
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	typeText(t, m, "and then?")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	if p := lastPrompt(t, f); !strings.Contains(p, "SQL assistant inside dbc") {
		t.Errorf("a reopened conversation's first question should start a new session:\n%s", p)
	}
	saved = savedChats(t, dir)
	if len(saved) != 1 || saved[0].ID != id || saved[0].Count <= 4 {
		t.Errorf("the reopened conversation should be saved back to the same file: %+v", saved)
	}
}

// Quitting saves the conversation, even with an answer still in flight.
func TestAssistantSavesOnQuit(t *testing.T) {
	fakeAssistant(t, nil)
	m := newTestModel(t)
	dir := keepChats(t, m)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	typeText(t, m, "SLOW question")
	key(t, m, "enter")
	if len(savedChats(t, dir)) != 0 {
		t.Fatal("nothing should be saved before the answer")
	}
	m.shutdown()
	saved := savedChats(t, dir)
	if len(saved) != 1 || saved[0].Title != "SLOW question" {
		t.Errorf("quitting should save the conversation: %+v", saved)
	}
}

// A pane that was opened but never asked anything leaves nothing behind.
func TestAssistantDoesNotSaveAnEmptyConversation(t *testing.T) {
	fakeAssistant(t, nil)
	m := newTestModel(t)
	dir := keepChats(t, m)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	drive(t, m, nil, m.newChat())
	m.shutdown()
	if saved := savedChats(t, dir); len(saved) != 0 {
		t.Errorf("saved %+v", saved)
	}
}

// Past the few the empty pane shows, the rest are in the Recent
// conversations list, which the transcript's menu also opens.
func TestAssistantRecentConversationsList(t *testing.T) {
	fakeAssistant(t, nil)
	m := newTestModel(t)
	dir := keepChats(t, m)
	base := time.Now().Add(-time.Hour)
	for i := range 7 {
		at := base.Add(time.Duration(i) * time.Minute)
		q := fmt.Sprintf("question %d", i)
		err := userdata.SaveChat(dir, userdata.Chat{ID: userdata.NewChatID(at), Updated: at,
			Title: q, Conn: "other", Msgs: []userdata.ChatMsg{{Role: "user", Text: q}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	key(t, m, "ctrl+a")
	c := frame(m)
	findText(t, c, "◷ question 6")
	findText(t, c, "other") // a conversation about another connection says so
	x, y := findText(t, c, "all 7 recent…")
	click(t, m, x, y)
	if _, ok := m.modal.(*recentChatsModal); !ok {
		t.Fatalf("modal = %T", m.modal)
	}
	key(t, m, "esc")

	rightClick(t, m, m.chat.transcript.X+2, m.chat.transcript.Y+12)
	mx, my := findText(t, frame(m), "Recent conversations…")
	click(t, m, mx, my)
	key(t, m, "down")
	key(t, m, "enter")
	if m.modal != nil || !strings.Contains(m.chat.transcriptText(), "> question 5") {
		t.Errorf("picking the second row should open question 5:\n%s", m.chat.transcriptText())
	}
}

// The empty pane's "recent conversations" header folds its rows away and
// back, keeping the count while folded, and the fold is saved with the
// layout so the next run opens folded.
func TestRecentConversationsFold(t *testing.T) {
	fakeAssistant(t, nil)
	m := newTestModel(t)
	m.layoutFile = filepath.Join(t.TempDir(), "tui-layout.json")
	dir := keepChats(t, m)
	seedChats(t, dir, 7)
	key(t, m, "ctrl+a")
	c := frame(m)
	findText(t, c, "◷ question 6")
	x, y := findText(t, c, "▾ recent conversations")

	click(t, m, x+2, y)
	c = frame(m)
	findText(t, c, "▸ recent conversations · 7")
	for _, gone := range []string{"◷ question 6", "all 7 recent…"} {
		if strings.Contains(c.String(), gone) {
			t.Errorf("folded, the pane still shows %q:\n%s", gone, c.String())
		}
	}
	findText(t, c, "Ask about the query in the editor") // the hints move up
	m.saveLayout()

	m2 := newTestModel(t)
	m2.restoreLayout(userdata.LoadLayout(m.layoutFile))
	m2.chat.dir = dir
	key(t, m2, "ctrl+a")
	x, y = findText(t, frame(m2), "▸ recent conversations · 7")

	click(t, m2, x+2, y)
	c = frame(m2)
	findText(t, c, "▾ recent conversations")
	findText(t, c, "◷ question 6")
	findText(t, c, "all 7 recent…")
}

// seedChats saves n conversations "question 0" … "question n-1", newest last.
func seedChats(t *testing.T, dir string, n int) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	for i := range n {
		at := base.Add(time.Duration(i) * time.Minute)
		q := fmt.Sprintf("question %d", i)
		err := userdata.SaveChat(dir, userdata.Chat{ID: userdata.NewChatID(at), Updated: at,
			Title: q, Conn: "demo-sqlite", Msgs: []userdata.ChatMsg{{Role: "user", Text: q}}})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func savedTitles(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	for _, c := range savedChats(t, dir) {
		out = append(out, c.Title)
	}
	return strings.Join(out, ",")
}

// In the list, d arms the row and d again deletes it; any other key between
// the two disarms, and Esc disarms before it closes.
func TestRecentConversationsDeleteTakesTwoPresses(t *testing.T) {
	fakeAssistant(t, nil)
	m := newTestModel(t)
	dir := keepChats(t, m)
	seedChats(t, dir, 3)
	key(t, m, "ctrl+a")
	m.openRecentChats()

	key(t, m, "d")
	findText(t, frame(m), "delete? press d again")
	key(t, m, "down") // moving disarms
	key(t, m, "d")
	key(t, m, "esc") // disarms, does not close
	if m.modal == nil {
		t.Fatal("Esc with a delete armed should only disarm")
	}
	if got := savedTitles(t, dir); got != "question 2,question 1,question 0" {
		t.Fatalf("nothing should be deleted yet: %s", got)
	}

	key(t, m, "d")
	key(t, m, "d")
	if got := savedTitles(t, dir); got != "question 2,question 0" {
		t.Errorf("d d on the second row should delete question 1: %s", got)
	}
	if !strings.Contains(logText(m), `deleted the saved conversation "question 1"`) {
		t.Errorf("the log should name what was deleted:\n%s", logText(m))
	}
	if got := modalTitles(t, m); got != "question 2,question 0" {
		t.Errorf("the deleted row should leave the list: %s", got)
	}

	// deleting the last ones closes the list
	key(t, m, "d")
	key(t, m, "d")
	key(t, m, "d")
	key(t, m, "d")
	if m.modal != nil || len(savedChats(t, dir)) != 0 {
		t.Errorf("modal=%T left=%d", m.modal, len(savedChats(t, dir)))
	}
}

// Right-clicking a row, in the list or in the empty pane, offers Delete.
func TestRecentConversationsDeleteFromMenus(t *testing.T) {
	fakeAssistant(t, nil)
	m := newTestModel(t)
	dir := keepChats(t, m)
	seedChats(t, dir, 3)
	key(t, m, "ctrl+a")

	x, y := findText(t, frame(m), "◷ question 2")
	rightClick(t, m, x+2, y)
	mx, my := findText(t, frame(m), "Delete conversation")
	click(t, m, mx, my)
	if got := savedTitles(t, dir); got != "question 1,question 0" {
		t.Fatalf("pane row delete: %s", got)
	}
	if strings.Contains(frame(m).Text(), "◷ question 2") {
		t.Error("the pane should stop offering the deleted conversation")
	}

	m.openRecentChats()
	frame(m)
	lv := m.modal.(*recentChatsModal).lst.view
	rightClick(t, m, lv.X+2, lv.Y+1) // the second row: question 0
	if got := modalTitles(t, m); got != "question 1,question 0" {
		t.Fatalf("list rows: %s", got)
	}
	mx, my = findText(t, frame(m), "Delete conversation")
	click(t, m, mx, my)
	if got := savedTitles(t, dir); got != "question 1" {
		t.Fatalf("list row delete: %s", got)
	}
	if got := modalTitles(t, m); got != "question 1" {
		t.Errorf("the list should stay open with the rest: %s", got)
	}
}

// modalTitles lists the rows of the open Recent conversations list.
func modalTitles(t *testing.T, m *Model) string {
	t.Helper()
	rc, ok := m.modal.(*recentChatsModal)
	if !ok {
		t.Fatalf("modal = %T, want the Recent conversations list", m.modal)
	}
	var out []string
	for _, it := range rc.lst.items {
		out = append(out, it.label)
	}
	return strings.Join(out, ",")
}

// The transcript's menu deletes the live conversation: its file goes, the
// pane clears without saving it back, and the next question starts a fresh
// session that never saw it.
func TestAssistantDeletesTheLiveConversation(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	dir := keepChats(t, m)
	seedChats(t, dir, 1)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })

	// with nothing on screen the row is offered but disabled, and says why
	rightClick(t, m, m.chat.transcript.X+2, m.chat.transcript.Y+12)
	mx, my := findText(t, frame(m), "Delete this conversation")
	click(t, m, mx, my)
	if !strings.Contains(logText(m), "no conversation to delete") || len(savedChats(t, dir)) != 1 {
		t.Fatalf("an empty pane has nothing to delete:\n%s", logText(m))
	}

	typeText(t, m, "why is Oliver first?")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	if got := savedTitles(t, dir); got != "why is Oliver first?,question 0" {
		t.Fatalf("after one answer: %s", got)
	}

	rightClick(t, m, m.chat.transcript.X+2, m.chat.transcript.Y+2)
	mx, my = findText(t, frame(m), "Delete this conversation")
	click(t, m, mx, my)
	if got := savedTitles(t, dir); got != "question 0" {
		t.Errorf("only the live conversation should be deleted: %s", got)
	}
	if len(m.chat.msgs) != 0 || m.chat.archiveID != "" {
		t.Errorf("the pane should clear and forget the file:\n%s", m.chat.transcriptText())
	}
	if lg := logText(m); !strings.Contains(lg, `deleted the conversation "why is Oliver first?"`) ||
		strings.Contains(lg, "saved the assistant conversation") {
		t.Errorf("the log should name the deletion and not claim a save:\n%s", lg)
	}
	findText(t, frame(m), "◷ question 0") // the empty pane offers the rest

	// quitting now saves nothing, and a follow-up is a new session
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	typeText(t, m, "and then?")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	if p := lastPrompt(t, f); !strings.Contains(p, "SQL assistant inside dbc") {
		t.Errorf("the next question should start a new session:\n%s", p)
	}
	m.shutdown()
	if got := savedTitles(t, dir); got != "and then?,question 0" {
		t.Errorf("the deleted conversation should stay gone: %s", got)
	}
}

// fakeSignIn makes the pane's sign-in run against f's scripted language
// server, for the duration of a test.
func fakeSignIn(t *testing.T, f *aitest.Fake) {
	t.Helper()
	prev := beginSignIn
	beginSignIn = f.SignIn
	t.Cleanup(func() { beginSignIn = prev })
}

// A signed-out Copilot user asks a question: the handshake is refused, the
// pane offers ⎆ sign in, the device code is shown, copied and its page
// opened, and once GitHub confirms the pane reconnects and the question
// that was refused goes out.
func TestAssistantSignsInToCopilot(t *testing.T) {
	f := fakeAssistant(t, nil)
	f.SignedOut = true
	fakeSignIn(t, f)
	m := newTestModel(t)
	m.chat.open, m.focus = true, focusChat
	typeText(t, m, "hello")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return m.chat.state == chatDead })

	tr := m.chat.transcriptText()
	for _, want := range []string{"Authentication required", "once you are signed in"} {
		if !strings.Contains(tr, want) {
			t.Errorf("transcript lacks %q:\n%s", want, tr)
		}
	}
	if strings.Contains(tr, "⟲ new to try again") {
		t.Error("with sign-in on offer, ⟲ new is not the next step")
	}
	x, y := findText(t, frame(m), "⎆ sign in to Copilot")
	click(t, m, x+1, y) // runs the flow to the end: the fake confirms at once

	if f.SignIns() != 1 {
		t.Errorf("device codes handed out = %d", f.SignIns())
	}
	tr = m.chat.transcriptText()
	if !strings.Contains(tr, "enter code "+aitest.FakeUserCode+" at https://github.com/login/device") {
		t.Errorf("the code is not in the transcript:\n%s", tr)
	}
	if got := lastClip(t).Text; got != aitest.FakeUserCode {
		t.Errorf("clipboard = %q", got)
	}
	if len(openLog) != 1 || openLog[0] != "https://github.com/login/device" {
		t.Errorf("opened = %v", openLog)
	}
	if !strings.Contains(tr, "signed in to GitHub as octocat") {
		t.Errorf("no success line:\n%s", tr)
	}
	pumpChat(t, m, func() bool { return m.chat.state == chatReady && !m.chat.streaming })
	if !strings.Contains(lastPrompt(t, f), "Question: hello") {
		t.Errorf("the refused question was not sent after sign-in: %q", lastPrompt(t, f))
	}
	if m.chat.offerSignIn() {
		t.Error("the chip should be gone once signed in")
	}
}

// A GitHub account without Copilot says so and offers sign-in again (for a
// different account).
func TestAssistantSignInWithoutSubscription(t *testing.T) {
	f := fakeAssistant(t, nil)
	f.SignedOut = true
	f.SignInResult = ai.AuthStatus{Status: "NotAuthorized", User: "octocat"}
	fakeSignIn(t, f)
	m := newTestModel(t)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatDead })
	x, y := findText(t, frame(m), "⎆ sign in to Copilot")
	click(t, m, x+1, y)
	if tr := m.chat.transcriptText(); !strings.Contains(tr, "no Copilot subscription") {
		t.Errorf("transcript:\n%s", tr)
	}
	if m.chat.state != chatDead || !m.chat.offerSignIn() {
		t.Error("still signed out: the chip should be back")
	}
}

// A sign-in that is waiting on GitHub can be cancelled, and its late answer
// changes nothing.
func TestAssistantSignInCancel(t *testing.T) {
	f := fakeAssistant(t, nil)
	f.SignedOut = true
	fakeSignIn(t, f)
	m := newTestModel(t)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatDead })

	// Start the flow but hold back the wait's answer, as if the user has not
	// entered the code yet: land the code, but do not run the wait it
	// returns.
	code := m.chatSignIn()().(chatSignInCodeMsg)
	_ = m.chatSignInCode(code)
	if m.chat.signState != signInWaiting {
		t.Fatalf("sign-in state = %v", m.chat.signState)
	}
	c := frame(m)
	findText(t, c, "waiting on GitHub — code "+aitest.FakeUserCode)
	x, y := findText(t, c, "✕ cancel")
	seq := m.chat.signSeq
	click(t, m, x+1, y)
	if m.chat.signState != signInNone || !m.chat.offerSignIn() {
		t.Error("cancel should end the sign-in and offer it again")
	}
	drive(t, m, chatSignInDoneMsg{seq: seq, st: ai.AuthStatus{Status: "OK", User: "octocat"}})
	if strings.Contains(m.chat.transcriptText(), "signed in to GitHub") {
		t.Error("a cancelled sign-in's answer was acted on")
	}
}

// The transcript menu offers sign-in for Copilot even when nothing was
// refused; a signed-in account is told so and nothing else changes.
func TestAssistantMenuSignInWhenSignedIn(t *testing.T) {
	f := fakeAssistant(t, nil)
	fakeSignIn(t, f)
	m := newTestModel(t)
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })
	drive(t, m, nil, m.chatSignIn())
	if tr := m.chat.transcriptText(); !strings.Contains(tr, "signed in to GitHub as octocat") {
		t.Errorf("transcript:\n%s", tr)
	}
	if f.SignIns() != 0 || len(openLog) != 0 {
		t.Error("an account already signed in needs no code and no browser")
	}
	if m.chat.state != chatReady {
		t.Errorf("state = %v", m.chat.state)
	}
}

// tempScript writes a script into a temp dir and returns its path.
func tempScript(t *testing.T, name, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// After a script run the assistant is asked about the script, as dbc web's
// script tab asks (N-137): its source fenced as Go and named, the
// connections, its error, and the sdb API summary on the conversation's
// first script question only. Once the user is back in the editor (a caret
// move), the statement under the caret is the question's subject again.
func TestAssistantAsksAboutTheScriptThatRan(t *testing.T) {
	f := fakeAssistant(t, nil)
	m := newTestModel(t)
	path := tempScript(t, "broken.go", `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	_, err := s.Query("demo-sqlite", "SELECT * FROM no_such_table")
	return err
}
`)
	drive(t, m, nil, m.runScript(path))
	if !strings.Contains(m.ws.LastErr(), "no_such_table") {
		t.Fatalf("the script should have failed on no_such_table; log: %s", logText(m))
	}
	key(t, m, "ctrl+a")
	pumpChat(t, m, func() bool { return m.chat.state == chatReady })

	// the chip forecasts the script, the error and the API before sending
	if c := frame(m); !strings.Contains(c.String(), "script") || !strings.Contains(c.String(), "sdb API") {
		t.Errorf("the chip should say the script and the sdb API go:\n%s", c.String())
	}
	typeText(t, m, "why did it fail?")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	p := lastPrompt(t, f)
	for _, want := range []string{"The Go script in question (broken.go)", "```go", `s.Query("demo-sqlite"`,
		"Running it failed with", "no_such_table", "The sdb API, by signature", "demo-sqlite (sqlite)"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "The SQL in question") {
		t.Errorf("the editor's SQL should not go with a script question:\n%s", p)
	}

	// the API summary went once; the next script question leaves it out
	typeText(t, m, "and now?")
	key(t, m, "enter")
	pumpChat(t, m, func() bool { return !m.chat.streaming })
	if p := lastPrompt(t, f); !strings.Contains(p, "broken.go") || strings.Contains(p, "The sdb API, by signature") {
		t.Errorf("the second script question should carry the script but not the API again:\n%s", p)
	}

	// back in the editor: its statement is "this query" again
	m.focus = focusEditor
	key(t, m, "right")
	if ctx, _ := m.chatContext("q"); ctx.Script != "" || !strings.Contains(ctx.Query, "FROM cats") {
		t.Errorf("after a caret move the editor's statement should go, got script %q query %q", ctx.Script, ctx.Query)
	}
}

// A script that showed a result sends that result (rows with ai_rows), and
// running a statement from the editor afterwards ends the script's turn even
// without touching the editor — the workspace forgets the script.
func TestAssistantScriptResultThenAStatementRun(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Connections[0].AIRows = true
	drive(t, m, nil, m.runScript("../scripts/loop_params.go"))
	ctx, _ := m.chatContext("why these cats?")
	if ctx.Script != "loop_params.go" || !strings.Contains(ctx.Query, "func Run(s *sdb.S) error") {
		t.Fatalf("the script should be the subject: script %q query %q", ctx.Script, ctx.Query)
	}
	if ctx.Err != "" || len(ctx.Columns) == 0 || len(ctx.Rows) == 0 {
		t.Errorf("its shown result should go: err %q columns %v rows %d", ctx.Err, ctx.Columns, len(ctx.Rows))
	}
	if ctx.ScriptAPI == "" {
		t.Error("the conversation's first script question should carry the sdb API")
	}

	key(t, m, "ctrl+r")
	if m.ws.LastScript() != "" {
		t.Fatal("a statement run should replace the script as the last run")
	}
	if ctx, _ := m.chatContext("q"); ctx.Script != "" || ctx.ScriptAPI != "" {
		t.Errorf("after a statement run the script should not go: %q", ctx.Script)
	}
}

// The script's source is read again when the file changes on disk, so an
// edit made outside dbc after the run is what the next question carries.
func TestAssistantRereadsAnEditedScript(t *testing.T) {
	m := newTestModel(t)
	src := "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(s *sdb.S) error {\n\treturn nil\n}\n"
	path := tempScript(t, "edited.go", src)
	drive(t, m, nil, m.runScript(path))
	if ctx, _ := m.chatContext(""); ctx.Query != src {
		t.Fatalf("query = %q, want the file's source", ctx.Query)
	}
	edited := strings.Replace(src, "return nil", "s.Print(\"hi\")\n\treturn nil", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if ctx, _ := m.chatContext(""); ctx.Query != edited {
		t.Errorf("query = %q, want the edited source", ctx.Query)
	}
	// a script trashed since it ran still goes by name, without source
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if ctx, _ := m.chatContext(""); ctx.Script != "edited.go" || ctx.Query != "" {
		t.Errorf("a gone script: script %q query %q", ctx.Script, ctx.Query)
	}
}
