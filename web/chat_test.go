package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/ai/aitest"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/userdata"
)

// The assistant pane's server half, against a scripted agent (aitest.Fake):
// what reaches the model, the transcript on the stream, stop, the archive,
// sign-in. The prompt assertions are the TUI's (tui/chat_test.go), since the
// two share workspace.ChatContext and ai.Build and must send the same.

// chatEnv is a test server whose assistant is f, keeping conversations in
// a temp dir.
func chatEnv(t *testing.T, f *aitest.Fake, tweaks ...func(*config.Config, *Options)) (*testEnv, string) {
	t.Helper()
	dir := t.TempDir()
	tw := func(_ *config.Config, o *Options) {
		o.ChatsDir = dir
		o.StartChat = func(a ai.Agent, opt ai.Options) *ai.Chat { return aitest.Start(a, opt, f) }
		o.BeginSignIn = f.SignIn
	}
	return newTestEnv(t, append([]func(*config.Config, *Options){tw}, tweaks...)...), dir
}

// awaitChat reads events until a chat.state event satisfies cond.
func awaitChat(t *testing.T, s *stream, cond func(chatView) bool) chatView {
	t.Helper()
	for {
		ev, _ := s.await(t, "chat.state")
		var v chatView
		if err := json.Unmarshal(ev.Data, &v); err != nil {
			t.Fatal(err)
		}
		if cond(v) {
			return v
		}
	}
}

func chatReadyView(v chatView) bool { return v.State == chatReady && !v.Streaming }

// ask sends a question the way the page does, with the editor holding
// buffer and the grid showing the result as view describes.
func (e *testEnv) ask(id, q, buffer string, view *chatGrid, want int) testEnvelope {
	e.t.Helper()
	b, _ := json.Marshal(chatReq{Question: q, Attach: true,
		Editor: runReq{Buffer: buffer, Caret: len(buffer)}, View: view})
	return e.api("POST", "/api/v1/ws/"+id+"/chat/ask", string(b), want)
}

func (e *testEnv) transcript(id string) chatSnapshot {
	e.t.Helper()
	return decodeData[chatSnapshot](e.t, e.api("GET", "/api/v1/ws/"+id+"/chat", "", 200))
}

// runQuery runs sql to completion and returns the grid's seq for it.
func (e *testEnv) runQuery(id string, s *stream, sql string) int {
	e.t.Helper()
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody(sql, 0, false), 200)
	s.await(e.t, "run")
	return decodeData[resultPage](e.t, e.api("GET", "/api/v1/ws/"+id+"/result?n=1", "", 200)).Seq
}

// A question about a query that ran: the query and the column names go,
// the rows do not (no ai_rows), the transcript says so, the answer streams
// into the transcript, and the conversation is saved.
func TestChatAskWithholdsRowsAndSaves(t *testing.T) {
	f := &aitest.Fake{}
	e, dir := chatEnv(t, f)
	id, s := e.connected()
	sql := "SELECT * FROM cats ORDER BY age"
	seq := e.runQuery(id, s, sql)

	e.api("POST", "/api/v1/ws/"+id+"/chat/open", "", 200)
	v := awaitChat(t, s, chatReadyView)
	if v.Model != "Fast Model" || len(v.Models) != 2 || v.Agent.ID != "copilot" {
		t.Fatalf("ready view = %+v", v)
	}

	e.ask(id, "why is Oliver first?", sql, &chatGrid{Seq: seq, Sort: -1}, 200)
	awaitChat(t, s, chatReadyView)

	p := f.Prompts()
	if len(p) != 1 {
		t.Fatalf("prompts = %d, want 1", len(p))
	}
	for _, want := range []string{"SQL assistant inside dbc", `Connection "demo-sqlite" uses the SQLite driver`,
		"FROM cats ORDER BY age", "columns: id, name, breed, age, adopted", "Question: why is Oliver first?"} {
		if !strings.Contains(p[0], want) {
			t.Errorf("prompt is missing %q:\n%s", want, p[0])
		}
	}
	if strings.Contains(p[0], "Whiskers") {
		t.Errorf("rows went without ai_rows:\n%s", p[0])
	}

	tr := e.transcript(id)
	var roles []string
	for _, m := range tr.Msgs {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,note,agent" {
		t.Fatalf("transcript roles = %s: %+v", got, tr.Msgs)
	}
	if !strings.Contains(tr.Msgs[1].Text, "rows not sent — set ai_rows = true") ||
		!strings.Contains(tr.Msgs[1].Text, "schema of cats") {
		t.Errorf("the note does not say what went: %q", tr.Msgs[1].Text)
	}
	if tr.Msgs[2].Text != "echo: Question: why is Oliver first?" {
		t.Errorf("answer = %q", tr.Msgs[2].Text)
	}

	metas, err := userdata.ListChats(dir)
	if err != nil || len(metas) != 1 || metas[0].Title != "why is Oliver first?" || metas[0].Conn != "demo-sqlite" {
		t.Fatalf("archive = %+v, %v", metas, err)
	}
	if tr.View.ArchiveID != metas[0].ID {
		t.Errorf("the view's archive id %q is not the file's %q", tr.View.ArchiveID, metas[0].ID)
	}
}

// With ai_rows, rows go in the grid's sort order and without the columns
// hidden in the grid — named, but not their values.
func TestChatSendsGridOrderWithoutHiddenColumns(t *testing.T) {
	f := &aitest.Fake{}
	e, _ := chatEnv(t, f, func(c *config.Config, _ *Options) { c.Connections[0].AIRows = true })
	id, s := e.connected()
	sql := "SELECT id, name, breed, age FROM cats"
	e.runQuery(id, s, sql)
	// the grid sorts by age descending (column 3): the page's fetch sets
	// the server's order, as a header click does
	pg := decodeData[resultPage](t, e.api("GET", "/api/v1/ws/"+id+"/result?sort=3&desc=1&n=1", "", 200))

	e.ask(id, "who is oldest?", sql, &chatGrid{Seq: pg.Seq, Sort: 3, Desc: true, Hidden: []int{1, 1, 9}}, 200)
	s.await(t, "chat.msg") // the question
	awaitChat(t, s, chatReadyView)

	p := f.Prompts()[0]
	if !strings.Contains(p, "sorted the result in the grid by age") {
		t.Errorf("the sort is not explained:\n%s", p)
	}
	if strings.Contains(p, "Milo") || strings.Contains(p, "Whiskers") {
		t.Errorf("a hidden column's values went:\n%s", p)
	}
	if !strings.Contains(p, "hid this column in the grid, so it is left out above: name") {
		t.Errorf("the hidden column is not named:\n%s", p)
	}
	// Milo (7) is the oldest; his breed leads the rows sent
	first := strings.Index(p, "Siamese")
	if first < 0 || strings.Index(p, "Maine Coon") < first {
		t.Errorf("rows are not in the grid's order (age desc):\n%s", p)
	}
	note := e.transcript(id).Msgs[1].Text
	if !strings.Contains(note, "(sorted by age desc, 1 column hidden)") {
		t.Errorf("note = %q", note)
	}
}

// A grid view that no longer describes the result — a rerun landed after
// the page drew it — is refused when it hides columns: dropping the view
// would send exactly the columns the user hid.
func TestChatStaleViewWithHiddenColumnsIsRefused(t *testing.T) {
	f := &aitest.Fake{}
	e, _ := chatEnv(t, f)
	id, s := e.connected()
	seq := e.runQuery(id, s, "SELECT * FROM cats")
	e.runQuery(id, s, "SELECT * FROM cats")
	env := e.ask(id, "q", "SELECT 1", &chatGrid{Seq: seq, Sort: -1, Hidden: []int{1}}, 409)
	if !strings.Contains(env.Error, "ask again") {
		t.Errorf("409 says %q", env.Error)
	}
	// without hidden columns a stale view is harmless: it is simply unused
	e.ask(id, "q", "SELECT 1", &chatGrid{Seq: seq, Sort: -1}, 200)
}

// The context chip's forecast says what the next question would carry.
func TestChatContextForecast(t *testing.T) {
	e, _ := chatEnv(t, &aitest.Fake{})
	id, s := e.connected()
	sql := "SELECT * FROM cats"
	seq := e.runQuery(id, s, sql)
	b, _ := json.Marshal(chatReq{Question: "", Editor: runReq{Buffer: sql}, View: &chatGrid{Seq: seq, Sort: -1}})
	got := decodeData[map[string]string](t, e.api("POST", "/api/v1/ws/"+id+"/chat/context", string(b), 200))["note"]
	for _, want := range []string{"schema of cats", "query", "column names", "rows not sent"} {
		if !strings.Contains(got, want) {
			t.Errorf("forecast %q is missing %q", got, want)
		}
	}
	if strings.HasPrefix(got, "sent:") {
		t.Errorf("forecast keeps the transcript's prefix: %q", got)
	}
}

// A second question mid-answer is a 409; stop cancels the answer, which
// ends as "— stopped".
func TestChatBusyThenStop(t *testing.T) {
	f := &aitest.Fake{}
	e, _ := chatEnv(t, f)
	id, s := e.open()
	e.api("POST", "/api/v1/ws/"+id+"/chat/open", "", 200)
	awaitChat(t, s, chatReadyView)

	e.ask(id, "SLOW please", "", nil, 200)
	awaitChat(t, s, func(v chatView) bool { return v.Streaming })
	env := e.ask(id, "and another", "", nil, 409)
	if !strings.Contains(env.Error, "still answering") {
		t.Errorf("409 says %q", env.Error)
	}
	e.api("POST", "/api/v1/ws/"+id+"/chat/stop", "", 200)
	awaitChat(t, s, chatReadyView)
	if f.Cancels() != 1 {
		t.Errorf("cancels = %d", f.Cancels())
	}
	msgs := e.transcript(id).Msgs
	if msgs[len(msgs)-1].Text != "— stopped" {
		t.Errorf("last line = %+v", msgs[len(msgs)-1])
	}
	e.api("POST", "/api/v1/ws/"+id+"/chat/ask", `{"question":"   "}`, 400)
}

// A question naming a table sends that table's columns, looked up at send
// time on the pool.
func TestChatSendsSchemaOfMentionedTables(t *testing.T) {
	f := &aitest.Fake{}
	e, _ := chatEnv(t, f)
	id, s := e.connected()
	e.ask(id, "how many cats are adopted?", "", nil, 200)
	awaitChat(t, s, chatReadyView)
	p := f.Prompts()[0]
	if !strings.Contains(p, "cats") || !strings.Contains(p, "adopted") || !strings.Contains(p, "breed") {
		t.Errorf("the schema of cats did not go:\n%s", p)
	}
}

// ⟲ new saves the conversation; the list offers it (not the live one);
// reopening loads its transcript in a fresh session and says so; the live
// one cannot be deleted from the list, only from its own pane.
func TestChatArchiveNewOpenDelete(t *testing.T) {
	f := &aitest.Fake{}
	e, dir := chatEnv(t, f)
	id, s := e.open()
	e.ask(id, "first question", "", nil, 200)
	awaitChat(t, s, chatReadyView)
	liveID := e.transcript(id).View.ArchiveID

	listed := decodeData[[]savedChat](t, e.api("GET", "/api/v1/chats?ws="+id, "", 200))
	if len(listed) != 0 {
		t.Errorf("the live conversation is offered for reopening: %+v", listed)
	}
	env := e.api("DELETE", "/api/v1/chats/"+liveID, "", 400)
	if !strings.Contains(env.Error, "open in an assistant pane") {
		t.Errorf("deleting the live one says %q", env.Error)
	}

	e.api("POST", "/api/v1/ws/"+id+"/chat/new", "", 200)
	s.await(t, "chat.reset")
	if n := len(e.transcript(id).Msgs); n != 0 {
		t.Fatalf("⟲ new left %d messages", n)
	}
	listed = decodeData[[]savedChat](t, e.api("GET", "/api/v1/chats?ws="+id, "", 200))
	if len(listed) != 1 || listed[0].ID != liveID || listed[0].Title != "first question" {
		t.Fatalf("list after new = %+v", listed)
	}

	e.api("POST", "/api/v1/ws/"+id+"/chat/load", `{"id":"`+liveID+`"}`, 200)
	tr := e.transcript(id)
	last := tr.Msgs[len(tr.Msgs)-1]
	if tr.Msgs[0].Text != "first question" || !strings.Contains(last.Text, "not a resumed session") {
		t.Fatalf("reopened transcript = %+v", tr.Msgs)
	}
	if tr.View.ArchiveID != liveID {
		t.Errorf("reopening forked the file: %q vs %q", tr.View.ArchiveID, liveID)
	}

	e.api("POST", "/api/v1/ws/"+id+"/chat/delete", "", 200)
	if _, err := os.Stat(filepath.Join(dir, liveID+".json")); !os.IsNotExist(err) {
		t.Errorf("the deleted conversation's file is still there: %v", err)
	}
	if n := len(e.transcript(id).Msgs); n != 0 {
		t.Errorf("delete left %d messages", n)
	}
	e.api("POST", "/api/v1/ws/"+id+"/chat/load", `{"id":"../x"}`, 400)
}

// An agent that refuses for lack of sign-in offers ⎆ sign in; the device
// flow puts the code in the view and the transcript; on success the chat
// reconnects and the question asked before the refusal goes out.
func TestChatSignInThenQueuedQuestionGoes(t *testing.T) {
	f := &aitest.Fake{SignedOut: true}
	e, _ := chatEnv(t, f)
	id, s := e.open()
	e.ask(id, "hello there", "", nil, 200)
	v := awaitChat(t, s, func(v chatView) bool { return v.State == chatDead })
	if v.SignIn != "offer" {
		t.Fatalf("a refused handshake does not offer sign-in: %+v", v)
	}
	e.api("POST", "/api/v1/ws/"+id+"/chat/signin", "", 200)
	awaitChat(t, s, chatReadyView)
	if f.SignIns() != 1 {
		t.Errorf("sign-ins = %d", f.SignIns())
	}
	p := f.Prompts()
	if len(p) != 1 || !strings.Contains(p[0], "Question: hello there") {
		t.Fatalf("the queued question did not go: %q", p)
	}
	var saw bool
	for _, m := range e.transcript(id).Msgs {
		saw = saw || strings.Contains(m.Text, "enter code "+aitest.FakeUserCode)
	}
	if !saw {
		t.Error("the device code never reached the transcript")
	}
}

// Switching model tells the agent and the transcript; an unknown model or
// assistant is a 400.
func TestChatModelAndAgent(t *testing.T) {
	f := &aitest.Fake{}
	e, _ := chatEnv(t, f)
	id, s := e.open()
	e.api("POST", "/api/v1/ws/"+id+"/chat/open", "", 200)
	awaitChat(t, s, chatReadyView)
	e.api("POST", "/api/v1/ws/"+id+"/chat/model", `{"id":"smart"}`, 200)
	v := awaitChat(t, s, func(v chatView) bool { return v.ModelID == "smart" })
	if v.Model != "Smart Model" {
		t.Errorf("model = %q", v.Model)
	}
	e.api("POST", "/api/v1/ws/"+id+"/chat/model", `{"id":"nope"}`, 400)
	e.api("POST", "/api/v1/ws/"+id+"/chat/agent", `{"id":"nope"}`, 400)
}

// ---------------------------------------------------------------------------
// Scripts
// ---------------------------------------------------------------------------

const testScript = `//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	r, err := s.Query("demo-sqlite", "SELECT name FROM cats WHERE age >= ? ORDER BY name", 5)
	if err != nil {
		return err
	}
	s.Print("found %d cats", len(r.Rows))
	s.Show(r)
	return nil
}
`

// A script from scripts_dir runs by name: s.Print reaches the log and
// s.Show the grid, as they happen, then the outcome; a name that is not in
// the list is refused.
func TestScriptListAndRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old_cats.go"), []byte(testScript), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newTestEnv(t, func(c *config.Config, _ *Options) { c.ScriptsDir = dir })
	list := decodeData[struct {
		Scripts []string `json:"scripts"`
	}](t, e.api("GET", "/api/v1/scripts", "", 200))
	if len(list.Scripts) != 1 || list.Scripts[0] != "old_cats.go" {
		t.Fatalf("scripts = %v", list.Scripts)
	}

	id, s := e.connected()
	e.api("POST", "/api/v1/ws/"+id+"/script", `{"name":"../old_cats.go"}`, 404)
	e.api("POST", "/api/v1/ws/"+id+"/script", `{"name":"old_cats.go"}`, 200)
	_, logs := s.await(t, "result")
	if !strings.Contains(strings.Join(logs, "\n"), "found 3 cats") {
		t.Errorf("s.Print did not reach the log before s.Show: %q", logs)
	}
	ev, _ := s.await(t, "run")
	run := decodeData[runEvent](t, testEnvelope{Data: ev.Data})
	if !run.OK || !run.HasResult || !strings.Contains(run.Status, "3 rows") {
		t.Errorf("script outcome = %+v", run)
	}
	pg := decodeData[resultPage](t, e.api("GET", "/api/v1/ws/"+id+"/result", "", 200))
	if len(pg.Cells) != 3 || *pg.Cells[0][0] != "Bella" {
		t.Errorf("the shown result is not in the grid: %+v", pg.Cells)
	}
}
