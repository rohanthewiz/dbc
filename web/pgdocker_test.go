package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/pgdocker"
)

// fakePGDocker swaps in a Docker whose CLI is canned: daemon is version's
// error (nil: running), ps the containers listed, and no container exists
// yet, so a start creates one. The server "answers" at once.
func fakePGDocker(t *testing.T, daemon error, ps string) { fakePGDockerWith(t, daemon, ps, "") }

// fakePGDockerWith is fakePGDocker with `docker container inspect`
// answering inspect ("" : no such container).
func fakePGDockerWith(t *testing.T, daemon error, ps, inspect string) {
	t.Helper()
	prev := newPGDocker
	newPGDocker = func(progress func(string)) *pgdocker.Docker {
		return &pgdocker.Docker{Progress: progress,
			Ping: func(context.Context, string) error { return nil },
			Run: func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "version":
					return "29.0.0", daemon
				case "ps":
					return ps, nil
				case "container":
					if inspect != "" {
						return inspect, nil
					}
					return "", errors.New("Error: No such container: " + args[2])
				}
				return "", nil
			}}
	}
	t.Cleanup(func() { newPGDocker = prev })
}

// pgDockerResp is GET and POST /api/v1/pgdocker.
type pgDockerResp struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Versions []struct {
		Major, EOL, Status string
	} `json:"versions"`
	connListResp
	Name      string   `json:"name"`
	Added     bool     `json:"added"`
	Container string   `json:"container"`
	Port      int      `json:"port"`
	Created   bool     `json:"created"`
	Steps     []string `json:"steps"`
}

// The dialog's dropdown lists the supported versions with their containers'
// states; a start adds docker-pgNN, saved, for the page to connect to.
func TestPGDockerListAndStart(t *testing.T) {
	e := newTestEnv(t)
	fakePGDocker(t, nil, "18\trunning\t127.0.0.1:5518->5432/tcp\n")

	r := decodeData[pgDockerResp](t, e.api("GET", "/api/v1/pgdocker", "", 200))
	if !r.OK || len(r.Versions) == 0 || r.Versions[0].Major != "18" || r.Versions[0].Status != "running on :5518" {
		t.Fatalf("list: %+v", r)
	}

	r = decodeData[pgDockerResp](t, e.api("POST", "/api/v1/pgdocker", `{"major":"17"}`, 200))
	if !r.OK || r.Name != "docker-pg17" || !r.Added || !r.Created || r.Container != "dbc-pg17" || r.Port == 0 {
		t.Fatalf("start: %+v", r)
	}
	if c, ok := r.find("docker-pg17"); !ok || !c.Saved || c.Driver != "postgres" {
		t.Fatalf("connection in the list: %+v, %v", c, ok)
	}
	if len(r.Steps) == 0 || !strings.Contains(strings.Join(r.Steps, "\n"), "creating container dbc-pg17") {
		t.Errorf("steps %q", r.Steps)
	}
	if _, found, _ := e.srv.saved.Get("docker-pg17"); !found {
		t.Error("not kept in the saved-connections file")
	}

	// a version dbc does not offer is the request's fault
	e.api("POST", "/api/v1/pgdocker", `{"major":"9.6"}`, 400)
}

// Docker down is an answer, not a server fault: ok false, docker's words.
func TestPGDockerDown(t *testing.T) {
	e := newTestEnv(t)
	fakePGDocker(t, errors.New("Cannot connect to the Docker daemon"), "")
	r := decodeData[pgDockerResp](t, e.api("GET", "/api/v1/pgdocker", "", 200))
	if r.OK || !strings.Contains(r.Error, "not running") {
		t.Fatalf("list: %+v", r)
	}
	r = decodeData[pgDockerResp](t, e.api("POST", "/api/v1/pgdocker", `{"major":"17"}`, 200))
	if r.OK || !strings.Contains(r.Error, "not running") {
		t.Fatalf("start: %+v", r)
	}
}

// Stop answers for a connection Postgres in Docker made, and refuses any
// other.
func TestPGDockerStop(t *testing.T) {
	e := newTestEnv(t)
	fakePGDocker(t, nil, "")
	e.api("POST", "/api/v1/pgdocker", `{"major":"17"}`, 200)

	// the container, running and dbc's
	fakePGDockerWith(t, nil, "", `[{"State":{"Running":true},"Config":{"Labels":{"dbc.pgdocker":"17"}}}]`)
	type stopResp struct {
		OK         bool   `json:"ok"`
		Container  string `json:"container"`
		WasRunning bool   `json:"was_running"`
		Error      string `json:"error"`
	}
	r := decodeData[stopResp](t, e.api("POST", "/api/v1/pgdocker/stop", `{"conn":"docker-pg17"}`, 200))
	if !r.OK || !r.WasRunning || r.Container != "dbc-pg17" {
		t.Fatalf("stop: %+v", r)
	}

	// not one of its connections: the request's fault; gone: a 404
	e.api("POST", "/api/v1/pgdocker/stop", `{"conn":"demo-sqlite"}`, 400)
	e.api("POST", "/api/v1/pgdocker/stop", `{"conn":"docker-pg18"}`, 404)
}
