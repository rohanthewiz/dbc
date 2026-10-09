package jobs

import (
	"errors"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/pipeline"
)

// CheckJob's rules, one case each: the diag's where and a piece of its
// message.
func TestCheckJob(t *testing.T) {
	pipes := map[string]string{
		"p":   `{"name": "p", "fragments": [{"name": "f", "nodes": [{"id": "n", "plugin": "sql.exec", "cfg": {"conn": "a", "sql": "SELECT 1"}}]}]}`,
		"req": `{"name": "req", "params": {"day": {"default": ""}}, "fragments": [{"name": "f", "nodes": [{"id": "n", "plugin": "sql.exec", "cfg": {"conn": "a", "sql": "SELECT '${day}'"}}]}]}`,
		"bad": `{"name": "bad", "fragments": [{"name": "f", "nodes": [{"id": "n", "plugin": "no.such"}]}]}`,
	}
	find := func(name string) (*pipeline.Spec, error) {
		text, ok := pipes[strings.TrimSuffix(name, ".json")]
		if !ok {
			return nil, errors.New("no such pipeline")
		}
		return pipeline.Parse(text)
	}
	ok := `{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}, {"id": "b", "pipeline": "p.json", "after": ["a"]}]}`
	if d := CheckJob(job(t, ok), CheckOptions{Find: find}); d != nil {
		t.Fatalf("clean job: %v", d)
	}
	cases := []struct{ text, where, msg string }{
		{`{"name": "a b", "pipelines": [{"id": "a", "pipeline": "p"}]}`, "name", "not a job name"},
		{`{"name": "j", "pipelines": []}`, "pipelines", "at least one"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}, {"id": "a", "pipeline": "p", "after": ["a"]}]}`, "a", "two steps"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}, {"id": "b", "pipeline": "p", "after": ["x"]}]}`, "b.after", `no step called "x"`},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}, {"id": "b", "pipeline": "p"}]}`, "root", "a job has one root"},
		{`{"name": "j", "root": "b", "pipelines": [{"id": "a", "pipeline": "p"}, {"id": "b", "pipeline": "p", "after": ["a"]}]}`, "root", `root is "b"`},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}, {"id": "b", "pipeline": "p", "after": ["a", "c"]}, {"id": "c", "pipeline": "p", "after": ["b"]}]}`,
			"pipelines", "in a cycle: b, c"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "zz"}]}`, "a", "pipeline zz: no such pipeline"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "bad"}]}`, "a", "pipeline bad: f/n"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "req"}]}`, "a.params", `needs "day"`},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p", "params": {"x": "1"}}]}`, "a.params.x", `has no parameter "x"`},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "req", "params": {"day": "${when}"}}]}`, "a.params.day", "${when} is not a parameter"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "req", "params": {"day": "${run.dat}"}}]}`, "a.params.day", "${run.dat}"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}], "triggers": {"schedule": ["0 25 * * *"]}}`, "triggers.schedule[0]", "hour: 25"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}], "triggers": {"schedule": ["0 0 30 2 *"]}}`, "triggers.schedule[0]", "never fires"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}], "triggers": {"tz": "Mars/Olympus"}}`, "triggers.tz", "unknown time zone"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}], "policy": {"on_failure": "panic"}}`, "policy.on_failure", "finish_branches"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}], "policy": {"overlap": "both"}}`, "policy.overlap", `"skip" or "queue"`},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}], "policy": {"max_parallel": -1}}`, "policy.max_parallel", "at least 1"},
		{`{"name": "j", "pipelines": [{"id": "a", "pipeline": "p"}], "policy": {"timeout": "soon"}}`, "policy.timeout", "positive duration"},
	}
	for _, c := range cases {
		diags := CheckJob(job(t, c.text), CheckOptions{Find: find})
		found := false
		for _, d := range diags {
			if d.Where == c.where && strings.Contains(d.Msg, c.msg) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s\n  want %s: …%s…, got %v", c.text, c.where, c.msg, diags)
		}
	}
	// a typo'd key is a parse error, not a silent drop
	if _, err := ParseJob(`{"name": "j", "pipeline": []}`); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("unknown key: %v", err)
	}
	// the root may be left out when one step has no after
	if r := job(t, ok).RootID(); r != "a" {
		t.Errorf("root = %q", r)
	}
}

// A half-built job still lays out: an after naming no step or the step
// itself is ignored, a step named twice keeps its first place, and a cycle
// is cut rather than refused. A run lays out from its record.
func TestLayoutSteps(t *testing.T) {
	l := LayoutSteps([]string{"a", "b", "c", "b"}, [][]string{nil, {"a", "nope", "b"}, {"b"}, {"c"}})
	if len(l.Nodes) != 3 || l.Card != [2]float64{cardW, cardH} {
		t.Fatalf("layout = %+v", l)
	}
	if !(l.Nodes["a"][0] < l.Nodes["b"][0] && l.Nodes["b"][0] < l.Nodes["c"][0]) {
		t.Errorf("a chain not left to right: %+v", l.Nodes)
	}
	if l.W < 3*cardW || l.H < cardH {
		t.Errorf("size %vx%v", l.W, l.H)
	}
	cyc := LayoutSteps([]string{"x", "y"}, [][]string{{"y"}, {"x"}})
	if len(cyc.Nodes) != 2 || cyc.Nodes["x"] == cyc.Nodes["y"] {
		t.Errorf("cycle = %+v", cyc.Nodes)
	}
	if empty := LayoutSteps(nil, nil); len(empty.Nodes) != 0 || empty.W != 0 {
		t.Errorf("empty = %+v", empty)
	}
	r := &Run{Pipelines: []PipelineRun{{ID: "a"}, {ID: "b", After: []string{"a"}}}}
	if rl := r.Layout(); rl.Nodes["a"][0] >= rl.Nodes["b"][0] {
		t.Errorf("run layout = %+v", rl.Nodes)
	}
}

// Spec.JSON puts arrays of scalars on one line, as a pipeline's, and
// reads back the same job.
func TestSpecJSONCompact(t *testing.T) {
	s := &Spec{Name: "j", Pipelines: []Step{{ID: "a", Pipeline: "p"}, {ID: "b", Pipeline: "p", After: []string{"a"}}},
		Triggers: Triggers{Schedule: []string{"0 2 * * *", "30 4 * * 1"}}}
	text, err := s.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `"after": ["a"]`) || !strings.Contains(text, `"schedule": ["0 2 * * *", "30 4 * * 1"]`) {
		t.Errorf("not compact:\n%s", text)
	}
	back, err := ParseJob(text)
	if err != nil || back.Pipelines[1].After[0] != "a" || len(back.Triggers.Schedule) != 2 {
		t.Errorf("round trip: %v %+v", err, back)
	}
}
