package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// otherPortRefused: a page served from another 127.0.0.1 port cannot reach
// dbc web's API with the browser's session cookie (N-274).
//
// The premise is browser behaviour, so it is checked in a browser: a site
// is scheme + host with no port, so 127.0.0.1:<other> is same-site with dbc
// and SameSite=Strict still attaches the cookie. The step proves that first
// (the cookie went out, Sec-Fetch-Site said "same-site"), then that dbc
// answered 403. Read off the DevTools protocol, since the page making the
// requests cannot see any of it: its responses are opaque by design.
//
// Three shapes of request, one per header the guard reads:
//
//	probe    how it is made                   what tells dbc it is foreign
//	───────  ───────────────────────────────  ────────────────────────────
//	img      <img src>                        Sec-Fetch-Site (no Origin)
//	nocors   fetch, mode no-cors              Sec-Fetch-Site (no Origin)
//	cors     fetch, mode cors (the default)   Origin and Sec-Fetch-Site
//
// conn-tables is the target because it is the GET with an effect: it dials
// the named connection. The workbench's own request to it, the control,
// still answers 200.
func otherPortRefused(t *testing.T, e *env, p *rod.Page) {
	api := e.base + "/api/v1/conn-tables?conn=lite"

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>another port</title><img src="` + api + `&probe=img">`))
	}))
	defer other.Close()
	if !strings.HasPrefix(other.URL, "http://127.0.0.1:") || !strings.HasPrefix(e.base, "http://127.0.0.1:") {
		t.Fatalf("both servers must be on 127.0.0.1 to be same-site: %s and %s", other.URL, e.base)
	}

	// what each probe sent and what came back, by CDP request id. Events
	// arrive on rod's goroutine, the asserts read on this one.
	type seen struct {
		probe, cookie, site, origin string
		status                      int
	}
	var mu sync.Mutex
	byID := map[proto.NetworkRequestID]*seen{}
	get := func(id proto.NetworkRequestID) *seen {
		if byID[id] == nil {
			byID[id] = &seen{}
		}
		return byID[id]
	}
	header := func(h proto.NetworkHeaders, name string) string {
		for k, v := range h {
			if strings.EqualFold(k, name) {
				return v.Str()
			}
		}
		return ""
	}

	q := e.browser.MustPage("") // the same browser profile: it holds the session cookie
	defer q.MustClose()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// subscribed before the navigation, so the <img> request is not missed
	wait := q.Context(ctx).EachEvent(
		func(ev *proto.NetworkRequestWillBeSent) {
			if _, probe, ok := strings.Cut(ev.Request.URL, "&probe="); ok && strings.HasPrefix(ev.Request.URL, api) {
				mu.Lock()
				get(ev.RequestID).probe = probe
				mu.Unlock()
			}
		},
		// the ExtraInfo events carry what went over the wire: the Cookie
		// header and the Sec-Fetch-* headers the browser adds late
		func(ev *proto.NetworkRequestWillBeSentExtraInfo) {
			mu.Lock()
			s := get(ev.RequestID)
			s.cookie, s.site, s.origin = header(ev.Headers, "Cookie"), header(ev.Headers, "Sec-Fetch-Site"), header(ev.Headers, "Origin")
			mu.Unlock()
		},
		func(ev *proto.NetworkResponseReceivedExtraInfo) {
			mu.Lock()
			get(ev.RequestID).status = ev.StatusCode
			mu.Unlock()
		},
	)
	go wait()

	q.MustNavigate(other.URL)
	q.MustWaitLoad()
	// both fetches settle (the cors one rejects: no CORS headers come back,
	// which is how it should be) before the page is read
	if _, err := q.Eval(`async (u) => {
	  await fetch(u + "&probe=nocors", { mode: "no-cors", credentials: "include" }).catch(() => {});
	  await fetch(u + "&probe=cors", { credentials: "include" }).catch(() => {});
	}`, api); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"img": true, "nocors": true, "cors": true}
	var got map[string]seen
	deadline := time.Now().Add(10 * time.Second)
	for {
		got = map[string]seen{}
		mu.Lock()
		for _, s := range byID {
			if want[s.probe] && s.status != 0 && s.site != "" {
				got[s.probe] = *s
			}
		}
		mu.Unlock()
		if len(got) == len(want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not every probe was seen to completion within 10s: %+v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}

	for probe, s := range got {
		// the premise: the browser did send the session, as same-site
		if !strings.Contains(s.cookie, "dbc_session_") {
			t.Errorf("%s: no session cookie went out (Cookie %q) — the browser no longer sends it same-site, so the step proves nothing", probe, s.cookie)
		}
		if s.site != "same-site" {
			t.Errorf("%s: Sec-Fetch-Site = %q, want same-site", probe, s.site)
		}
		// the fix: dbc refused it before the handler ran
		if s.status != http.StatusForbidden {
			t.Errorf("%s from another port: dbc answered %d, want 403", probe, s.status)
		}
	}
	if o := got["cors"].origin; o != other.URL {
		t.Errorf("cors: Origin = %q, want %q", o, other.URL)
	}
	if o := got["img"].origin + got["nocors"].origin; o != "" {
		t.Errorf("a no-cors GET carried an Origin (%q): Sec-Fetch-Site is then not the only thing that catches it", o)
	}

	// the control: the workbench's own request (same-origin) still works
	if n := evalNum(t, p, `async () => (await fetch("/api/v1/conn-tables?conn=lite")).status`); n != 200 {
		t.Fatalf("the workbench's own conn-tables request = %v, want 200", n)
	}
}
