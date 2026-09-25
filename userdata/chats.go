package userdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// The assistant's conversations, kept one JSON file per conversation under
// ~/.config/dbc/chats, listed newest first and capped at the most recent
// MaxChats.
//
// PROVENANCE. Ported from ced (github.com/rohanthewiz/ced,
// internal/chatstore), where it has kept Copilot chats since August 2026.
// The shape is unchanged; what differs is what a conversation is filed
// under — the connection it was about, where ced records a project root —
// and the role names, which are dbc's six rather than ced's four.
//
// WHY A DIRECTORY, NOT ONE FILE. The history is a JSON-lines file because an
// entry is written once and never changes. A conversation is the opposite: a
// document that is rewritten after every turn and can grow to hundreds of
// messages. One file each makes that rewrite proportional to one
// conversation rather than all of them, makes "keep the last few" a delete
// of the oldest files rather than a rewrite of a growing blob, and lets a
// user remove one conversation with rm.
//
// WHY THE TRANSCRIPT AND NOT THE SESSION. What is stored is what the user
// SAW — the pane's message list. The agent's own memory lives in its ACP
// session, which dies with the process, so a restored conversation is
// something to read and quote from, not something the agent can continue.
// Storing the session id would be storing a handle that is already invalid
// by the time anyone could use it.
//
// Every function takes the directory rather than reading ChatsDir itself, so
// tests point it at a temp dir and nothing here reaches for a global.

const (
	// MaxChats caps how many conversations the directory keeps, trimmed
	// from the oldest. The feature is "the last few chats": an unbounded
	// archive would be a directory growing forever and a picker nobody can
	// scan. Thirty is a working week or two of questions.
	MaxChats = 30

	// MaxChatTitle bounds a derived title, in runes. The picker row also
	// carries a date, a message count and sometimes a connection name, and
	// a title long enough to push those off the row costs more than it says.
	MaxChatTitle = 72

	// chatExt is the file suffix. Anything else in the directory is ignored,
	// so a note the user keeps beside the archive costs nothing.
	chatExt = ".json"
)

// ChatMsg is one transcript entry. Role is a string ("user", "agent",
// "tool", "note", "info", "error") rather than the TUI's integer: an archive
// outlives the enum, and a reordered constant block must not silently
// restyle every stored conversation.
type ChatMsg struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Chat is one archived conversation.
type Chat struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Agent   string    `json:"agent,omitempty"` // the backend's display name
	Model   string    `json:"model,omitempty"` // the model's display name
	Conn    string    `json:"conn,omitempty"`  // the active connection at the last save
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
	Msgs    []ChatMsg `json:"msgs"`
}

// ChatMeta is the listing view: what a picker row needs, without the
// transcript. ListChats still reads whole files — they are few and small,
// and a partial parse would need a second schema to stay honest — but
// returning only this keeps a caller from holding every conversation in
// memory to draw a list of titles.
type ChatMeta struct {
	ID      string
	Title   string
	Agent   string
	Conn    string
	Updated time.Time
	Count   int // messages, so a row can say how much is in there
}

// ChatsDir is where conversations persist, beside the history and the
// editor buffer. It returns "" when no home directory is known, and
// conversations then live only for the session.
func ChatsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "chats")
}

// NewChatID mints a conversation id from a wall-clock instant: sortable by
// construction, which lets a listing fall back on id order when a file's
// Updated stamp is missing, and legible to a user browsing the directory.
//
// The tail is the full nanosecond field. ⟲ new saves the old conversation
// and the next save mints the new one's id moments later; a coarser tail
// could collide there, and a collision would overwrite the earlier file.
func NewChatID(now time.Time) string {
	return fmt.Sprintf("%s-%09d", now.Format("20060102-150405"), now.Nanosecond())
}

// ChatTitle picks what a conversation is remembered by: the first line of
// the first thing the USER asked. Not the answer — the question is what the
// user scans a list for, and an answer's first line is usually a
// restatement or a preamble. Returns "" when there is no question; the
// caller decides how an untitled row reads.
func ChatTitle(msgs []ChatMsg) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		line := strings.TrimSpace(m.Text)
		if i := strings.IndexAny(line, "\r\n"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > MaxChatTitle {
			return strings.TrimSpace(string(r[:MaxChatTitle-1])) + "…"
		}
		return line
	}
	return ""
}

// SaveChat writes c into dir as <id>.json, then trims the directory back to
// MaxChats. Rewriting the same id is the normal case — the live
// conversation is re-saved after every answer — so the write goes to a temp
// file that is renamed over the old one: a crash mid-write must not leave a
// half-written transcript where a readable one used to be.
//
// An empty dir, id or message list is a no-op rather than an error. The
// caller saves unconditionally at teardown, and "there was no conversation"
// is not a failure.
//
// The files are user-only, like the history: a conversation about a query
// carries the query, and on connections with ai_rows its result rows.
func SaveChat(dir string, c Chat) error {
	if dir == "" || c.ID == "" || len(c.Msgs) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return serr.Wrap(err, "dir", dir)
	}
	bs, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return serr.Wrap(err)
	}
	path := filepath.Join(dir, c.ID+chatExt)
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, bs, 0o600); err != nil {
		return serr.Wrap(err, "path", tmp)
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return serr.Wrap(err, "path", path)
	}
	pruneChats(dir)
	return nil
}

// ListChats returns the archived conversations newest first. A file that
// will not parse costs itself and nothing else: one truncated or hand-edited
// transcript must not take the whole list down with it. A missing directory
// is the common case — a user who has never asked anything — and is not an
// error.
func ListChats(dir string) ([]ChatMeta, error) {
	if dir == "" {
		return nil, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, serr.Wrap(err, "dir", dir)
	}
	var out []ChatMeta
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), chatExt) {
			continue
		}
		c, err := readChat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, ChatMeta{ID: c.ID, Title: c.Title, Agent: c.Agent,
			Conn: c.Conn, Updated: c.Updated, Count: len(c.Msgs)})
	}
	// Newest first by Updated, falling back on the id — a timestamp by
	// construction — so a file with a zero stamp still lands in a sensible
	// place instead of clumping at one end.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// LoadChat reads one conversation by id.
func LoadChat(dir, id string) (Chat, error) {
	if dir == "" || id == "" {
		return Chat{}, errors.New("no conversation named")
	}
	// The id becomes a filename, so it must not be able to reach out of the
	// archive. Ids are minted by NewChatID, but one makes a round trip
	// through the UI and the file it was read from on the way here.
	if id != filepath.Base(id) {
		return Chat{}, fmt.Errorf("bad conversation id %q", id)
	}
	return readChat(filepath.Join(dir, id+chatExt))
}

// RemoveChat deletes one conversation. A missing file is success: the
// caller wanted it gone.
func RemoveChat(dir, id string) error {
	if dir == "" || id == "" || id != filepath.Base(id) {
		return nil
	}
	err := os.Remove(filepath.Join(dir, id+chatExt))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return serr.Wrap(err, "id", id)
	}
	return nil
}

// readChat parses one file, taking the id from the filename when the stored
// one is missing: the id IS the file's identity, and a conversation that
// cannot name itself could be neither reopened nor saved back.
func readChat(path string) (Chat, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return Chat{}, serr.Wrap(err, "path", path)
	}
	var c Chat
	if err = json.Unmarshal(bs, &c); err != nil {
		return Chat{}, serr.Wrap(err, "path", path)
	}
	if c.ID == "" {
		c.ID = strings.TrimSuffix(filepath.Base(path), chatExt)
	}
	return c, nil
}

// pruneChats trims the directory back to MaxChats, oldest first.
// Best-effort by design: failing to delete an old conversation must never
// fail the save of the current one, which is where the user's work is.
func pruneChats(dir string) {
	metas, err := ListChats(dir)
	if err != nil || len(metas) <= MaxChats {
		return
	}
	for _, m := range metas[MaxChats:] {
		_ = RemoveChat(dir, m.ID)
	}
}

// RestoreNote is the transcript line shown when a saved conversation is
// reopened: what the user got back, and what they did not. A reopened
// conversation is a transcript, not a resumed agent session, and silence
// about that would let a follow-up get a confidently unrelated answer from
// a model that never saw the conversation. Both UIs show these words.
func (c Chat) RestoreNote() string {
	when := ""
	if !c.Updated.IsZero() {
		when = " from " + c.Updated.Local().Format("Jan 2 15:04")
	}
	return "— reopened conversation" + when + ". This is the saved transcript, not a resumed " +
		"session: the assistant has no memory of it, so quote what matters in a follow-up."
}
