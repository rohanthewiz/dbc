package web

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// "inuse" (inuse.go): every window learns which connection each query tab
// is on — its own tabs by id, another window's without one — as a snapshot
// on each change, and in GET /api/v1/win/:id for a page that just attached.

// awaitInUse reads "inuse" events off s until one satisfies ok, so the
// announcements of earlier steps (each connect sends two) are skipped
// rather than mistaken for this step's.
func awaitInUse(t *testing.T, s *stream, what string, ok func(inUseEvent) bool) inUseEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, open := <-s.events:
			if !open {
				t.Fatalf("stream closed waiting for inuse: %s", what)
			}
			if ev.Type != "inuse" {
				continue
			}
			if ev.WS != "" {
				t.Fatalf("inuse is a window event, got ws %q", ev.WS)
			}
			u := decodeData[inUseEvent](t, testEnvelope{Data: ev.Data})
			if ok(u) {
				return u
			}
		case <-deadline:
			t.Fatalf("no inuse event: %s", what)
		}
	}
}

// has reports whether u lists a tab on conn with id ws ("" for another
// window's tab).
func (u inUseEvent) has(conn, ws string) bool {
	return slices.Contains(u.Tabs, inUseTab{Conn: conn, WS: ws})
}

func TestInUseAcrossTabsAndWindows(t *testing.T) {
	e := newTestEnv(t)
	a1, sa := e.connected()
	winA := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+a1, "", 200)).Win
	b1, _ := e.connected() // a second window

	// A page attaching now gets the snapshot from its window's state: its
	// own tab named, the other window's not.
	w := decodeData[winState](t, e.api("GET", "/api/v1/win/"+winA, "", 200))
	if len(w.InUse.Tabs) != 2 || !w.InUse.has("demo-sqlite", a1) || !w.InUse.has("demo-sqlite", "") {
		t.Fatalf("win snapshot = %+v, want %s and another window's tab on demo-sqlite", w.InUse, a1)
	}
	for _, u := range w.InUse.Tabs {
		if u.WS == b1 {
			t.Fatalf("another window's tab id %s leaked into window A's snapshot", b1)
		}
	}

	// The other window's tab disconnecting takes its entry out of A's.
	e.api("POST", "/api/v1/ws/"+b1+"/disconnect", "", 200)
	awaitInUse(t, sa, "after b1's disconnect", func(u inUseEvent) bool {
		return len(u.Tabs) == 1 && u.has("demo-sqlite", a1)
	})

	// A second tab in A shows up by id once connected, and goes when closed.
	a2 := e.openIn(winA, sa)
	awaitInUse(t, sa, "after a2's connect", func(u inUseEvent) bool {
		return len(u.Tabs) == 2 && u.has("demo-sqlite", a1) && u.has("demo-sqlite", a2)
	})
	e.api("DELETE", "/api/v1/ws/"+a2, "", 200)
	awaitInUse(t, sa, "after a2 closed", func(u inUseEvent) bool {
		return len(u.Tabs) == 1 && u.has("demo-sqlite", a1)
	})
}

// A tab with no connection is not listed, and the list is [] rather than
// null, so the page can always iterate it.
func TestInUseEmpty(t *testing.T) {
	e := newTestEnv(t)
	a, _ := e.connected()
	e.api("POST", "/api/v1/ws/"+a+"/disconnect", "", 200)
	win := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+a, "", 200)).Win
	env := e.api("GET", "/api/v1/win/"+win, "", 200)
	w := decodeData[struct {
		InUse json.RawMessage `json:"inUse"`
	}](t, env)
	if string(w.InUse) != `{"tabs":[]}` {
		t.Fatalf("inUse = %s, want {\"tabs\":[]}", w.InUse)
	}
}
