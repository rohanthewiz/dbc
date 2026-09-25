package userdata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleChat(id string, updated time.Time, question string) Chat {
	msgs := []ChatMsg{{Role: "user", Text: question}, {Role: "agent", Text: "an answer"}}
	return Chat{ID: id, Title: ChatTitle(msgs), Agent: "Copilot", Conn: "demo",
		Started: updated.Add(-time.Minute), Updated: updated, Msgs: msgs}
}

// A saved conversation reads back whole, and rewriting the same id replaces
// it rather than adding a second file.
func TestChatRoundTrips(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 24, 15, 4, 0, 0, time.UTC)
	c := sampleChat(NewChatID(at), at, "why is this slow?")
	if err := SaveChat(dir, c); err != nil {
		t.Fatalf("save: %v", err)
	}
	c.Msgs = append(c.Msgs, ChatMsg{Role: "user", Text: "and now?"})
	if err := SaveChat(dir, c); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, err := LoadChat(dir, c.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Msgs) != 3 || got.Title != "why is this slow?" || got.Conn != "demo" || !got.Updated.Equal(at) {
		t.Errorf("loaded %+v", got)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("re-saving should replace the file, have %d entries", len(ents))
	}
	if fi, _ := os.Stat(filepath.Join(dir, c.ID+chatExt)); fi.Mode().Perm() != 0o600 {
		t.Errorf("conversation file mode = %v, want user-only", fi.Mode().Perm())
	}
}

// Nothing to save writes nothing: no messages, no id, or no directory.
func TestSaveChatSkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := SaveChat(dir, Chat{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveChat("", sampleChat("y", time.Now(), "q")); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("wrote %d files for nothing", len(ents))
	}
}

// The listing is newest first, skips files it cannot parse and files that
// are not conversations, and a missing directory is simply empty.
func TestListChats(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i, q := range []string{"first", "second", "third"} {
		at := base.Add(time.Duration(i) * time.Hour)
		if err := SaveChat(dir, sampleChat(NewChatID(at), at, q)); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o600)

	metas, err := ListChats(dir)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, m := range metas {
		titles = append(titles, m.Title)
	}
	if strings.Join(titles, ",") != "third,second,first" {
		t.Errorf("titles = %v, want newest first", titles)
	}
	if metas[0].Count != 2 || metas[0].Conn != "demo" || metas[0].Agent != "Copilot" {
		t.Errorf("meta = %+v", metas[0])
	}

	if none, err := ListChats(filepath.Join(dir, "missing")); err != nil || none != nil {
		t.Errorf("missing dir: %v, %v", none, err)
	}
}

// The directory keeps only the newest MaxChats conversations.
func TestSaveChatPrunesTheOldest(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var oldest string
	for i := range MaxChats + 3 {
		at := base.Add(time.Duration(i) * time.Minute)
		c := sampleChat(NewChatID(at), at, "q")
		if i == 0 {
			oldest = c.ID
		}
		if err := SaveChat(dir, c); err != nil {
			t.Fatal(err)
		}
	}
	metas, _ := ListChats(dir)
	if len(metas) != MaxChats {
		t.Errorf("kept %d, want %d", len(metas), MaxChats)
	}
	if _, err := LoadChat(dir, oldest); err == nil {
		t.Error("the oldest conversation should have been pruned")
	}
}

// An id cannot name a file outside the archive.
func TestLoadChatRejectsPaths(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadChat(dir, "../secrets"); err == nil {
		t.Error("an id with a path in it must be refused")
	}
	if err := RemoveChat(dir, "../secrets"); err != nil {
		t.Errorf("remove with a path id should do nothing, got %v", err)
	}
}

// A file without a stored id takes its filename as the id.
func TestLoadChatFillsIDFromFilename(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "abc.json"), []byte(`{"title":"t","msgs":[{"role":"user","text":"t"}]}`), 0o600)
	c, err := LoadChat(dir, "abc")
	if err != nil || c.ID != "abc" {
		t.Errorf("c=%+v err=%v", c, err)
	}
}

// The title is the first line of the first question, bounded in length.
func TestChatTitle(t *testing.T) {
	long := strings.Repeat("x", MaxChatTitle+10)
	cases := []struct {
		msgs []ChatMsg
		want string
	}{
		{[]ChatMsg{{"info", "connecting"}, {"user", "  why slow?\nmore detail"}}, "why slow?"},
		{[]ChatMsg{{"user", "   "}, {"user", "second"}}, "second"},
		{[]ChatMsg{{"agent", "hello"}}, ""},
		{[]ChatMsg{{"user", long}}, strings.Repeat("x", MaxChatTitle-1) + "…"},
	}
	for _, c := range cases {
		if got := ChatTitle(c.msgs); got != c.want {
			t.Errorf("ChatTitle(%v) = %q, want %q", c.msgs, got, c.want)
		}
	}
}

// Ids minted a moment apart differ and sort in time order.
func TestNewChatIDOrders(t *testing.T) {
	at := time.Date(2026, 9, 24, 15, 4, 5, 100, time.UTC)
	a, b := NewChatID(at), NewChatID(at.Add(time.Nanosecond))
	if a == b || a > b {
		t.Errorf("ids %q, %q should differ and sort in time order", a, b)
	}
}
