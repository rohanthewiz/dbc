package web

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/rohanthewiz/rweb"
)

// The log journal: the last log lines each window's stream carried, kept so
// a page can fetch what it did not get (N-266).
//
// Log lines used to live only in the page's DOM, so two things lost them:
//
//   - a reload: the new page starts with empty logs, though the window, its
//     sessions and its runs are the same;
//   - a drop: the stream's SSE hub sends without blocking, and a client
//     whose channel is full misses the event. rweb evicts a client only
//     after MaxDropped drops IN A ROW, and any send that gets through
//     resets the count, so one or two drops in a burst (a script printing
//     in a loop) were lost without a disconnect, and the page's resync
//     never ran. The hub now evicts on the first drop (hub.open,
//     MaxDropped 1): a drop is always a reconnect, and a reconnect always
//     resyncs.
//
// Each line is numbered per window (seq) as it is sent, and the page
// remembers the last number it drew. After a reconnect, and on its first
// attach after a reload, it asks for what came after (GET
// /api/v1/win/:id/log?after=N) and draws those lines as they would have
// been drawn live:
//
//	sendLog ─► seq 41 ─► journal ─► stream ─► page draws, logSeq = 41
//	sendLog ─► seq 42 ─► journal ─► stream ✗ (full: client evicted)
//	sendLog ─► seq 43 ─► journal ─► (no client)
//	page reconnects ─► resync + GET …/log?after=41 ─► 42, 43 drawn
//
// The seq is assigned and the event sent under the journal's lock, so the
// stream carries log lines in seq order and the page can tell a line it
// already has by its number alone.
//
// Only "log" events are journaled. The rest are state the page fetches
// whole on a resync (the tab's result, busy and session marks; runs,
// connections and claims) or keeps its own record of (a run's lines are in
// its run record), so a missed one is made good without being replayed.

// journalLines and journalBytes bound a window's journal: the newest lines
// up to either limit. The page keeps 500 lines per log (core.js LOG_MAX);
// a window has a few logs, so a thousand lines covers a reload, and a
// megabyte keeps a script printing large values from holding much memory
// for a window nobody is looking at.
const (
	journalLines = 1000
	journalBytes = 1 << 20
)

// journalEntry is one kept line, as GET …/log returns it: the same shape as
// the "log" event that carried it.
type journalEntry struct {
	Seq  uint64  `json:"seq"`
	WS   string  `json:"ws,omitempty"`
	Data logLine `json:"data"`
}

// journal is a window's kept log lines, oldest first.
type journal struct {
	mu    sync.Mutex
	seq   uint64 // the last seq given; 0 before the first line
	lines []journalEntry
	bytes int
}

// size is what an entry counts against journalBytes: its text, which is
// what can be large; the rest is a few dozen bytes.
func (e journalEntry) size() int { return len(e.Data.Text) + len(e.Data.Conn) + 64 }

// addLocked numbers line, stamps it with now, keeps it and returns it,
// dropping the oldest lines past the limits. j.mu is held.
func (j *journal) addLocked(ws string, l logLine, now time.Time) journalEntry {
	j.seq++
	l.At = now.UnixMilli()
	e := journalEntry{Seq: j.seq, WS: ws, Data: l}
	j.lines = append(j.lines, e)
	j.bytes += e.size()
	drop := 0
	for len(j.lines)-drop > journalLines || (j.bytes > journalBytes && len(j.lines)-drop > 1) {
		j.bytes -= j.lines[drop].size()
		drop++
	}
	if drop > 0 {
		// copied down rather than resliced, so the dropped lines' text is
		// not held by the array behind the slice
		j.lines = append(j.lines[:0], j.lines[drop:]...)
	}
	return e
}

// journalTail is GET …/log's answer: the lines after the page's last, and
// First, the oldest seq still kept — after+1 < First means lines were lost
// to the limits, which the page says. Next is the seq the next line gets.
type journalTail struct {
	Lines []journalEntry `json:"lines"`
	First uint64         `json:"first"`
	Next  uint64         `json:"next"`
}

// after returns the kept lines numbered above seq.
func (j *journal) after(seq uint64) journalTail {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := journalTail{Lines: []journalEntry{}, First: j.seq + 1, Next: j.seq + 1}
	if len(j.lines) > 0 {
		out.First = j.lines[0].Seq
	}
	for _, e := range j.lines {
		if e.Seq > seq {
			out.Lines = append(out.Lines, e)
		}
	}
	return out
}

// sendLog puts one log line on the window's stream, numbered and kept (see
// above). ws is the query tab it belongs to; "" is the window's own (the
// assistant's).
func (w *window) sendLog(ws string, l logLine) {
	w.log.mu.Lock()
	defer w.log.mu.Unlock()
	e := w.log.addLocked(ws, l, time.Now())
	b, err := json.Marshal(wireEvent{Type: "log", WS: ws, Seq: e.Seq, Data: e.Data})
	if err != nil {
		return // a logLine is plain strings; this cannot happen
	}
	w.sse.BroadcastRaw(rweb.SSEvent{Type: "message", Data: string(b)})
}

// handleWindowLog is GET /api/v1/win/:id/log?after=N: the window's kept
// log lines numbered above N (0, or no after: all of them).
func (s *Server) handleWindowLog(ctx rweb.Context) error {
	w, err := s.hub.window(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var after uint64
	if v := ctx.Request().QueryParam("after"); v != "" {
		if after, err = strconv.ParseUint(v, 10, 64); err != nil {
			return fail(ctx, badRequest("after is a log line's number, not %q", v))
		}
	}
	return ok(ctx, w.log.after(after))
}
