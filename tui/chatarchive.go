package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/userdata"
)

// Keeping the assistant's conversations: each one is saved to
// ~/.config/dbc/chats (see userdata/chats.go for the format and the cap), and
// the recent ones are offered back — as rows in the empty pane, and in a
// Recent conversations list from the transcript's right-click menu.
//
//	answer done ─┐
//	agent exits ─┤
//	⟲ new ───────┼─► chatSave ─► userdata.SaveChat(<id>.json)   (same id all
//	open saved ──┤                                               conversation
//	quit ────────┘                                               long)
//
//	empty pane / right-click ─► Recent conversations ─► chatOpenSaved
//	    saves the live one, starts a fresh agent session, loads the transcript
//
//	right-click a saved row, or d d in the list ─► chatDelete ─► RemoveChat
//
//	transcript menu ─► Delete this conversation ─► chatDeleteLive
//	    RemoveChat(live id), then resetChat — the clear that skips the save
//
// SAVED AFTER EVERY ANSWER, not only on ⟲ new or quit. A crash or a killed
// terminal must cost at most the answer in flight: the transcript is the one
// thing in the pane nothing else can rebuild, and one small file per answer
// is nothing against the seconds the answer took.
//
// A REOPENED CONVERSATION IS A TRANSCRIPT, NOT A RESUMED SESSION, and the
// pane says so. What was saved is what the user saw; the agent's memory of
// it lived in an ACP session that ended with its process. ACP's session/load
// could in principle resume one, but it is optional, agent-side, and replays
// the whole history back as updates that would duplicate what dbc just drew.
// So reopening starts a fresh session and a line in the transcript names the
// gap — silence there would let a follow-up get a confidently unrelated
// answer from a model that never saw the conversation.
//
// NOTHING IS DESTROYED, SO NOTHING CONFIRMS. ⟲ new saves first, and opening a
// saved conversation saves the live one on its way out, so each gesture is
// undone from the same list. The exception is deleting a conversation —
// saved, or the live one — the one gesture that cannot be undone, so it is
// never one keypress (see chatDelete and chatDeleteLive).
//
// REOPENING CONTINUES THE SAME FILE. A conversation reopened and carried on is
// one conversation; forking a copy would leave two rows that differ only in
// where the user stopped reading.

// chatNow stamps conversation ids and save times; tests pin it to get a
// stable order instead of racing the wall clock.
var chatNow = time.Now

// recentInPane is how many saved conversations the empty pane offers. The
// rest are one click further, in the list; the pane's first job is still to
// say how to ask something.
const recentInPane = 5

// chatRoleName maps a transcript role to the archive's string for it.
func chatRoleName(r chatRole) string {
	switch r {
	case roleUser:
		return "user"
	case roleAgent:
		return "agent"
	case roleTool:
		return "tool"
	case roleNote:
		return "note"
	case roleErr:
		return "error"
	default:
		return "info"
	}
}

// chatRoleFromName is chatRoleName's inverse. An unknown name reads as info,
// the neutral style, so a file written by a later dbc still loads with its
// text intact and only its styling degraded.
func chatRoleFromName(s string) chatRole {
	switch s {
	case "user":
		return roleUser
	case "agent":
		return roleAgent
	case "tool":
		return roleTool
	case "note":
		return roleNote
	case "error":
		return roleErr
	default:
		return roleInfo
	}
}

// worthSaving reports whether the transcript holds a conversation at all. One
// with no question in it — the pane opened, the agent connected, nothing was
// asked — would only fill the recent list with rows nobody could tell apart.
func (p *chatPane) worthSaving() bool {
	for _, msg := range p.msgs {
		if msg.role == roleUser {
			return true
		}
	}
	return false
}

// chatSave writes the live conversation to the archive. It runs on the event
// loop, deliberately: the file is small, and saving here means the archive
// can never be written from two goroutines at once or lag what is on screen.
//
// A failure is logged once, not after every answer: the log pane is where
// dbc reports background trouble, but the same "permission denied" repeated
// per answer would bury everything else in it.
//
// Reports whether the conversation is now on disk.
func (m *Model) chatSave() bool {
	p := m.chat
	if p.dir == "" || !p.worthSaving() {
		return false
	}
	now := chatNow()
	if p.archiveID == "" {
		p.archiveID = userdata.NewChatID(now)
		p.archiveStart = now
	}
	msgs := p.archiveMsgs()
	name := p.agent.Name
	if name == "" {
		name = m.aiAgent.Name
	}
	err := userdata.SaveChat(p.dir, userdata.Chat{
		ID: p.archiveID, Title: userdata.ChatTitle(msgs),
		Agent: name, Model: p.modelName(), Conn: m.active,
		Started: p.archiveStart, Updated: now, Msgs: msgs,
	})
	switch {
	case err == nil:
		p.saveErr = ""
		return true
	case err.Error() != p.saveErr:
		p.saveErr = err.Error()
		m.logf(logWarn, "could not save the assistant conversation: %v", err)
	}
	return false
}

// archiveMsgs is the transcript in the archive's form.
func (p *chatPane) archiveMsgs() []userdata.ChatMsg {
	msgs := make([]userdata.ChatMsg, 0, len(p.msgs))
	for _, msg := range p.msgs {
		msgs = append(msgs, userdata.ChatMsg{Role: chatRoleName(msg.role), Text: msg.text})
	}
	return msgs
}

// chatLoadRecent refreshes the conversations the empty pane offers. Called
// when the pane opens empty and when a conversation is put away — not from
// the draw, which runs every frame and must not read a directory.
func (m *Model) chatLoadRecent() {
	p := m.chat
	p.recent = nil
	if p.dir == "" {
		return
	}
	metas, err := userdata.ListChats(p.dir)
	if err != nil {
		return // the list simply is not offered; opening it says why
	}
	p.recent = metas
}

// chatOpenSaved puts the live conversation away and loads a saved one.
//
// It goes through newChat, which saves the live conversation, clears the
// pane, and starts a fresh agent session. The fresh session matters as much
// as the save: continuing to talk to a session that remembers the
// conversation being replaced is the confusing failure, and an invisible one.
func (m *Model) chatOpenSaved(id string) tea.Cmd {
	p := m.chat
	c, err := userdata.LoadChat(p.dir, id)
	if err != nil {
		m.logf(logErr, "could not open that conversation: %v", err)
		return nil
	}
	cmd := m.newChat()
	for _, msg := range c.Msgs {
		p.add(chatRoleFromName(msg.Role), msg.Text)
	}
	p.archiveID, p.archiveStart = c.ID, c.Started
	if p.archiveStart.IsZero() {
		p.archiveStart = c.Updated
	}
	p.add(roleInfo, restoreNote(c))
	p.follow = true
	m.chat.open = true
	m.focus = focusChat
	return cmd
}

// restoreNote tells the user what they got back — and what they did not.
func restoreNote(c userdata.Chat) string {
	when := ""
	if !c.Updated.IsZero() {
		when = " from " + c.Updated.Local().Format("Jan 2 15:04")
	}
	return "— reopened conversation" + when + ". This is the saved transcript, not a resumed " +
		"session: the assistant has no memory of it, so quote what matters in a follow-up."
}

// chatWhen renders a save time for a list row: the clock for today, the date
// before that, and the year once it is not this one. A row is scanned, not
// read — "this morning" and "last week" are the questions it answers.
func chatWhen(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t, now = t.Local(), now.Local()
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("Jan 2 2006")
	}
}

// chatRowTitle is a saved conversation's title, or a stand-in for one with
// no question to be named by (hand-edited, or written by a later dbc).
func chatRowTitle(c userdata.ChatMeta) string {
	if c.Title == "" {
		return "(untitled)"
	}
	return c.Title
}

// chatRowDetail is the rest of a row: when, how long, and — only when it is
// not the connection in use — which connection it was about. The common case
// is the current one, and repeating it on every row would spend the width
// the title needs while telling nothing apart.
func (m *Model) chatRowDetail(c userdata.ChatMeta) string {
	return strings.Join(m.chatRowDetails(c)[0], " · ")
}

// chatRowDetails is chatRowDetail's parts at decreasing widths, for a row
// that cannot fit them all: the message count goes first, since it is the
// least telling; then the connection; the time last, since it is what tells
// otherwise similar rows apart. The final, empty entry leaves only the title.
func (m *Model) chatRowDetails(c userdata.ChatMeta) [][]string {
	when := chatWhen(c.Updated, chatNow())
	count := fmt.Sprintf("%d msg%s", c.Count, pick(c.Count == 1, "", "s"))
	conn := ""
	if c.Conn != "" && c.Conn != m.active {
		conn = c.Conn
	}
	nonEmpty := func(parts ...string) []string {
		out := parts[:0]
		for _, s := range parts {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return [][]string{
		nonEmpty(when, count, conn),
		nonEmpty(when, conn),
		nonEmpty(when),
		nil,
	}
}

// otherRecent is the saved conversations other than the live one — reopening
// what is already on screen would be a no-op at best, and at worst reload it
// from a copy one answer stale.
func (p *chatPane) otherRecent() []userdata.ChatMeta {
	out := make([]userdata.ChatMeta, 0, len(p.recent))
	for _, c := range p.recent {
		if c.ID != p.archiveID {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Deleting
// ---------------------------------------------------------------------------

// chatDelete removes a saved conversation for good. It is the one gesture
// here that destroys something, so it is never a single stray keypress:
// from the keyboard it takes the delete key twice on the same row, and from
// the mouse it is a named row of a right-click menu — itself a second,
// deliberate step. Reports whether the file is gone.
//
// The live conversation cannot be deleted this way: it is never offered in
// the lists (see otherRecent), and its next save would only write it back.
// chatDeleteLive is its own path.
func (m *Model) chatDelete(id string) bool {
	p := m.chat
	title := id
	for _, c := range p.recent {
		if c.ID == id {
			title = chatRowTitle(c)
		}
	}
	if err := userdata.RemoveChat(p.dir, id); err != nil {
		m.logf(logErr, "could not delete the conversation: %v", err)
		return false
	}
	p.recent = slices.DeleteFunc(p.recent, func(c userdata.ChatMeta) bool { return c.ID == id })
	p.hover = -1 // the rows below the deleted one move up under the mouse
	m.logf(logOk, "deleted the saved conversation %q", title)
	return true
}

// chatDeleteLive deletes the conversation on screen: its file, if it has
// been saved, and then the pane, cleared WITHOUT the save newChat would do —
// that save is exactly what would write the file back.
//
// The pane is cleared by a fresh agent session, not just an empty message
// list: the old session still remembers the deleted conversation, and a
// follow-up answered from that memory would bring it back in all but name.
//
// Like deleting a saved conversation from the mouse, the menu row is the
// deliberate second step; there is no keyboard shortcut to hit by accident.
//
// A file that will not delete leaves the pane as it was, so what the log
// says failed is still there to try again — clearing it anyway would leave a
// file the user believes is gone and no longer sees.
func (m *Model) chatDeleteLive() tea.Cmd {
	p := m.chat
	if err := userdata.RemoveChat(p.dir, p.archiveID); err != nil {
		m.logf(logErr, "could not delete the conversation: %v", err)
		return nil
	}
	title := userdata.ChatTitle(p.archiveMsgs())
	if title == "" {
		title = "(untitled)"
	}
	cmd := m.resetChat()
	m.logf(logOk, "deleted the conversation %q", title)
	return cmd
}

// openSavedChatMenu is the right-click menu of a saved conversation's row,
// in the empty pane or in the Recent conversations list. after runs once the
// conversation is deleted, for the list to redraw without it.
func (m *Model) openSavedChatMenu(x, y int, id string, after func()) {
	m.openMenu(x, y, []menuItem{
		{label: "Open conversation", act: func(m *Model) tea.Cmd {
			m.modal = nil // opened from the list, the list has done its job
			return m.chatOpenSaved(id)
		}},
		{label: "Delete conversation", act: func(m *Model) tea.Cmd {
			if m.chatDelete(id) && after != nil {
				after()
			}
			return nil
		}},
	})
}

// chatRightClick is a right-click in the pane: a saved conversation's row
// gets its own menu, anywhere else the transcript's.
func (m *Model) chatRightClick(x, y int) {
	for _, t := range m.chat.targets {
		if t.kind == targetOpenSaved && t.r.Contains(x, y) {
			m.openSavedChatMenu(x, y, t.text, nil)
			return
		}
	}
	m.openChatMenu(x, y)
}

// ---------------------------------------------------------------------------
// Recent conversations list
// ---------------------------------------------------------------------------

// recentChatsModal lists the saved conversations, newest first; picking one
// opens it in the pane.
//
// DELETE TAKES TWO PRESSES. d (or Delete/Backspace) arms the cursor's row —
// it is marked and says what the next press will do — and the same key again
// deletes it. Any other key, or moving the cursor, disarms; so does Esc,
// which only closes the list once nothing is armed.
type recentChatsModal struct {
	modalBase
	chats []userdata.ChatMeta
	lst   *list
	armed int // the row a second delete press removes; -1 when none
}

// openRecentChats reads the archive afresh — it is opened rarely, and the
// pane's cached list may predate a save — and shows it.
func (m *Model) openRecentChats() {
	p := m.chat
	if p.dir == "" {
		m.log(logWarn, "conversations are not being saved in this session")
		return
	}
	metas, err := userdata.ListChats(p.dir)
	if err != nil {
		m.logf(logErr, "could not read the saved conversations: %v", err)
		return
	}
	p.recent = metas
	chats := p.otherRecent()
	if len(chats) == 0 {
		m.log(logWarn, "no saved conversations yet — each is saved as the assistant answers")
		return
	}
	md := &recentChatsModal{chats: chats, lst: newList(), armed: -1}
	md.refresh(m)
	m.openModal(md)
}

// refresh rebuilds the rows from chats, marking the armed one.
func (rc *recentChatsModal) refresh(m *Model) {
	items := make([]listItem, len(rc.chats))
	for i, c := range rc.chats {
		items[i] = listItem{label: chatRowTitle(c), sub: m.chatRowDetail(c)}
		if i == rc.armed {
			items[i].mark = "✕"
			items[i].sub = "delete? press d again — any other key keeps it"
		}
	}
	rc.lst.set(items)
}

// disarm cancels a pending delete.
func (rc *recentChatsModal) disarm(m *Model) {
	if rc.armed >= 0 {
		rc.armed = -1
		rc.refresh(m)
	}
}

// remove deletes row i and redraws; the list closes once nothing is left.
func (rc *recentChatsModal) remove(m *Model, i int) (closed bool) {
	rc.armed = -1
	if i < 0 || i >= len(rc.chats) || !m.chatDelete(rc.chats[i].ID) {
		rc.refresh(m)
		return false
	}
	rc.chats = slices.Delete(rc.chats, i, i+1)
	rc.refresh(m) // list.set keeps the cursor in range
	return len(rc.chats) == 0
}

func (rc *recentChatsModal) title() string {
	return "Recent conversations · Enter opens · d deletes"
}
func (rc *recentChatsModal) size(w, h int) (int, int) {
	return max(60, w*2/3), min(len(rc.chats)+4, max(12, h*2/3))
}
func (rc *recentChatsModal) draw(m *Model, s Surface) *caret {
	rc.lst.draw(s.Sub(Rect{0, 1, s.W(), s.H() - 1}), m.st, m.st.panel, true, "")
	return nil
}
func (rc *recentChatsModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		if rc.armed >= 0 {
			rc.disarm(m)
			return nil, false
		}
		return nil, true
	case "d", "delete", "backspace":
		if rc.armed == rc.lst.cur {
			return nil, rc.remove(m, rc.lst.cur)
		}
		if rc.lst.cur >= 0 && rc.lst.cur < len(rc.chats) {
			rc.armed = rc.lst.cur
			rc.refresh(m)
		}
		return nil, false
	}
	rc.disarm(m)
	if _, picked := rc.lst.key(k); picked {
		return m.chatOpenSaved(rc.chats[rc.lst.cur].ID), true
	}
	return nil, false
}
func (rc *recentChatsModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	if m.modalClose().Contains(x, y) {
		return nil, true
	}
	if i := rc.lst.indexAt(x, y); i >= 0 {
		return m.chatOpenSaved(rc.chats[i].ID), true
	}
	return nil, false
}

// rightClick offers Open and Delete for the row under the mouse, moving the
// cursor onto it first so the menu and the keyboard act on the same row.
func (rc *recentChatsModal) rightClick(m *Model, x, y int) tea.Cmd {
	i := rc.lst.indexAt(x, y)
	if i < 0 {
		return nil
	}
	rc.lst.cur = i
	rc.disarm(m)
	id := rc.chats[i].ID
	m.openSavedChatMenu(x, y, id, func() {
		if j := slices.IndexFunc(rc.chats, func(c userdata.ChatMeta) bool { return c.ID == id }); j >= 0 {
			rc.chats = slices.Delete(rc.chats, j, j+1)
		}
		if len(rc.chats) == 0 {
			m.modal = nil
			return
		}
		rc.refresh(m)
	})
	return nil
}
func (rc *recentChatsModal) hover(m *Model, x, y int)     { rc.lst.hover = rc.lst.indexAt(x, y) }
func (rc *recentChatsModal) wheel(m *Model, x, y, dy int) { rc.lst.scroll(dy) }
