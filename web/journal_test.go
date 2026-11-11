package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/rweb"
)

// The journal (journal.go) numbers a window's log lines, keeps the newest
// within its limits, and hands back those after a number.
func TestJournalKeepsTheNewestLines(t *testing.T) {
	var j journal
	at := time.UnixMilli(1_700_000_000_000)
	for i := 1; i <= journalLines+5; i++ {
		j.mu.Lock()
		e := j.addLocked("ws1", logLine{Level: "info", Text: fmt.Sprint("line ", i)}, at)
		j.mu.Unlock()
		if e.Seq != uint64(i) || e.Data.At != at.UnixMilli() {
			t.Fatalf("line %d: seq %d at %d", i, e.Seq, e.Data.At)
		}
	}
	all := j.after(0)
	if len(all.Lines) != journalLines || all.First != 6 || all.Next != journalLines+6 {
		t.Fatalf("after(0): %d lines, first %d, next %d; want %d from 6, next %d",
			len(all.Lines), all.First, all.Next, journalLines, journalLines+6)
	}
	tail := j.after(journalLines + 3)
	if len(tail.Lines) != 2 || tail.Lines[0].Data.Text != fmt.Sprint("line ", journalLines+4) || tail.Lines[1].WS != "ws1" {
		t.Fatalf("after(n): %+v", tail.Lines)
	}
	if none := j.after(journalLines + 5); len(none.Lines) != 0 || none.Lines == nil {
		t.Fatalf("after(last) = %+v, want an empty list (not null, for the page)", none.Lines)
	}

	// the byte limit: a few big lines push the older ones out, but the
	// newest is kept even when it alone is over
	var b journal
	big := strings.Repeat("x", journalBytes/3)
	for range 4 {
		b.mu.Lock()
		b.addLocked("", logLine{Text: big}, at)
		b.mu.Unlock()
	}
	if got := b.after(0); len(got.Lines) != 2 || got.First != 3 || b.bytes > journalBytes {
		t.Fatalf("by bytes: %d lines from %d, %d bytes; want 2 from 3 within %d", len(got.Lines), got.First, b.bytes, journalBytes)
	}
	var one journal
	one.mu.Lock()
	one.addLocked("", logLine{Text: strings.Repeat("y", 2*journalBytes)}, at)
	one.mu.Unlock()
	if got := one.after(0); len(got.Lines) != 1 {
		t.Fatalf("an oversized line alone: %d kept, want 1", len(got.Lines))
	}
}

// A tab's log lines go out numbered and stamped, and GET …/log gives a
// page the ones after the last it drew — what a reconnect or a reload
// asks for (N-266).
func TestWindowLogCatchUp(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.open()
	tb, _ := e.srv.hub.get(id)
	win := tb.win.id
	tb.logOn("lite", "info", "first")
	tb.logOn("lite", "warn", "second")
	tb.win.send("log", "", logLine{Level: "warn", Text: "the window's own"}) // the assistant's path

	// live, each line comes stamped with its time (its number is
	// TestLogEventsCarrySeq's: the stream helper keeps type, ws and data)
	for range 3 {
		ev, _ := s.await(t, "log")
		var l logLine
		_ = json.Unmarshal(ev.Data, &l)
		if l.At == 0 {
			t.Fatalf("log line %q not stamped with its time", l.Text)
		}
	}

	all := decodeData[journalTail](t, e.api("GET", "/api/v1/win/"+win+"/log", "", 200))
	if len(all.Lines) != 3 || all.First != 1 || all.Next != 4 {
		t.Fatalf("all = %+v", all)
	}
	if l := all.Lines[0]; l.Seq != 1 || l.WS != id || l.Data.Text != "first" || l.Data.Conn != "lite" {
		t.Fatalf("line 1 = %+v", l)
	}
	if l := all.Lines[2]; l.WS != "" || l.Data.Text != "the window's own" {
		t.Fatalf("line 3 = %+v", l)
	}
	tail := decodeData[journalTail](t, e.api("GET", "/api/v1/win/"+win+"/log?after=2", "", 200))
	if len(tail.Lines) != 1 || tail.Lines[0].Seq != 3 {
		t.Fatalf("after=2: %+v", tail)
	}
	e.api("GET", "/api/v1/win/"+win+"/log?after=-1", "", 400)
	e.api("GET", "/api/v1/win/nope/log", "", 404)
}

// The stream's seq field: every "log" event carries its number, in order,
// so the page can tell a line it already drew.
func TestLogEventsCarrySeq(t *testing.T) {
	e := newTestEnv(t)
	id, _ := e.open()
	tb, _ := e.srv.hub.get(id)
	ch := make(chan any, 8)
	tb.win.sse.Register(ch)
	defer tb.win.sse.Unregister(ch)
	tb.logOn("", "info", "a")
	tb.logOn("", "info", "b")
	for want := uint64(1); want <= 2; want++ {
		v := <-ch
		ev, ok := v.(rweb.SSEvent)
		if !ok {
			t.Fatalf("got %T", v)
		}
		var w struct {
			Type string `json:"type"`
			Seq  uint64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(ev.Data.(string)), &w); err != nil || w.Type != "log" || w.Seq != want {
			t.Fatalf("event %s: %+v (%v), want log seq %d", ev.Data, w, err, want)
		}
	}
}

// A client that misses ONE event is dropped from the window's stream, so
// its EventSource reconnects and the page resyncs (N-266). With rweb's
// count of drops in a row (3, reset by any send that gets through), one or
// two drops in a burst were lost silently: no reconnect, no resync, and a
// lost "run" left the tab busy.
func TestStreamDropsAClientAtItsFirstMiss(t *testing.T) {
	e := newTestEnv(t)
	id, _ := e.open()
	tb, _ := e.srv.hub.get(id)
	before := tb.win.sse.ClientCount()
	full := make(chan any) // unbuffered, never read: every send misses
	tb.win.sse.Register(full)
	if tb.win.sse.ClientCount() != before+1 {
		t.Fatal("the stuck client was not registered")
	}
	tb.send("busy", map[string]string{"status": "running"})
	if got := tb.win.sse.ClientCount(); got != before {
		t.Fatalf("after one missed event: %d clients, want the stuck one dropped (%d)", got, before)
	}
	if _, open := <-full; open {
		t.Fatal("the dropped client's channel is still open")
	}
}
