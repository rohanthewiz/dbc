package web

import (
	"bufio"
	"strconv"
	"strings"
	"testing"
	"time"
)

// N-335: a window's result rows go at the idle release only when its page
// said goodbye (POST …/release on pagehide) and no stream came back. A
// window that just lost its stream — a laptop asleep, a tab Chrome
// discarded — keeps them until it is forgotten, as before.

// detach closes s and waits for the hub to stop counting it.
func detach(t *testing.T, tb *tab, s interface{ Close() error }) {
	t.Helper()
	_ = s.Close()
	for deadline := time.Now().Add(3 * time.Second); tb.win.sse.ClientCount() > 0; {
		if time.Now().After(deadline) {
			t.Fatal("the stream never detached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// reapPastRelease runs the reaper past conn_idle_timeout (the test env's is an
// hour) and waits until the tab's session is gone, which releaseIdle does
// first: anything it does to the rows is done by then, or just after.
func reapPastRelease(t *testing.T, e *testEnv, tb *tab) {
	t.Helper()
	now := time.Now()
	e.srv.hub.reap(now) // starts the idle clock
	e.srv.hub.reap(now.Add(2 * time.Hour))
	for deadline := time.Now().Add(3 * time.Second); ; {
		if conn, _ := tb.ws.Session(); conn == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle session was not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // ReleaseResults runs right after the session's Close
}

func TestIdleReleaseDropsRowsAfterGoodbye(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	tb, _ := e.srv.hub.get(id)
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 1 AS a UNION ALL SELECT 2", 0, false), 200)
	s.await(t, "run")
	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if !st.HasResult || len(st.ResultTabs) != 1 || st.ResultTabs[0].Rows != 2 {
		t.Fatalf("before: %+v", st)
	}
	tabID, seq := st.ResultTabs[0].ID, st.ResultTabs[0].Seq
	// pin it: the release must keep the pin, and a rerun must refill a
	// pinned tab in place rather than open a new one
	e.api("POST", "/api/v1/ws/"+id+"/result-tab", `{"id":`+strconv.Itoa(tabID)+`,"op":"pin"}`, 200)
	// a page of it, as the grid fetches: the tab's grid cache now holds
	// the same rows, and must let go of them too
	e.api("GET", "/api/v1/ws/"+id+"/result", "", 200)

	// the page goes: its goodbye, then its stream
	e.api("POST", "/api/v1/win/"+tb.win.id+"/release", `{}`, 200)
	detach(t, tb, s.body)
	reapPastRelease(t, e, tb)
	// the note is stored last, after the rows and the cache went
	for deadline := time.Now().Add(3 * time.Second); tb.dropped.Load() == nil; {
		if time.Now().After(deadline) {
			t.Fatal("the release left no note about the rows")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if r := tb.ws.LastResult(); r != nil {
		t.Fatalf("the rows outlived the release: %d rows", len(r.Rows))
	}
	tb.viewMu.Lock()
	cached := tb.rv.res
	tb.viewMu.Unlock()
	if cached != nil {
		t.Fatal("the grid's cache still holds the released rows")
	}
	st = decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if st.HasResult || len(st.ResultTabs) != 1 {
		t.Fatalf("after the release: %+v", st)
	}
	rt := st.ResultTabs[0]
	if !rt.Released || rt.Rows != 0 || !rt.Pinned || rt.ID != tabID || rt.Stmt == "" {
		t.Fatalf("the released tab = %+v, want it kept, pinned, with its statement and no rows", rt)
	}
	if pg := e.api("GET", "/api/v1/ws/"+id+"/result", "", 200); len(pg.Data) != 0 && string(pg.Data) != "null" {
		t.Fatalf("a released result's page = %s, want none", pg.Data)
	}

	// the page comes back on its own window's stream (as app.js attaches:
	// /win/:id/events) and hears what went — that stream did not say the
	// reaper's notes at all before N-335
	res := e.req("GET", "/api/v1/win/"+tb.win.id+"/events", "", nil)
	heard := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "results were released") && strings.Contains(sc.Text(), "1 result tab") {
				heard <- true
				return
			}
		}
		heard <- false
	}()
	select {
	case ok := <-heard:
		if !ok {
			t.Fatal("the stream ended without the release's note")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reattached page was not told its results were released")
	}
	e.srv.hub.mu.Lock()
	gb := tb.win.goodbye
	e.srv.hub.mu.Unlock()
	if gb {
		t.Error("a stream attached and the goodbye still stands")
	}

	// r reruns it: the rows come back in the same tab, still pinned, its
	// rerunOf naming the result the page last saw
	e.api("POST", "/api/v1/ws/"+id+"/rerun", `{"id":`+strconv.Itoa(tabID)+`}`, 200)
	for deadline := time.Now().Add(5 * time.Second); tb.ws.LastResult() == nil; {
		if time.Now().After(deadline) {
			t.Fatal("the rerun never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	st = decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	rt = st.ResultTabs[0]
	if !st.HasResult || len(st.ResultTabs) != 1 || rt.Released || rt.Rows != 2 || !rt.Pinned || rt.RerunOf != seq {
		t.Fatalf("after the rerun: %+v, want the tab refilled in place (rerunOf %d)", st, seq)
	}
	_ = res.Body.Close()
}

// No goodbye — the stream just went — and the rows stay through the idle
// release; the session still goes. A goodbye followed by a stream (a
// reload) counts for nothing either.
func TestIdleReleaseKeepsRowsWithoutGoodbye(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream lost", true: "reloaded"}[reload], func(t *testing.T) {
			e := newTestEnv(t)
			id, s := e.connected()
			tb, _ := e.srv.hub.get(id)
			e.api("POST", "/api/v1/ws/"+id+"/run", runBody("SELECT 1 AS a", 0, false), 200)
			s.await(t, "run")
			if reload {
				e.api("POST", "/api/v1/win/"+tb.win.id+"/release", `{}`, 200)
				detach(t, tb, s.body)
				res := e.req("GET", "/api/v1/win/"+tb.win.id+"/events", "", nil)
				for deadline := time.Now().Add(3 * time.Second); tb.win.sse.ClientCount() == 0; {
					if time.Now().After(deadline) {
						t.Fatal("the reload's stream never attached")
					}
					time.Sleep(5 * time.Millisecond)
				}
				// later the laptop sleeps: the stream goes, with no goodbye
				detach(t, tb, res.Body)
			} else {
				detach(t, tb, s.body)
			}
			reapPastRelease(t, e, tb)
			st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
			if !st.HasResult || len(st.ResultTabs) != 1 || st.ResultTabs[0].Released || st.ResultTabs[0].Rows != 1 {
				t.Fatalf("the rows went without a goodbye: %+v", st)
			}
		})
	}
}
