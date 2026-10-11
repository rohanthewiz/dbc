package pgdump

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/pgdocker"
)

// pg_dump FROM DOCKER (N-170). pg_dump refuses a server newer than itself,
// and a machine often has none new enough: Homebrew's postgresql@16 beside
// a "Postgres in Docker…" 18. dbc already drives the docker CLI
// (pgdocker), so when Locate finds no pg_dump for the server, and Docker
// answers, the tools run in the server's own image instead —
// postgres:<server major>, whose pg_dump always matches:
//
//	docker run --rm -i --user UID:GID [--network host]
//	  -e PGSERVICEFILE -e PGPASSWORD          (values from the CLI's env, not argv)
//	  -v <tmp>:<tmp>:ro                       the service file (Do)
//	  -v <out dir>:<out dir>                  where the dump is written
//	  -v <tls file>:<tls file>:ro …           sslrootcert, sslcert, … as named
//	  -w <cwd>  postgres:18  pg_dump <the same args as a local pg_dump>
//
// Every path is mounted at the path it has here, so the arguments a local
// run would get — --file=OUT.partial, a split's archive and list files —
// mean the same inside. --user keeps what is written the user's. The
// container is the run's: --rm takes it away, and docker run passes the
// interrupt that stops a dump on to pg_dump (sig-proxy).
//
// THE NETWORK. Inside a container 127.0.0.1 is the container: a server on
// this machine's loopback (a dbc Postgres in Docker, a Homebrew server) is
// reached through --network host on Linux, where that shares the host's
// loopback, and through host.docker.internal elsewhere (Docker Desktop,
// colima), which the service file is rewritten to (dockerConn). A server
// elsewhere is reached as it is. A Unix-socket connection cannot be, and
// is refused. Windows is left out: its paths do not mount at themselves.

// dockerHost is the name a container reaches the Docker host by, outside
// Linux's host networking.
const dockerHost = "host.docker.internal"

// hostNetwork reports whether containers share the host's network
// (--network host): Linux only, where a loopback server is reached as is.
var hostNetwork = runtime.GOOS == "linux"

// LocateOrDocker is Locate, falling back on the server's image in Docker
// when no local tools are new enough (see pg_dump FROM DOCKER). Only when
// the tools were looked for (no binDir named: one given is meant) and the
// server's major is known; Locate's own error stands when Docker cannot
// help, so it is the message the user sees.
func LocateOrDocker(ctx context.Context, binDir string, serverMajor int, needRestore bool) (Tools, error) {
	t, err := Locate(ctx, binDir, serverMajor, needRestore)
	if err == nil || binDir != "" || serverMajor <= 0 || runtime.GOOS == "windows" {
		return t, err
	}
	bin, ok := dockerAvailable(ctx)
	if !ok {
		return t, err
	}
	img := pgdocker.Image(strconv.Itoa(serverMajor))
	return Tools{Dump: "pg_dump", Restore: "pg_restore", Major: serverMajor,
		Version: fmt.Sprintf("%d (in Docker, %s)", serverMajor, img), Image: img, Docker: bin}, nil
}

// dockerAvailable finds a docker CLI whose daemon answers; a var, so tests
// can say yes or no without one.
var dockerAvailable = func(ctx context.Context) (string, bool) {
	bin, err := pgdocker.FindBin()
	if err != nil {
		return "", false
	}
	d := &pgdocker.Docker{Bin: bin}
	return bin, d.Check(ctx) == nil
}

// dockerConn is the connection as a container must be told it: a loopback
// host is the Docker host's (dockerHost) unless the container shares the
// host's network, and libpq's default root certificate, which the
// container's $HOME does not have, is named by its path here so it is
// mounted. An error for a Unix socket, which no container can reach.
func dockerConn(lc db.LibpqConn) (db.LibpqConn, error) {
	out := db.LibpqConn{Password: lc.Password, Dropped: lc.Dropped}
	// no host (and no hostaddr) is libpq's default socket; a host that is a
	// path, or an abstract socket's @name, is one too
	hosts, named := lc.Get("host")
	_, byAddr := lc.Get("hostaddr")
	socket := !named && !byAddr
	for _, h := range strings.Split(hosts, ",") {
		socket = socket || named && (h == "" && !byAddr || strings.HasPrefix(h, "/") || strings.HasPrefix(h, "@"))
	}
	if socket {
		return db.LibpqConn{}, serr.New("this connection is a Unix socket, which pg_dump in a container cannot reach — " +
			"connect over TCP (host=127.0.0.1), or install client tools new enough for the server")
	}
	loop := func(h string) bool {
		return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
	}
	for _, kv := range lc.Settings {
		switch k, v := kv[0], kv[1]; {
		case hostNetwork:
		case k == "host":
			hs := strings.Split(v, ",")
			for i, h := range hs {
				if loop(h) {
					hs[i] = dockerHost
				}
			}
			kv[1] = strings.Join(hs, ",")
		case k == "hostaddr" && slices.ContainsFunc(strings.Split(v, ","), loop):
			continue // the host's name (rewritten above) is reached instead
		}
		out.Settings = append(out.Settings, kv)
	}
	if mode, _ := out.Get("sslmode"); (mode == "verify-ca" || mode == "verify-full") && homeRootCertPath() != "" {
		if _, named := out.Get("sslrootcert"); !named {
			out.Settings = append(out.Settings, [2]string{"sslrootcert", homeRootCertPath()})
		}
	}
	return out, nil
}

// homeRootCertPath is libpq's default root certificate,
// ~/.postgresql/root.crt, when it exists; "".
func homeRootCertPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".postgresql", "root.crt")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// fileSettings are the libpq settings that name a file of this machine,
// which a container is given by mounting it.
var fileSettings = []string{"sslcert", "sslkey", "sslrootcert", "sslcrl", "sslcrldir", "passfile"}

// dockerArgs is the `docker run` that runs tool with args in the image:
// mounts for the service file's directory (svcDir), where the output goes,
// and each file the connection names. env is the tools' environment, whose
// PGSERVICEFILE and PGPASSWORD are passed by name only.
func (r *Run) dockerArgs(tool string, args []string, svcDir string) []string {
	a := []string{"run", "--rm", "-i"}
	if runtime.GOOS != "windows" {
		a = append(a, "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()))
	}
	if hostNetwork {
		a = append(a, "--network", "host")
	}
	a = append(a, "-e", "PGSERVICEFILE")
	if r.Conn.Password != "" {
		a = append(a, "-e", "PGPASSWORD")
	}
	mounts := map[string]bool{}
	mount := func(p string, ro bool) {
		if p == "" || mounts[p] {
			return
		}
		mounts[p] = true
		spec := p + ":" + p
		if ro {
			spec += ":ro"
		}
		a = append(a, "-v", spec)
	}
	if svcDir != "" {
		mount(svcDir, true)
	}
	cwd, _ := os.Getwd()
	if r.Opts.Out != "" {
		out := r.Opts.Out
		if !filepath.IsAbs(out) {
			out = filepath.Join(cwd, out)
		}
		if r.Opts.Format == Split {
			mount(out, false) // the archive, the lists and the parts are all in it
		} else {
			mount(filepath.Dir(out), false) // OUT.partial, renamed to OUT beside it
		}
	}
	for _, k := range fileSettings {
		if v, ok := r.Conn.Get(k); ok && filepath.IsAbs(v) {
			mount(v, true)
		}
	}
	if rc := homeRootCertPath(); rc != "" {
		if v, _ := r.Conn.Get("sslrootcert"); v == rc {
			mount(rc, true)
		}
	}
	if cwd != "" {
		a = append(a, "-w", cwd)
	}
	a = append(a, r.Tools.Image, tool)
	return append(a, args...)
}

// command is the exec.Cmd that runs tool with args: the tool itself, or
// docker running it in the image (Tools.Image).
func (r *Run) command(ctx context.Context, tool string, args []string) *exec.Cmd {
	if r.Tools.Image == "" {
		return exec.CommandContext(ctx, tool, args...)
	}
	return exec.CommandContext(ctx, r.Tools.Docker, r.dockerArgs(tool, args, r.svcDir)...)
}

// pullImage fetches the tools' image unless Docker has it, saying so first:
// the first dump of a version downloads it (about 150 MB).
func (r *Run) pullImage(ctx context.Context) error {
	if err := exec.CommandContext(ctx, r.Tools.Docker, "image", "inspect", "--format", "{{.Id}}", r.Tools.Image).Run(); err == nil {
		return nil
	}
	r.say("pulling %s for its pg_dump — the first dump of a version downloads it (about 150 MB)", r.Tools.Image)
	out, err := exec.CommandContext(ctx, r.Tools.Docker, "pull", "--quiet", r.Tools.Image).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return serr.New("docker could not pull "+r.Tools.Image+": "+strings.TrimSpace(string(out)), "image", r.Tools.Image)
	}
	return nil
}
