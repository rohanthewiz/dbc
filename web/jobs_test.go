package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/userdata"
)

// jobEnv is a test server with fresh pipelines, jobs and runs directories,
// two pipelines on the test connection (demo-sqlite) — "quick", and
// "slow", which waits until stopped — and its dirs.
type jobDirs struct{ pipelines, jobs, runs string }

func jobEnv(t *testing.T) (*testEnv, jobDirs) {
	t.Helper()
	root := t.TempDir()
	d := jobDirs{filepath.Join(root, "pipelines"), filepath.Join(root, "jobs"), filepath.Join(root, "runs")}
	e := newTestEnv(t, func(c *config.Config, _ *Options) {
		c.PipelinesDir, c.JobsDir, c.RunsDir = d.pipelines, d.jobs, d.runs
	})
	for name, text := range map[string]string{
		"quick.json": `{"name": "quick", "fragments": [{"name": "f", "nodes": [
		  {"id": "x", "plugin": "sql.exec", "cfg": {"conn": "demo-sqlite", "sql": "SELECT 1"}}]}]}`,
		"slow.json": `{"name": "slow", "fragments": [{"name": "f", "nodes": [
		  {"id": "z", "plugin": "go.action", "cfg": {"code": "func Run(s *sdb.S) error {\n\t<-s.Ctx().Done()\n\treturn s.Ctx().Err()\n}\n"}}]}]}`,
	} {
		if _, _, err := userdata.SavePipeline(d.pipelines, name, text, ""); err != nil {
			t.Fatal(err)
		}
	}
	return e, d
}

func (e *testEnv) putJob(name, text, base string, want int) scriptSaved {
	e.t.Helper()
	b, _ := json.Marshal(scriptSave{Text: text, Base: base})
	env := e.api("PUT", "/api/v1/jobs/"+name+"?win=w1", string(b), want)
	if want != 200 {
		return scriptSaved{}
	}
	return decodeData[scriptSaved](e.t, env)
}

// The jobs store: the pipelines' protocol, told as "jobs"; the list shows
// a saved job's next fire once the scheduler has read it, and the
// examples.
func TestJobStore(t *testing.T) {
	e, _ := jobEnv(t)
	_, s := e.open()
	text := `{"name": "two", "pipelines": [{"id": "a", "pipeline": "quick"}, {"id": "b", "pipeline": "quick.json", "after": ["a"]}],
	  "triggers": {"schedule": ["0 2 * * *"]}}`
	e.api("GET", "/api/v1/jobs/two.json", "", 404)
	r1 := e.putJob("two.json", text, "", 200)
	if ev := awaitJob[scriptsEvent](t, s, "jobs"); ev.Op != "saved" || ev.Name != "two.json" {
		t.Errorf("save event = %+v", ev)
	}
	e.putJob("two.json", text, "", 409) // create never overwrites
	if got := e.putJob("two.json", "{}", "stale", 200); !got.Conflict || got.Text != text {
		t.Errorf("stale save = %+v", got)
	}
	e.putJob("two.json", "not json", r1.Rev, 400)

	// the scheduler reads the directory again on the save: poll for it
	var list jobsList
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		list = decodeData[jobsList](t, e.api("GET", "/api/v1/jobs", "", 200))
		if len(list.Jobs) == 1 && list.Jobs[0].Next != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("list = %+v", list)
		}
	}
	if list.Jobs[0].Pipelines != 2 || list.Jobs[0].Next.Local().Hour() != 2 {
		t.Errorf("row = %+v", list.Jobs[0])
	}
	if len(list.Examples) == 0 || list.Examples[0].Name != "nightly.json" || list.Examples[0].Next != nil {
		t.Errorf("examples = %+v (an example is never scheduled)", list.Examples)
	}
	e.api("POST", "/api/v1/jobs/two.json/rename?win=w1", `{"to": "three.json"}`, 200)
	trashed := decodeData[map[string]string](t, e.api("DELETE", "/api/v1/jobs/three.json?win=w1", "", 200))
	e.api("POST", "/api/v1/job-trash/"+trashed["id"]+"/restore", `{}`, 200)
	e.api("GET", "/api/v1/jobs/three.json", "", 200)
	if ex := decodeData[map[string]string](t, e.api("GET", "/api/v1/job-examples/nightly.json", "", 200)); !strings.Contains(ex["text"], `"root": "copy"`) {
		t.Errorf("example = %q", ex["text"])
	}
}

// The check: a job's text, its pipelines found as a run would, and each
// cron line's next five fires — or why it has none.
func TestJobCheckRoute(t *testing.T) {
	e, _ := jobEnv(t)
	type checked struct {
		Diags []struct{ Where, Severity, Msg string } `json:"diags"`
		Fires []cronFires                             `json:"fires"`
	}
	got := decodeData[checked](t, e.api("POST", "/api/v1/job-check", `{"text": `+jsonString(
		`{"name": "c", "pipelines": [{"id": "a", "pipeline": "quick"}, {"id": "b", "pipeline": "nope", "after": ["a"]}],
		  "triggers": {"schedule": ["*/15 * * * *", "0 99 * * *"], "tz": "UTC"}}`)+`}`, 200))
	if len(got.Diags) != 2 || got.Diags[0].Where != "b" || !strings.Contains(got.Diags[0].Msg, "no such pipeline") ||
		got.Diags[1].Where != "triggers.schedule[1]" {
		t.Errorf("diags = %+v", got.Diags)
	}
	if len(got.Fires) != 2 || len(got.Fires[0].Next) != 5 || got.Fires[0].Next[0].Minute()%15 != 0 || got.Fires[1].Error == "" {
		t.Errorf("fires = %+v", got.Fires)
	}
	got = decodeData[checked](t, e.api("POST", "/api/v1/job-check", `{"text": "{\"name\": "}`, 200))
	if len(got.Diags) != 1 || got.Diags[0].Severity != "error" {
		t.Errorf("unparsable = %+v", got.Diags)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Running a job: the webhook (Bearer) only for a job that opens it, and
// recorded as such; by hand (the session cookie) for any; a second run of
// a running job is a 409; the record is read back, and listed in the
// history with its filters.
func TestJobRunAndWebhook(t *testing.T) {
	e, d := jobEnv(t)
	_, s := e.open()
	e.putJob("shut.json", `{"name": "shut", "pipelines": [{"id": "a", "pipeline": "quick"}]}`, "", 200)
	e.putJob("open.json", `{"name": "open", "pipelines": [{"id": "a", "pipeline": "quick"}, {"id": "b", "pipeline": "quick", "after": ["a"]}],
	  "triggers": {"webhook": true}}`, "", 200)
	if env := e.api("POST", "/api/v1/jobs/shut.json/run", "", 403); !strings.Contains(env.Error, `"webhook": true`) {
		t.Errorf("closed webhook = %q", env.Error)
	}
	e.api("POST", "/api/v1/jobs/none.json/run", "", 404)

	run := decodeData[runEnvelope](t, e.api("POST", "/api/v1/jobs/open.json/run", `{"params": {}}`, 200)).Run
	if run.Kind != jobs.KindJob || run.Trigger != jobs.TriggerWebhook || run.Source != "open.json" || len(run.Pipelines) != 2 {
		t.Fatalf("run = %+v", run)
	}
	// the window hears the run, its steps' states, and its end
	type stateEv struct {
		Run, Pipeline, Status string
	}
	st := awaitJob[stateEv](t, s, "job.state")
	if st.Run != run.ID || st.Pipeline != "a" || st.Status != "running" {
		t.Errorf("first state = %+v", st)
	}
	done := awaitJob[runEnvelope](t, s, "job.done").Run
	if done.ID != run.ID || done.Status != "succeeded" {
		t.Fatalf("done = %s %s %q", done.ID, done.Status, done.Error)
	}
	rec := decodeData[jobs.Run](t, e.api("GET", "/api/v1/runs/"+run.ID, "", 200))
	if rec.Status != "succeeded" || len(rec.Log) == 0 {
		t.Errorf("record = %s, %d lines", rec.Status, len(rec.Log))
	}
	if _, err := os.Stat(filepath.Join(d.runs, "job", "open", run.ID+".json")); err != nil {
		t.Errorf("no record on disk: %v", err)
	}

	// by hand, with the session cookie: trigger manual, no webhook needed
	login := e.req("GET", "/login?s="+testSecret, "", map[string]string{})
	login.Body.Close()
	var cookie string
	for _, c := range login.Cookies() {
		if strings.HasPrefix(c.Name, "dbc_session_") {
			cookie = c.Name + "=" + c.Value
		}
	}
	res := e.req("POST", "/api/v1/jobs/shut.json/run", "", map[string]string{"Cookie": cookie})
	var env testEnvelope
	_ = json.NewDecoder(res.Body).Decode(&env)
	res.Body.Close()
	manual := decodeData[runEnvelope](t, env).Run
	if res.StatusCode != 200 || manual.Trigger != jobs.TriggerManual {
		t.Fatalf("manual run = %d %+v", res.StatusCode, manual)
	}
	awaitJob[runEnvelope](t, s, "job.done")

	// a running job: a second start is a conflict; cancel stops it
	e.putJob("long.json", `{"name": "long", "pipelines": [{"id": "z", "pipeline": "slow"}], "triggers": {"webhook": true}}`, "", 200)
	long := decodeData[runEnvelope](t, e.api("POST", "/api/v1/jobs/long.json/run", "", 200)).Run
	if env := e.api("POST", "/api/v1/jobs/long.json/run", "", 409); !strings.Contains(env.Error, "overlap: skip") {
		t.Errorf("overlap = %q", env.Error)
	}
	e.api("POST", "/api/v1/runs/"+long.ID+"/cancel", "", 200)
	for {
		if done := awaitJob[runEnvelope](t, s, "job.done").Run; done.ID == long.ID {
			if done.Status != "canceled" {
				t.Errorf("long = %s", done.Status)
			}
			break
		}
	}

	type runsList struct {
		Runs []jobs.Summary `json:"runs"`
	}
	all := decodeData[runsList](t, e.api("GET", "/api/v1/runs?kind=job", "", 200))
	if len(all.Runs) != 3 || all.Runs[0].ID != long.ID {
		t.Errorf("history = %+v", all.Runs)
	}
	one := decodeData[runsList](t, e.api("GET", "/api/v1/runs?name=open&status=succeeded&since=1h", "", 200))
	if len(one.Runs) != 1 || one.Runs[0].ID != run.ID || one.Runs[0].Trigger != "webhook" {
		t.Errorf("filtered = %+v", one.Runs)
	}
	if plain := decodeData[map[string]json.RawMessage](t, e.api("GET", "/api/v1/runs", "", 200)); plain["runs"] != nil {
		t.Error("a catch-up without a filter got the history")
	}
	e.api("GET", "/api/v1/runs?since=yesterday", "", 400)
}
