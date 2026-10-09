package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/dbc/userdata"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"":                 {},
		"7d":               now.AddDate(0, 0, -7),
		"36h":              now.Add(-36 * time.Hour),
		"90m":              now.Add(-90 * time.Minute),
		"2026-10-01":       time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
		"2026-10-01 08:30": time.Date(2026, 10, 1, 8, 30, 0, 0, time.Local),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q = %s %v, want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "-3d", "7 days"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// `dbc runs --sql`: the records land as four tables in a scratch bytdb,
// one level of the drilldown each, and a statement over them answers.
func TestRunsSQLTables(t *testing.T) {
	runs := t.TempDir()
	started := time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	rec := jobs.Run{ID: "20261009-020000-0a0a", Kind: jobs.KindJob, Name: "nightly", Trigger: jobs.TriggerSchedule,
		Status: pipeline.Succeeded, Started: started, Ended: started.Add(3 * time.Second),
		Pipelines: []jobs.PipelineRun{{ID: "copy", RunStats: pipeline.RunStats{Pipeline: "copy-cats", Status: pipeline.Succeeded,
			Started: started, Ended: started.Add(time.Second),
			Fragments: []pipeline.FragmentStats{{Name: "cats", Status: pipeline.Succeeded, Rows: 8, Direct: true,
				Started: started, Ended: started.Add(time.Second),
				Nodes: []pipeline.NodeStats{{ID: "src", Plugin: "sql.table", Out: 8}, {ID: "dst", Plugin: "sql.write", In: 8, Out: 8}}}}}}},
	}
	bs, _ := json.Marshal(rec)
	if err := userdata.SaveRun(runs, jobs.KindJob, "nightly", rec.ID, bs); err != nil {
		t.Fatal(err)
	}
	e := jobs.New(&config.Config{}, nil, jobs.Options{RunsDir: runs})
	heads, err := e.History(userdata.RunFilter{})
	if err != nil || len(heads) != 1 {
		t.Fatal(heads, err)
	}
	cfg := &config.Config{MaxRows: 100, Connections: []config.Connection{
		{Name: "q", Driver: "bytdb", DSN: filepath.Join(t.TempDir(), "runs.bytdb")}}}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	got, err := loadAndQuery(mgr, "q", e, heads, sqlsplit.Split(
		`SELECT r.name, p.step, f.fragment, f.rows, f.direct, n.node, n.rows_out
		 FROM runs r JOIN pipelines p ON p.run_id = r.id JOIN fragments f ON f.run_id = r.id AND f.step = p.step
		 JOIN nodes n ON n.run_id = r.id AND n.fragment = f.fragment WHERE n.node = 'dst';
		 SELECT seconds FROM runs`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[0].Rows) != 1 || got[1].Rows[0][0] != "3" {
		t.Fatalf("results = %+v / %+v", got[0].Rows, got[1].Rows)
	}
	want := []string{"nightly", "copy", "cats", "8", "true", "dst", "8"}
	for i, w := range want {
		if got[0].Rows[0][i] != w {
			t.Errorf("column %s = %q, want %q", got[0].Columns[i], got[0].Rows[0][i], w)
		}
	}
}

// dbc run cancel's client: the Bearer secret on every call, dbc web's
// envelope unwrapped — the data on success, the server's words and the
// status on a refusal, status 0 when nothing answers.
func TestWebClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success": false, "error": "sign in"}`))
			return
		}
		if r.URL.Path == "/api/v1/runs/x/cancel" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"success": false, "error": "run x is running in another process"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success": true, "data": {"run": "y"}}`))
	}))
	defer srv.Close()
	c := &webClient{base: srv.URL, secret: "s3cret", http: srv.Client()}
	data, status, err := c.call(context.Background(), http.MethodPost, "/api/v1/runs/y/cancel")
	if err != nil || status != 200 || string(data) != `{"run": "y"}` {
		t.Errorf("ok call = %s %d %v", data, status, err)
	}
	_, status, err = c.call(context.Background(), http.MethodPost, "/api/v1/runs/x/cancel")
	if status != http.StatusConflict || err == nil || !strings.Contains(err.Error(), "another process") {
		t.Errorf("refused call = %d %v", status, err)
	}
	c.secret = "wrong"
	if _, status, _ = c.call(context.Background(), http.MethodGet, "/"); status != http.StatusUnauthorized {
		t.Errorf("bad secret = %d", status)
	}
	srv.Close()
	if _, status, err = c.call(context.Background(), http.MethodGet, "/"); status != 0 || err == nil {
		t.Errorf("nothing there = %d %v", status, err)
	}
}
