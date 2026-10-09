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
