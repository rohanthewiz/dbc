package pgdocker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/connedit"
)

// fakeDocker stands in for the docker CLI: it records each command and
// answers from canned replies keyed by the command's first word(s).
type fakeDocker struct {
	calls   [][]string
	inspect string // `container inspect`'s JSON; "" answers "No such container"
	image   bool   // `image inspect` finds the image
	runErr  error  // `run` fails with this
	daemon  error  // `version` fails with this
	ps      string
}

func (f *fakeDocker) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "version":
		return "29.0.0\n", f.daemon
	case "ps":
		return f.ps, nil
	case "container":
		if f.inspect == "" {
			return "", errors.New("Error: No such container: " + args[2])
		}
		return f.inspect, nil
	case "image":
		if !f.image {
			return "", errors.New("Error: No such image: " + args[len(args)-1])
		}
		return "sha256:abc\n", nil
	case "run":
		return "c0ffee\n", f.runErr
	}
	return "", nil
}

// did reports whether a command starting with words was run.
func (f *fakeDocker) did(words ...string) []string {
	for _, c := range f.calls {
		if len(c) >= len(words) && slices.Equal(c[:len(words)], words) {
			return c
		}
	}
	return nil
}

func newDocker(f *fakeDocker, pingErrs ...error) (*Docker, *[]string) {
	var said []string
	d := &Docker{Run: f.run, ReadyTimeout: 2 * time.Second, Progress: func(s string) { said = append(said, s) }}
	// the server refuses the first len(pingErrs) pings, then answers
	d.Ping = func(context.Context, string) error {
		if len(pingErrs) > 0 {
			err := pingErrs[0]
			pingErrs = pingErrs[1:]
			return err
		}
		return nil
	}
	return d, &said
}

func TestSupportedDropsVersionsPastEOL(t *testing.T) {
	majors := func(vs []Version) (out []string) {
		for _, v := range vs {
			out = append(out, v.Major)
		}
		return out
	}
	if got := majors(Supported(date(2026, 10, 8))); !slices.Equal(got, []string{"18", "17", "16", "15", "14"}) {
		t.Errorf("Supported(2026-10-08) = %v", got)
	}
	if got := majors(Supported(date(2026, 11, 12))); !slices.Equal(got, []string{"18", "17", "16", "15"}) {
		t.Errorf("Supported(14's EOL day) = %v: 14 should be gone", got)
	}
	// a clock past every EOL still offers the newest rather than nothing
	if got := majors(Supported(date(2040, 1, 1))); !slices.Equal(got, []string{"18"}) {
		t.Errorf("Supported(2040) = %v", got)
	}
}

func TestNamesPortsAndDataDir(t *testing.T) {
	if ContainerName("18") != "dbc-pg18" || VolumeName("18") != "dbc-pg18-data" || ConnName("18") != "docker-pg18" ||
		Image("18") != "postgres:18" || PreferredPort("18") != 5518 {
		t.Error("a name or port for 18 changed — existing containers and connections would be orphaned")
	}
	if dataDir("18") != "/var/lib/postgresql" || dataDir("17") != "/var/lib/postgresql/data" {
		t.Errorf("dataDir: 18 %q, 17 %q", dataDir("18"), dataDir("17"))
	}
}

func TestStatusesParsesPs(t *testing.T) {
	f := &fakeDocker{ps: "18\trunning\t127.0.0.1:5518->5432/tcp\n16\texited\t\n\trunning\t\n"}
	d, _ := newDocker(f)
	sts, err := d.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st := sts["18"]; !st.Running || st.Port != 5518 || st.Text() != "running on :5518" {
		t.Errorf("18: %+v %q", st, st.Text())
	}
	if st := sts["16"]; st.Running || !st.Exists || st.Text() != "stopped" {
		t.Errorf("16: %+v %q", st, st.Text())
	}
	if len(sts) != 2 || sts["17"].Text() != "" {
		t.Errorf("statuses = %+v: an unlabelled row or a version with no container leaked in", sts)
	}
	if c := f.did("ps"); !slices.Contains(c, "label="+Label) {
		t.Errorf("ps not filtered to dbc's containers: %v", c)
	}
}

func TestCheckSaysDockerIsNotRunning(t *testing.T) {
	d, _ := newDocker(&fakeDocker{daemon: errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")})
	err := d.Check(context.Background())
	if !errors.Is(err, ErrNotRunning) || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Check = %v, want ErrNotRunning", err)
	}
}

func TestStartCreatesPullsAndWaits(t *testing.T) {
	f := &fakeDocker{}
	d, said := newDocker(f, errors.New("connection reset"), errors.New("connection reset"))
	res, err := d.Start(context.Background(), "17")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !res.Pulled || res.Port == 0 || len(res.Password) != 32 {
		t.Errorf("result %+v", res)
	}
	if want := fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", res.Password, res.Port); res.DSN != want {
		t.Errorf("DSN %q, want %q", res.DSN, want)
	}
	if f.did("pull", "--quiet", "postgres:17") == nil {
		t.Error("the missing image was not pulled")
	}
	run := strings.Join(f.did("run"), " ")
	for _, want := range []string{"--name dbc-pg17", "--label dbc.pgdocker=17", "POSTGRES_PASSWORD=" + res.Password,
		fmt.Sprintf("127.0.0.1:%d:5432", res.Port), "dbc-pg17-data:/var/lib/postgresql/data", "postgres:17"} {
		if !strings.Contains(run, want) {
			t.Errorf("docker run %q lacks %q", run, want)
		}
	}
	if len(*said) != 3 || !strings.HasPrefix((*said)[0], "pulling") {
		t.Errorf("progress %q: want pull, create, wait", *said)
	}
}

func TestStartRemovesAContainerThatFailedToRun(t *testing.T) {
	f := &fakeDocker{image: true, runErr: errors.New("port is already allocated")}
	d, _ := newDocker(f)
	if _, err := d.Start(context.Background(), "16"); err == nil || !strings.Contains(err.Error(), "already allocated") {
		t.Fatalf("Start = %v", err)
	}
	if f.did("pull") != nil {
		t.Error("pulled an image Docker already had")
	}
	if f.did("rm", "--force", "dbc-pg16") == nil {
		t.Error("the half-made container was left behind")
	}
}

// inspectJSON is `docker container inspect` for a container of dbc's (or,
// with label "", someone else's).
func inspectJSON(running bool, label, port, password string) string {
	return fmt.Sprintf(`[{"State":{"Running":%t},"Config":{"Env":["PATH=/usr/bin","POSTGRES_PASSWORD=%s"],
		"Labels":{"dbc.pgdocker":%q}},"HostConfig":{"PortBindings":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":%q}]}}}]`,
		running, password, label, port)
}

func TestStartReusesDbcsContainer(t *testing.T) {
	f := &fakeDocker{inspect: inspectJSON(false, "18", "5518", "s3cret")}
	d, _ := newDocker(f)
	res, err := d.Start(context.Background(), "18")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || res.Port != 5518 || res.Password != "s3cret" {
		t.Errorf("result %+v: want the container's own port and password", res)
	}
	if f.did("start", "dbc-pg18") == nil || f.did("run") != nil {
		t.Errorf("a stopped container should be started, not recreated: %v", f.calls)
	}

	// running already: nothing to start
	f = &fakeDocker{inspect: inspectJSON(true, "18", "5518", "s3cret")}
	d, _ = newDocker(f)
	if _, err = d.Start(context.Background(), "18"); err != nil || f.did("start") != nil {
		t.Errorf("Start = %v, calls %v", err, f.calls)
	}
}

func TestStartRefusesWhatIsNotDbcs(t *testing.T) {
	f := &fakeDocker{inspect: inspectJSON(true, "", "5518", "x")}
	d, _ := newDocker(f)
	if _, err := d.Start(context.Background(), "18"); err == nil || !strings.Contains(err.Error(), "did not create") {
		t.Fatalf("Start on someone else's container = %v", err)
	}
	if _, err := d.Start(context.Background(), "9.6"); err == nil || !strings.Contains(err.Error(), "not a version") {
		t.Fatalf("Start(9.6) = %v", err)
	}
}

func TestStartGivesUpWhenTheServerNeverAnswers(t *testing.T) {
	f := &fakeDocker{inspect: inspectJSON(true, "15", "5515", "pw")}
	d, _ := newDocker(f)
	d.ReadyTimeout = 600 * time.Millisecond
	d.Ping = func(context.Context, string) error { return errors.New("the database system is starting up") }
	_, err := d.Start(context.Background(), "15")
	if err == nil || !strings.Contains(err.Error(), "docker logs dbc-pg15") || !strings.Contains(err.Error(), "starting up") {
		t.Fatalf("Start = %v", err)
	}
}

// newEditor is a connedit.Editor over a config holding one file
// connection, and a saved-connections file in a temp dir.
func newEditor(t *testing.T, file ...config.Connection) *connedit.Editor {
	t.Helper()
	dir := t.TempDir()
	return &connedit.Editor{Cfg: &config.Config{Connections: file},
		Saved: config.OpenSaved(filepath.Join(dir, "connections.toml")), App: "dbc"}
}

func TestRegisterAddsKeepsAndUpdates(t *testing.T) {
	ed := newEditor(t)
	res := Result{Major: "18", Port: 5518, Password: "pw1", DSN: DSN(5518, "pw1")}

	reg, err := Register(ed, res, nil)
	if err != nil || reg.Name != "docker-pg18" || !reg.Added {
		t.Fatalf("first Register = %+v, %v", reg, err)
	}
	if cn, ok := ed.Cfg.ConnByName("docker-pg18"); !ok || cn.DSN != res.DSN || cn.Driver != "postgres" || !cn.Web {
		t.Fatalf("connection %+v", cn)
	}

	// a second pick of a running container: nothing changes
	reg, err = Register(ed, res, func(string) error { return errors.New("in use") })
	if err != nil || reg.Added || reg.Updated {
		t.Fatalf("same DSN = %+v, %v", reg, err)
	}

	// the container was recreated: the DSN follows, and ai_rows stays
	sc, _, _ := ed.Saved.Get("docker-pg18")
	sc.AIRows = true
	if err = ed.Saved.Update("docker-pg18", sc); err != nil {
		t.Fatal(err)
	}
	res2 := Result{Major: "18", Port: 5519, Password: "pw2", DSN: DSN(5519, "pw2")}
	if _, err = Register(ed, res2, func(string) error { return errors.New("tab 1 is on it") }); err == nil ||
		!strings.Contains(err.Error(), "tab 1") {
		t.Fatalf("a DSN change under a session = %v, want the in-use refusal", err)
	}
	reg, err = Register(ed, res2, nil)
	if err != nil || !reg.Updated {
		t.Fatalf("new DSN = %+v, %v", reg, err)
	}
	if sc, _, _ = ed.Saved.Get("docker-pg18"); sc.DSN != res2.DSN || !sc.AIRows {
		t.Errorf("saved %+v", sc)
	}
}

func TestRegisterLeavesTheConfigFilesConnectionAlone(t *testing.T) {
	ed := newEditor(t, config.Connection{Name: "docker-pg17", Driver: "postgres", DSN: "postgres://elsewhere/db"})
	reg, err := Register(ed, Result{Major: "17", DSN: DSN(5517, "pw")}, nil)
	if err != nil || reg.Name != "docker-pg17-2" || !reg.Added {
		t.Fatalf("Register = %+v, %v", reg, err)
	}
	if cn, _ := ed.Cfg.ConnByName("docker-pg17"); cn.DSN != "postgres://elsewhere/db" {
		t.Errorf("the file's connection was changed: %+v", cn)
	}
}

func TestStopOnlyDbcsRunningContainer(t *testing.T) {
	f := &fakeDocker{inspect: inspectJSON(true, "18", "5518", "pw")}
	d, _ := newDocker(f)
	if was, err := d.Stop(context.Background(), "18"); err != nil || !was || f.did("stop", "dbc-pg18") == nil {
		t.Fatalf("Stop = %v, %v; calls %v", was, err, f.calls)
	}

	// already stopped: no error, nothing run
	f = &fakeDocker{inspect: inspectJSON(false, "18", "5518", "pw")}
	d, _ = newDocker(f)
	if was, err := d.Stop(context.Background(), "18"); err != nil || was || f.did("stop") != nil {
		t.Fatalf("Stop of a stopped one = %v, %v; calls %v", was, err, f.calls)
	}

	// gone, or not dbc's: said, and never stopped
	for _, c := range []struct{ inspect, want string }{
		{"", "there is no container dbc-pg18"},
		{inspectJSON(true, "", "5518", "pw"), "not created by dbc"},
	} {
		f = &fakeDocker{inspect: c.inspect}
		d, _ = newDocker(f)
		if _, err := d.Stop(context.Background(), "18"); err == nil || !strings.Contains(err.Error(), c.want) || f.did("stop") != nil {
			t.Errorf("Stop = %v, want %q; calls %v", err, c.want, f.calls)
		}
	}
}

func TestMajorOfAndStopTarget(t *testing.T) {
	for name, want := range map[string]string{"docker-pg18": "18", "docker-pg14-2": "14",
		"docker-pg9": "", "docker-pg18x": "", "prod": "", "docker-pg18/other": ""} {
		if got, ok := MajorOf(name); got != want || ok != (want != "") {
			t.Errorf("MajorOf(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	ed := newEditor(t, config.Connection{Name: "docker-pg16", Driver: "postgres", DSN: "postgres://mine/db"})
	if _, err := Register(ed, Result{Major: "17", DSN: DSN(5517, "pw")}, nil); err != nil {
		t.Fatal(err)
	}
	if major, err := StopTarget(ed, "docker-pg17"); err != nil || major != "17" {
		t.Errorf("StopTarget(docker-pg17) = %q, %v", major, err)
	}
	// the config file's connection by that name is the user's own
	if _, err := StopTarget(ed, "docker-pg16"); err == nil || !strings.Contains(err.Error(), "not a connection dbc made") {
		t.Errorf("StopTarget(file's docker-pg16) = %v", err)
	}
	if _, err := StopTarget(ed, "docker-pg15"); err == nil || !strings.Contains(err.Error(), "no connection") {
		t.Errorf("StopTarget(missing) = %v", err)
	}
}
