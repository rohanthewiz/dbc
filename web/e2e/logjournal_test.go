package e2e

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

// logJournal: the log's lines outlive a reload, and the ones a dropped
// stream did not bring are drawn once it is back (N-266). They used to live
// only in the page's DOM: a reload started every log empty, and a line sent
// while the stream was down — or dropped from a full one — was gone. Now
// the server keeps the window's last lines, numbered (web/journal.go), and
// the page fetches those after the last it drew whenever the stream opens.
//
// In a browser window of its own. A run that fails is the line to look
// for: its error names the column, so each is told apart. The run whose
// lines are dropped goes to dbc web directly, as curl or a script would.
func logJournal(t *testing.T, e *env, _ *rod.Page) {
	q := e.page(t, "lite")
	defer q.MustClose()
	logHas := func(what, s string) {
		t.Helper()
		waitFor(t, q, what, `(s) => document.getElementById("log").textContent.includes(s)`, s)
	}

	// 1. a line, then a reload: the line is there again, with its time
	eval(t, q, `() => dbc.editor.setText("SELECT nope_before_reload")`)
	q.MustElement("#run").MustClick()
	logHas("the run's error", "nope_before_reload")
	stamp := evalStr(t, q, `() => [...document.querySelectorAll("#log > div")]
	  .find((d) => d.textContent.includes("nope_before_reload")).querySelector(".t").textContent`)
	// a run may say the column more than once (its statement, its error):
	// the reload must draw each of those lines once, no more
	count := func(s string) float64 {
		return evalNum(t, q, `(s) => [...document.querySelectorAll("#log > div")].filter((d) => d.textContent.includes(s)).length`, s)
	}
	before := count("nope_before_reload")
	time.Sleep(1100 * time.Millisecond) // a reload's own time would differ from the line's
	q.MustReload()
	q.MustWaitLoad()
	waitConnected(t, q, "lite")
	logHas("the line back after the reload", "nope_before_reload")
	if got := evalStr(t, q, `() => [...document.querySelectorAll("#log > div")]
	  .find((d) => d.textContent.includes("nope_before_reload")).querySelector(".t").textContent`); got != stamp {
		t.Fatalf("the line came back stamped %s, want its own time %s", got, stamp)
	}
	// once, not twice: the catch-up and the stream agree on what is drawn
	if n := count("nope_before_reload"); n != before {
		t.Fatalf("the run's lines are drawn %v times after the reload, want %v as before it", n, before)
	}

	// 2. a stream that drops events: the run's lines are lost on the way
	// in (dropped in the page — CDP's offline mode does not cut a stream
	// already open, and dbc web has no switch to cut one), then the
	// stream's reconnect is played (its onopen, as after an eviction —
	// which dbc web now makes of any drop, web.TestStreamDropsAClientAtItsFirstMiss):
	// the page fetches the lines it missed and draws them, once each
	ws := evalStr(t, q, `() => dbc.state.ws`)
	win := evalStr(t, q, `() => dbc.state.win`)
	eval(t, q, `() => {
	  const src = dbc.state.source, real = src.onmessage;
	  src.onmessage = (e) => { if (!e.data.includes("nope_while_away")) real(e); };
	  window.__e2eReconnect = () => { src.onmessage = real; src.onopen(); };
	}`)
	api := func(method, path, body string) string {
		t.Helper()
		req, _ := http.NewRequest(method, e.base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	// how many lines the page shows for a run like the first: the missed
	// run's are drawn as many times
	perRun := before
	api("POST", "/api/v1/ws/"+ws+"/run", `{"buffer":"SELECT nope_while_away","caret":0}`)
	for deadline := time.Now().Add(waitLimit); !strings.Contains(api("GET", "/api/v1/win/"+win+"/log", ""), "nope_while_away"); {
		if time.Now().After(deadline) {
			t.Fatal("the run's error never reached the window's journal")
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // the stream has carried (and dropped) them by now
	if strings.Contains(evalStr(t, q, `() => document.getElementById("log").textContent`), "nope_while_away") {
		t.Fatal("the run's lines reached the log though their events were dropped")
	}
	eval(t, q, `() => window.__e2eReconnect()`)
	logHas("the missed lines drawn after the reconnect", "nope_while_away")
	waitFor(t, q, "all the missed run's lines", `(n) => [...document.querySelectorAll("#log > div")]
	  .filter((d) => d.textContent.includes("nope_while_away")).length >= n`, perRun)
	time.Sleep(300 * time.Millisecond) // a second drawing would land by now
	if n := count("nope_while_away"); n != perRun {
		t.Fatalf("the missed run's lines are drawn %v times, want %v", n, perRun)
	}
}
