package web

import (
	"testing"
	"time"
)

// Claims (claims.go): a saved query tab is shown and saved by one browser
// tab — one window — at a time.

// openWin opens a window with its stream attached, as a page's boot does,
// and returns the window's id.
func (e *testEnv) openWin() (string, *stream) {
	e.t.Helper()
	id, s := e.open()
	tb, _ := e.srv.hub.get(id)
	return tb.win.id, s
}

// claimAll claims every free saved tab for window win. With boot, it is a
// page's boot claim — which a page makes before its stream attaches, so on
// a window whose stream is up (openWin's) it stands for a reload (after a
// release) or a duplicated browser tab.
func (e *testEnv) claimAll(win string, boot bool, want int) claimResp {
	e.t.Helper()
	body := `{}`
	if boot {
		body = `{"boot":true}`
	}
	env := e.api("POST", "/api/v1/win/"+win+"/tabs", body, want)
	if want != 200 {
		return claimResp{}
	}
	return decodeData[claimResp](e.t, env)
}

func (e *testEnv) saveIn(win, key, buffer string, want int) {
	e.t.Helper()
	e.api("PUT", "/api/v1/tabs/"+key+"?win="+win, `{"title":"Q","conn":"demo-sqlite","buffer":"`+buffer+`"}`, want)
}

func savedBuffer(t *testing.T, e *testEnv, key string) string {
	t.Helper()
	for _, tb := range decodeData[[]Tab](t, e.api("GET", "/api/v1/tabs", "", 200)) {
		if tb.ID == key {
			return tb.Buffer
		}
	}
	t.Fatalf("tab %q not saved", key)
	return ""
}

// A second window does not get the first one's tabs, and cannot save or
// delete them; the first saves them as ever.
func TestSecondWindowGetsNoClaimedTabs(t *testing.T) {
	e := newTestEnv(t)
	a, _ := e.openWin()
	e.saveIn(a, "k1", "SELECT 1", 200) // a new tab's first save claims it

	b, _ := e.openWin()
	got := e.claimAll(b, false, 200)
	if len(got.Tabs) != 0 || len(got.Held) != 1 || got.Held[0] != "k1" {
		t.Fatalf("second window's claim = %+v, want nothing and k1 held", got)
	}
	e.saveIn(b, "k1", "SELECT 2", 409)
	e.saveIn("", "k1", "SELECT 3", 409)     // no window: still turned away from a held tab
	e.saveIn("gone", "k1", "SELECT 4", 409) // an unknown window likewise
	e.api("DELETE", "/api/v1/tabs/k1?win="+b, "", 409)
	e.saveIn(a, "k1", "SELECT 5", 200)
	if got := savedBuffer(t, e, "k1"); got != "SELECT 5" {
		t.Fatalf("saved buffer = %q, want the holder's", got)
	}

	// B's own tab is B's; A's reload (a boot claim after its release)
	// gets back only its own
	e.saveIn(b, "k2", "SELECT 6", 200)
	e.api("POST", "/api/v1/win/"+a+"/release", "", 200)
	got = e.claimAll(a, true, 200)
	if len(got.Tabs) != 1 || got.Tabs[0].ID != "k1" || len(got.Held) != 1 || got.Held[0] != "k2" {
		t.Fatalf("reload's claim = %+v, want k1 and k2 held", got)
	}
}

// A window's goodbye frees its tabs at once, with its last save made first.
func TestReleaseSavesThenFrees(t *testing.T) {
	e := newTestEnv(t)
	a, _ := e.openWin()
	e.saveIn(a, "k1", "SELECT 1", 200)
	e.api("POST", "/api/v1/win/"+a+"/release",
		`{"save":{"id":"k1","title":"Q","conn":"demo-sqlite","buffer":"SELECT last"}}`, 200)
	b, _ := e.openWin()
	got := e.claimAll(b, false, 200)
	if len(got.Tabs) != 1 || got.Tabs[0].Buffer != "SELECT last" || len(got.Held) != 0 {
		t.Fatalf("claim after release = %+v", got)
	}
	e.saveIn(a, "k1", "SELECT stale", 409) // A's copy is B's now
	// a release from a window the server no longer has still saves
	e.api("POST", "/api/v1/win/gone/release", `{"save":{"id":"k9","title":"Q","conn":"","buffer":"x"}}`, 200)
	if savedBuffer(t, e, "k9") != "x" {
		t.Fatal("an unknown window's release did not save")
	}
}

// A window whose stream has been gone past the grace holds nothing; within
// it (a reload, a reconnect) it keeps its tabs.
func TestClaimLapsesAfterGrace(t *testing.T) {
	e := newTestEnv(t)
	a, sa := e.openWin()
	e.saveIn(a, "k1", "SELECT 1", 200)
	w, _ := e.srv.hub.window(a)
	_ = sa.body.Close()
	for deadline := time.Now().Add(3 * time.Second); w.sse.ClientCount() > 0; {
		if time.Now().After(deadline) {
			t.Fatal("the stream never detached")
		}
		time.Sleep(5 * time.Millisecond)
	}

	b, _ := e.openWin()
	if got := e.claimAll(b, false, 200); len(got.Held) != 1 {
		t.Fatalf("within the grace: %+v, want k1 held", got)
	}
	w.detached.Store(time.Now().Add(-2 * claimGrace).UnixNano())
	if got := e.claimAll(b, false, 200); len(got.Tabs) != 1 || got.Tabs[0].ID != "k1" {
		t.Fatalf("past the grace: %+v, want k1", got)
	}
	// A comes back: its reclaim finds k1 taken
	got := decodeData[claimResp](t, e.api("POST", "/api/v1/win/"+a+"/tabs", `{"keys":["k1"]}`, 200))
	if len(got.Claimed) != 0 || len(got.Held) != 1 {
		t.Fatalf("reclaim = %+v, want k1 held", got)
	}
}

// A duplicated browser tab boots on the original's window id while the
// original is showing it: refused, so the copy opens its own window.
func TestDuplicatedBrowserTabIsRefused(t *testing.T) {
	e := newTestEnv(t)
	a, _ := e.openWin()
	e.claimAll(a, true, 409)
	// a reload: pagehide's release first, then the boot
	e.api("POST", "/api/v1/win/"+a+"/release", "", 200)
	e.claimAll(a, true, 200)
	// a window's first page claims before its stream attaches: allowed
	st := decodeData[wsState](t, e.api("POST", "/api/v1/ws", "", 200))
	e.claimAll(st.Win, true, 200)
}

// A layout write names only the writer's tabs; the order and plans of
// another live window's tabs are kept.
func TestLayoutKeepsOtherWindowsTabs(t *testing.T) {
	e := newTestEnv(t)
	a, _ := e.openWin()
	b, _ := e.openWin()
	e.saveIn(a, "k1", "", 200)
	e.saveIn(a, "k2", "", 200)
	e.saveIn(b, "k3", "", 200)
	e.api("PUT", "/api/v1/layout?win="+a, `{"tabs":"k2,k1","plans":"k1"}`, 200)
	e.api("PUT", "/api/v1/layout?win="+b, `{"tabs":"k3","plans":"k3"}`, 200)
	l := decodeData[map[string]string](t, e.api("GET", "/api/v1/layout", "", 200))
	if l["tabs"] != "k3,k2,k1" || l["plans"] != "k3,k1" {
		t.Fatalf("layout = %v", l)
	}
	// A closes k1: gone from A's next write, and B's does not bring it back
	e.api("DELETE", "/api/v1/tabs/k1?win="+a, "", 200)
	e.api("PUT", "/api/v1/layout?win="+a, `{"tabs":"k2","plans":""}`, 200)
	e.api("PUT", "/api/v1/layout?win="+b, `{"tabs":"k3","plans":""}`, 200)
	l = decodeData[map[string]string](t, e.api("GET", "/api/v1/layout", "", 200))
	if l["tabs"] != "k3,k2" || l["plans"] != "" {
		t.Fatalf("layout after close = %v", l)
	}
}

func TestMergeTabKeys(t *testing.T) {
	for _, c := range []struct {
		given, old string
		elsewhere  map[string]bool
		want       string
	}{
		{"a,b", "x,a,y,b", map[string]bool{"x": true}, "a,b,x"},
		{"", "x,y", map[string]bool{"y": true}, "y"},
		{"a", "a", map[string]bool{"a": true}, "a"}, // not doubled
		{"a", "", nil, "a"},
	} {
		if got := mergeTabKeys(c.given, c.old, c.elsewhere); got != c.want {
			t.Errorf("mergeTabKeys(%q, %q) = %q, want %q", c.given, c.old, got, c.want)
		}
	}
}
