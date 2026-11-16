package tui

import (
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/workspace"
)

// A script's s.Print lines, batched.
//
// s.Print fires on the script's goroutine. It used to go to Update as a
// message of its own through Program.Send, which blocks until the event
// loop takes the message, and Bubble Tea draws a frame after every
// message. So every printed line cost the script one Update and one whole
// frame: a script printing in a loop ran at the TUI's frame rate, and a
// slow frame (a large buffer, a long log) slowed the script down with it.
// A query tab in the background was worse: each line went onto the tab's
// pending list, to be replayed one by one on the user's return, with
// nothing bounding the list (N-278).
//
// Now a line is appended to a queue, and the event loop is woken only when
// the queue goes from empty to not empty:
//
//	script goroutine                       event loop
//	s.Print ─► push ─┬─ queue was empty ─► m.post(printsMsg) ─► Update
//	                 └─ otherwise: return                          │
//	                                                               ▼
//	                       Update, on EVERY message, first: drainPrints
//	                       ─► each line to its connection's log (logTo)
//	                       ─► one frame for the whole batch
//
// The wake-up is POSTED (m.post: Send from a goroutine of its own), not
// sent from the script's goroutine. A Send there would block until the
// loop takes it, which is after the frame it is drawing; the script would
// print one more line, block again, and the next drain would find that one
// line — a frame per line, as before. Posted, the script keeps printing
// while the loop draws, and the next drain takes everything printed
// meanwhile. There is at most one wake-up in flight per drain: it is posted
// only when a push finds the queue empty.
//
// Draining first thing in Update, whatever the message, keeps the order a
// user reads: a script's RunDone (or an s.Show) is a later message than
// the lines printed before it, so those lines are in the log before "done"
// is written under them.
//
// A line from a background tab goes straight to its connection's log, as
// it would have on replay — the logs are per connection, not per tab
// (logs.go) — rather than onto the tab's pending list. A closed tab's
// lines are dropped, as its other events are (routeTab).
//
// BACKPRESSURE. The queue is not allowed to grow without bound behind an
// event loop that is busy elsewhere: once it holds printBacklog lines, the
// script's push SENDS a wake-up from the script's own goroutine, and
// Program.Send blocks until the loop takes it — the old one-line-per-message
// pace, but only while the loop is that far behind. Send returns once the
// program has ended, so a script printing at quit is never stuck here.

// printBacklog is how many lines may wait before a push waits for the
// event loop. A log keeps logMaxLines, so a batch that size is already a
// full log's worth of lines.
const printBacklog = logMaxLines

// printQueue holds printed lines between the script goroutine and Update.
type printQueue struct {
	mu sync.Mutex
	q  []queuedPrint
}

// queuedPrint is one s.Print line: the query tab whose script printed it
// (tabMsg's key) and the connection the run started on.
type queuedPrint struct {
	key  int
	conn string
	text string
}

// printsMsg wakes the event loop to drain the queue. It carries nothing:
// Update drains on every message, so the wake-up only makes sure there is
// one.
type printsMsg struct{}

// wakeKind is what a push asks of its caller.
type wakeKind int

const (
	wakeNone wakeKind = iota // a wake-up is already on its way
	wakePost                 // the queue was empty: post one
	wakeWait                 // the queue is full: send one, and wait for the loop
)

// push queues a line and says how the caller must wake the event loop.
func (p *printQueue) push(key int, ev *workspace.ScriptPrint) wakeKind {
	p.mu.Lock()
	defer p.mu.Unlock()
	wasEmpty := len(p.q) == 0
	p.q = append(p.q, queuedPrint{key: key, conn: ev.Conn, text: ev.Text})
	switch {
	case len(p.q) >= printBacklog:
		return wakeWait
	case wasEmpty:
		return wakePost
	}
	return wakeNone
}

// take empties the queue, returning what it held.
func (p *printQueue) take() []queuedPrint {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := p.q
	p.q = nil
	return q
}

// scriptPrinted is the sink's handling of a ScriptPrint from tab key's
// workspace: queued, and the loop woken when push says so.
func (m *Model) scriptPrinted(key int, ev *workspace.ScriptPrint) {
	switch m.prints.push(key, ev) {
	case wakePost:
		m.post(printsMsg{})
	case wakeWait:
		m.send(printsMsg{})
	}
}

// sendLater is Model.post's default: Send from a goroutine of its own, so
// the caller does not wait for the event loop to take the message. It
// reads m.send when it fires, as the workspace sink does (Run installs
// Program.Send after New).
func (m *Model) sendLater(msg tea.Msg) { go m.send(msg) }

// drainPrints writes every queued line to its connection's log. Update
// calls it before handling any message.
func (m *Model) drainPrints() {
	for _, p := range m.prints.take() {
		if m.tabByKey(p.key) == nil {
			continue // the tab was closed: its run's lines go nowhere
		}
		m.logTo(p.conn, logInfo, p.text)
	}
}
