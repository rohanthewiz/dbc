package tui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/pgdocker"
)

// fakePGDocker swaps in a Docker whose CLI is canned: daemon is version's
// error (nil: running), ps the containers listed, and no container exists
// yet, so a start creates one. The server "answers" at once. It returns
// the docker commands run.
func fakePGDocker(t *testing.T, daemon error, ps string) *[][]string {
	t.Helper()
	var calls [][]string
	prev := newPGDocker
	newPGDocker = func(progress func(string)) *pgdocker.Docker {
		return &pgdocker.Docker{Progress: progress,
			Ping: func(context.Context, string) error { return nil },
			Run: func(_ context.Context, args ...string) (string, error) {
				calls = append(calls, args)
				switch args[0] {
				case "version":
					return "29.0.0", daemon
				case "ps":
					return ps, nil
				case "container":
					return "", errors.New("Error: No such container: " + args[2])
				}
				return "", nil // image inspect: the image is there; run: ok
			}}
	}
	t.Cleanup(func() { newPGDocker = prev })
	return &calls
}

// The connections menu's "Postgres in Docker…" opens the version menu —
// each supported major, with its container's state — and a pick starts
// that version, adds docker-pgNN, and connects to it.
func TestPGDockerStartsAndAddsTheConnection(t *testing.T) {
	m := newTestModel(t)
	m.saved = config.OpenSaved(filepath.Join(t.TempDir(), "connections.toml"))
	calls := fakePGDocker(t, nil, "16\texited\t\n")

	x, y := connRowAt(t, m, m.ws.Active())
	rightClick(t, m, x, y)
	pickMenu(t, m, "Postgres in Docker")
	if m.menu == nil {
		t.Fatalf("no version menu; log:\n%s", logText(m))
	}
	newest := pgdocker.Supported(time.Now())[0].Major
	if it := menuRow(t, m, "PostgreSQL "+newest+" (newest)"); it.why != "" {
		t.Fatalf("newest row disabled: %q", it.why)
	}
	if it := menuRow(t, m, "PostgreSQL 16"); it.key != "stopped" {
		t.Fatalf("16's state = %q, want stopped", it.key)
	}

	pickMenu(t, m, "PostgreSQL 17")
	log := logText(m)
	for _, want := range []string{"starting PostgreSQL 17 in Docker", "creating container dbc-pg17",
		"PostgreSQL 17 started in container dbc-pg17", "added connection docker-pg17", "docker stop dbc-pg17"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if m.pgDockerBusy != "" {
		t.Errorf("still busy with %q", m.pgDockerBusy)
	}
	cn, ok := m.cfg.ConnByName("docker-pg17")
	if !ok || cn.Driver != "postgres" || !strings.Contains(cn.DSN, "@127.0.0.1:") {
		t.Fatalf("connection %+v, %v", cn, ok)
	}
	if _, found, _ := m.saved.Get("docker-pg17"); !found {
		t.Error("not kept in connections.toml")
	}
	if !strings.Contains(m.ws.Active()+" "+connecting(m), "docker-pg17") {
		t.Errorf("not switched to docker-pg17 (active %q)", m.ws.Active())
	}
	ran := false
	for _, c := range *calls {
		ran = ran || c[0] == "run"
	}
	if !ran {
		t.Errorf("no docker run: %v", *calls)
	}
}

// Docker not running says so in the log, and no version menu opens.
func TestPGDockerSaysWhenDockerIsDown(t *testing.T) {
	m := newTestModel(t)
	fakePGDocker(t, errors.New("Cannot connect to the Docker daemon"), "")
	x, y := connRowAt(t, m, m.ws.Active())
	rightClick(t, m, x, y)
	pickMenu(t, m, "Postgres in Docker")
	if m.menu != nil {
		t.Fatal("a version menu opened without Docker")
	}
	if log := logText(m); !strings.Contains(log, "docker is installed but not running") {
		t.Fatalf("log:\n%s", log)
	}
}

// While a start is out, the versions stay listed but say why they wait.
func TestPGDockerOneStartAtATime(t *testing.T) {
	m := newTestModel(t)
	m.pgDockerBusy = "18"
	m.pgDockerMenu(pgDockerListMsg{x: 1, y: 1})
	if why := menuRow(t, m, "PostgreSQL 17").why; !strings.Contains(why, "PostgreSQL 18 is still starting") {
		t.Fatalf("why %q", why)
	}
}

// connecting is the connection the workspace is still dialing, if any.
func connecting(m *Model) string {
	name, _ := m.ws.Connecting()
	return name
}

// A connection Postgres in Docker made has a Stop row: dim, saying why,
// while dbc is on it; once off it, it stops the container and drops the
// pool. Other connections have no such row.
func TestPGDockerStopFromTheMenu(t *testing.T) {
	m := newTestModel(t)
	m.saved = config.OpenSaved(filepath.Join(t.TempDir(), "connections.toml"))
	calls := fakePGDocker(t, nil, "")
	demo := m.ws.Active()
	// the Stop row goes by the name a Register gives (pgdocker.MajorOf);
	// over a SQLite file, so dbc can really be on it
	if _, err := m.connEditor().Add(connFormFor("docker-pg17", filepath.Join(t.TempDir(), "pg.db"))); err != nil {
		t.Fatal(err)
	}
	m.refreshConns()

	// on it: refused
	drive(t, m, nil, m.setActive("docker-pg17"))
	x, y := connRowAt(t, m, "docker-pg17")
	rightClick(t, m, x, y)
	if why := menuRow(t, m, "■ Stop container dbc-pg17").why; !strings.Contains(why, "docker-pg17") {
		t.Fatalf("stop while on it: why %q", why)
	}
	m.menu = nil

	// off it: stops
	drive(t, m, nil, m.setActive(demo))
	x, y = connRowAt(t, m, "docker-pg17")
	rightClick(t, m, x, y)
	*calls = nil
	pickMenu(t, m, "■ Stop container dbc-pg17")
	// the fake has no container, so Stop says it is gone, having looked
	// first: never a docker stop of a container it has not seen
	if log := logText(m); !strings.Contains(log, "stopping container dbc-pg17") ||
		!strings.Contains(log, "there is no container dbc-pg17") {
		t.Fatalf("log:\n%s", log)
	}
	for _, c := range *calls {
		if c[0] == "stop" {
			t.Fatalf("docker stop run on a container that is not there: %v", *calls)
		}
	}

	// another connection: no Stop row
	x, y = connRowAt(t, m, demo)
	rightClick(t, m, x, y)
	for _, it := range m.menu.items {
		if strings.Contains(it.label, "Stop container") {
			t.Fatalf("Stop on %s: %q", demo, it.label)
		}
	}
}
