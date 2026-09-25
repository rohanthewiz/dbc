package web

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/explain"
)

// Explain in the browser: the Plan tab's server half. SQLite's plans carry
// no costs or timings, which is what the demo has; the view's handling of
// measured plans is the standalone page's (explain/html_test.go), since the
// two are one script.

func explainBody(buffer string, analyze, again bool) string {
	b, _ := json.Marshal(explainReq{runReq: runReq{Buffer: buffer}, Analyze: analyze, Again: again})
	return string(b)
}

// explainAndWait explains and waits for the outcome on the stream.
func (e *testEnv) explainAndWait(id string, s *stream, body string) (explainEvent, []string) {
	e.t.Helper()
	e.api("POST", "/api/v1/ws/"+id+"/explain", body, 200)
	ev, logs := s.await(e.t, "explain")
	return decodeData[explainEvent](e.t, testEnvelope{Data: ev.Data}), logs
}

// The whole loop: explain from the editor, the event, the plan the view
// mounts, and the TUI's words in the log.
func TestExplainShowsPlan(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	const q = "SELECT name FROM cats WHERE age > 2 ORDER BY name"
	ex, logs := e.explainAndWait(id, s, explainBody(q, false, false))
	if !ex.OK || !ex.HasPlan || !strings.HasPrefix(ex.Status, "plan · ") || ex.Tag != "explain" {
		t.Fatalf("explain event = %+v (logs %q)", ex, logs)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "explained in ") {
		t.Errorf("logs = %q, want the TUI's \"explained in …\"", logs)
	}
	var out struct {
		planOut
		Doc struct {
			Headline  string   `json:"headline"`
			Metrics   []string `json:"metrics"`
			Statement string   `json:"statement"`
			Root      struct {
				Op string `json:"op"`
			} `json:"root"`
		} `json:"doc"`
	}
	env := e.api("GET", "/api/v1/ws/"+id+"/plan", "", 200)
	if err := json.Unmarshal(env.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Doc.Statement != q || out.Doc.Headline == "" || len(out.Doc.Metrics) == 0 || out.Doc.Root.Op == "" || !out.Again {
		t.Fatalf("plan = %+v", out)
	}
	if st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200)); !st.HasPlan {
		t.Error("the reattach state should say there is a plan")
	}

	// explain again: the plan's own statement, whatever the editor holds,
	// and the old plan becomes the "before"
	ex, logs = e.explainAndWait(id, s, explainBody("SELECT 1", false, true))
	if !ex.OK || !strings.Contains(strings.Join(logs, "\n"), strings.TrimSpace(q[:30])) {
		t.Fatalf("again = %+v, logs %q", ex, logs)
	}
	tb, _ := e.srv.hub.get(id)
	if ps := tb.planState(); ps.prev == nil || ps.plan.Statement != q || ps.seq != 2 {
		t.Errorf("after again: prev %v, statement %q, seq %d", ps.prev != nil, ps.plan.Statement, ps.seq)
	}
}

// Copying the plan: the text tree under its statement, and the engine's
// own output.
func TestPlanText(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.api("GET", "/api/v1/ws/"+id+"/plan/text", "", 400) // nothing yet
	e.explainAndWait(id, s, explainBody("SELECT * FROM cats WHERE id = 3", false, false))
	txt := decodeData[map[string]string](t, e.api("GET", "/api/v1/ws/"+id+"/plan/text?what=text&metric=nope", "", 200))
	if !strings.HasPrefix(txt["text"], "SELECT * FROM cats WHERE id = 3\n\n") || txt["what"] != "the plan" {
		t.Errorf("text = %q (%s)", txt["text"], txt["what"])
	}
	raw := decodeData[map[string]string](t, e.api("GET", "/api/v1/ws/"+id+"/plan/text?what=raw", "", 200))
	if raw["text"] == "" || raw["what"] != "the engine's plan output" {
		t.Errorf("raw = %+v", raw)
	}
}

// A plan the user ran as an EXPLAIN is recognized in the result and opens
// the Plan tab, as in the TUI; the raw rows stay the result.
func TestTypedExplainOpensThePlan(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody("EXPLAIN QUERY PLAN SELECT * FROM cats WHERE name = 'Leo'", 0, false), 200)
	ev, logs := s.await(t, "run")
	run := decodeData[runEvent](t, testEnvelope{Data: ev.Data})
	if !run.OK || !run.HasPlan || !run.HasResult {
		t.Fatalf("run = %+v", run)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "that result is a query plan") {
		t.Errorf("logs = %q", logs)
	}
	var out planOut
	if err := json.Unmarshal(e.api("GET", "/api/v1/ws/"+id+"/plan", "", 200).Data, &out); err != nil || len(out.Doc) == 0 {
		t.Fatalf("plan = %+v (%v)", out, err)
	}
}

// Refusals come back in the TUI's words; a stop is a stop.
func TestExplainRefusals(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	env := e.api("POST", "/api/v1/ws/"+id+"/explain", explainBody("  ", false, false), 400)
	if !strings.Contains(env.Error, "nothing to explain") {
		t.Errorf("error = %q", env.Error)
	}
	b, _ := json.Marshal(explainReq{runReq: runReq{Buffer: "SELECT 1; SELECT 2", Selection: "SELECT 1; SELECT 2"}})
	env = e.api("POST", "/api/v1/ws/"+id+"/explain", string(b), 400)
	if !strings.Contains(env.Error, "select one to explain") {
		t.Errorf("error = %q", env.Error)
	}
	// "again" with no plan yet explains the editor's statement instead
	if ex, _ := e.explainAndWait(id, s, explainBody("SELECT 1", false, true)); !ex.OK {
		t.Errorf("again without a plan = %+v", ex)
	}
	// busy: an explain while a run is in flight is a 409
	slow := "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n) SELECT count(*) FROM n"
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody(slow, 0, false), 200)
	s.await(t, "busy")
	e.api("POST", "/api/v1/ws/"+id+"/explain", explainBody("SELECT 1", false, false), 409)
	e.api("POST", "/api/v1/ws/"+id+"/cancel", "", 200)
	s.await(t, "run")
}

// The standalone page, served: the very page `dbc explain --open` writes,
// with a CSP that runs its one inline script by hash and nothing else.
func TestPlanPage(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	res := e.req("GET", "/api/v1/ws/"+id+"/plan.html", "", nil)
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Errorf("no plan yet = %d, want 404", res.StatusCode)
	}
	e.explainAndWait(id, s, explainBody("SELECT * FROM cats", false, false))
	res = e.req("GET", "/api/v1/ws/"+id+"/plan.html", "", nil)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	csp := res.Header.Get("Content-Security-Policy")
	if res.StatusCode != 200 || !strings.Contains(csp, "script-src "+explain.ScriptHash()+";") || strings.Contains(csp, "'self'") {
		t.Fatalf("page = %d, CSP %q", res.StatusCode, csp)
	}
	if !strings.Contains(string(b), "DbcPlan.mount(") || !strings.Contains(string(b), `id="plan-data"`) {
		t.Errorf("not the standalone page: %.200s", b)
	}
	res = e.req("GET", "/api/v1/ws/"+id+"/plan.html?download=1", "", nil)
	res.Body.Close()
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="plan-demo-sqlite-`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

// The Plan tab loads the same script the standalone page inlines.
func TestPlanAssetsAreTheSharedOnes(t *testing.T) {
	e := newTestEnv(t)
	for path, want := range map[string]string{"/static/js/plan.js": explain.PlanJS, "/static/css/plan.css": explain.PlanCSS} {
		res := e.req("GET", path+"?v=1", "", map[string]string{})
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || string(b) != want {
			t.Errorf("GET %s = %d, %d bytes (want the explain package's %d)", path, res.StatusCode, len(b), len(want))
		}
	}
	res := e.req("GET", "/", "", nil)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, ref := range []string{"/static/js/plan.js?v=", "/static/css/plan.css?v=", "/static/js/planview.js?v="} {
		if !strings.Contains(string(b), ref) {
			t.Errorf("the workbench does not load %s", ref)
		}
	}
}
