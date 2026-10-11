package pgdump

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/db"
)

// A local server, seen from a container: loopback becomes the Docker
// host's name, unless the container shares the host's network (Linux),
// and a hostaddr on loopback goes with it; a Unix socket is refused.
func TestDockerConn(t *testing.T) {
	saved := hostNetwork
	t.Cleanup(func() { hostNetwork = saved })
	lc := db.LibpqConn{Password: "pw", Settings: [][2]string{
		{"host", "localhost,db.example.com"}, {"hostaddr", "127.0.0.1"}, {"port", "5432"}, {"dbname", "app"}}}

	hostNetwork = false
	dc, err := dockerConn(lc)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := dc.Get("host"); h != dockerHost+",db.example.com" {
		t.Errorf("host = %q", h)
	}
	if _, ok := dc.Get("hostaddr"); ok || dc.Password != "pw" {
		t.Errorf("hostaddr kept or password lost: %+v", dc)
	}
	if h, _ := lc.Get("host"); h != "localhost,db.example.com" {
		t.Errorf("the original was changed: %q", h)
	}

	hostNetwork = true
	if dc, _ = dockerConn(lc); !slices.Equal(dc.Settings, lc.Settings) {
		t.Errorf("with host networking = %+v, want the settings as they are", dc.Settings)
	}

	for _, s := range [][][2]string{
		{{"host", "/var/run/postgresql"}},
		{{"dbname", "app"}}, // no host: libpq's default socket
	} {
		if _, err := dockerConn(db.LibpqConn{Settings: s}); err == nil || !strings.Contains(err.Error(), "Unix socket") {
			t.Errorf("%v: err = %v", s, err)
		}
	}
	if _, err := dockerConn(db.LibpqConn{Settings: [][2]string{{"hostaddr", "10.0.0.5"}}}); err != nil {
		t.Errorf("a hostaddr alone is TCP: %v", err)
	}
}

// The docker run: the image's tool with the arguments a local run gets,
// the paths it names mounted at themselves, the secrets by name only.
func TestDockerArgs(t *testing.T) {
	r := &Run{Tools: Tools{Image: "postgres:17", Docker: "docker", Dump: "pg_dump", Restore: "pg_restore", Major: 17},
		Conn: db.LibpqConn{Password: "pw", Settings: [][2]string{{"host", "h"}, {"sslrootcert", "/certs/ca.pem"}, {"sslcert", "rel.pem"}}},
		Opts: Options{Format: Plain, Out: "/dumps/x/out.sql"}}
	a := r.dockerArgs("pg_dump", []string{"--format=plain", "--file=/dumps/x/out.sql.partial"}, "/tmp/dbc-dump-1")
	line := strings.Join(a, " ")
	for _, want := range []string{
		"run --rm -i ", "-e PGSERVICEFILE", "-e PGPASSWORD",
		"-v /tmp/dbc-dump-1:/tmp/dbc-dump-1:ro", "-v /dumps/x:/dumps/x ", "-v /certs/ca.pem:/certs/ca.pem:ro",
		" postgres:17 pg_dump --format=plain --file=/dumps/x/out.sql.partial",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in: docker %s", want, line)
		}
	}
	if strings.Contains(line, "pw") || strings.Contains(line, "rel.pem") {
		t.Errorf("a secret or a relative file in argv: %s", line)
	}
	if runtime.GOOS == "linux" && !strings.Contains(line, "--network host") {
		t.Errorf("no host networking on Linux: %s", line)
	}
	// split: the output directory itself, where every part is written
	r.Opts = Options{Format: Split, Out: "/dumps/split"}
	if line = strings.Join(r.dockerArgs("pg_restore", nil, ""), " "); !strings.Contains(line, "-v /dumps/split:/dumps/split ") {
		t.Errorf("split: %s", line)
	}
	if !strings.Contains(line, "-e PGPASSWORD") {
		t.Errorf("split lost the password's -e: %s", line)
	}
	// the dry run shows the docker command
	r.Opts = Options{Format: Custom, Out: "/dumps/a.dump"}
	if c := r.Commands(); len(c) != 1 || !strings.HasPrefix(c[0], "docker run --rm -i") || !strings.Contains(c[0], "postgres:17 pg_dump --format=custom") {
		t.Errorf("commands = %q", c)
	}
	if n := r.RestoreNote(); !strings.Contains(n, "pg_restore 17 or newer") {
		t.Errorf("restore note = %q", n)
	}
}

// Docker is the last resort: when Locate finds nothing new enough, no
// tools directory was named, and Docker answers — the server's image.
func TestLocateOrDocker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Docker fallback on Windows")
	}
	saved := dockerAvailable
	t.Cleanup(func() { dockerAvailable = saved })
	ctx := context.Background()

	dockerAvailable = func(context.Context) (string, bool) { return "/x/docker", true }
	tools, err := LocateOrDocker(ctx, "", 99, true) // no pg_dump anywhere is 99
	if err != nil || tools.Image != "postgres:99" || tools.Docker != "/x/docker" || tools.Major != 99 || tools.Restore != "pg_restore" {
		t.Fatalf("tools = %+v, %v", tools, err)
	}
	if _, err := LocateOrDocker(ctx, t.TempDir(), 99, false); err == nil {
		t.Error("a named tools directory fell back on Docker")
	}
	dockerAvailable = func(context.Context) (string, bool) { return "", false }
	if _, err := LocateOrDocker(ctx, "", 99, false); err == nil || !strings.Contains(err.Error(), "99") {
		t.Errorf("no Docker: err = %v, want Locate's own", err)
	}
}
