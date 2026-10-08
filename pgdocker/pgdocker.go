// Package pgdocker starts a PostgreSQL server in a local Docker container
// and sets up the dbc connection to it — the connections menu's "Postgres
// in Docker…" in the TUI and in dbc web.
//
// It is UI-free, like connedit: each UI asks Statuses to draw its version
// picker, Start to bring a container up, and Register to add (or refresh)
// the connection, so the two cannot disagree on a container's name, port,
// password or the connection it gets.
//
// One container per major version, named and labelled so a second pick of
// the same version finds it again rather than starting another:
//
//	pick 18 ─► Start
//	            │ docker inspect dbc-pg18
//	            ├─ none ──► image there? ─ no ─► docker pull postgres:18
//	            │           free port (5518, else any) + random password
//	            │           docker run -d --name dbc-pg18 --label dbc.pgdocker=18
//	            │             -p 127.0.0.1:PORT:5432 -v dbc-pg18-data:… postgres:18
//	            ├─ stopped ► docker start dbc-pg18
//	            └─ running ► (nothing to do)
//	            │ port and password read back from the container
//	            └► wait until the server answers on 127.0.0.1:PORT
//	         ─► Register: connection "docker-pg18" added, or its DSN updated
//
// The data lives in a named volume (dbc-pg18-data), so removing the
// container by hand keeps the databases, and a later pick reattaches them.
// The port is published on 127.0.0.1 only: the server is for this machine.
//
// The password is generated once, at creation, and read back from the
// container's environment (POSTGRES_PASSWORD) on every later start, so the
// container itself is where it is kept; the connection's DSN carries a
// copy, as every saved connection's DSN does.
//
// Docker is driven through its CLI rather than the Engine API: the CLI is
// what the user has installed and configured (contexts, colima, Docker
// Desktop's socket location), and it adds no dependency to dbc.
package pgdocker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// Version is a PostgreSQL major version the picker offers, with the date
// the PostgreSQL project stops supporting it
// (https://www.postgresql.org/support/versioning/).
type Version struct {
	Major string
	EOL   time.Time
}

// Versions are the majors with an official postgres image, newest first.
// Supported drops each one at its end of life, so a version leaves the
// picker on its own once it is past it; a new major is added here when it
// is released (not its betas: a throwaway server is still one people
// expect to behave like production).
var Versions = []Version{
	{"18", date(2030, 11, 14)},
	{"17", date(2029, 11, 8)},
	{"16", date(2028, 11, 9)},
	{"15", date(2027, 11, 11)},
	{"14", date(2026, 11, 12)},
}

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// Supported is Versions less those past their end of life at now. It is
// never empty: were every listed one past it (a dbc far older than its
// clock), the newest is still offered rather than a picker with nothing in
// it.
func Supported(now time.Time) []Version {
	var out []Version
	for _, v := range Versions {
		if now.Before(v.EOL) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = Versions[:1]
	}
	return out
}

// Known reports whether major is one of Versions.
func Known(major string) bool {
	for _, v := range Versions {
		if v.Major == major {
			return true
		}
	}
	return false
}

// Label marks the containers dbc started; its value is the major version.
const Label = "dbc.pgdocker"

// The names one major's container, volume and connection go by.
func ContainerName(major string) string { return "dbc-pg" + major }
func VolumeName(major string) string    { return "dbc-pg" + major + "-data" }
func ConnName(major string) string      { return "docker-pg" + major }
func Image(major string) string         { return "postgres:" + major }

// PreferredPort is the host port a new container asks for first: 5500 plus
// the major (5518 for 18). Off 5432, so a server already installed on the
// machine keeps its port, and one per major, so several versions run side
// by side; a taken one falls back to any free port (freePort).
func PreferredPort(major string) int {
	n, _ := strconv.Atoi(major)
	return 5500 + n
}

// dataDir is where the image keeps its data directory's volume. Images
// from 18 on keep a per-major directory under /var/lib/postgresql (PGDATA
// /var/lib/postgresql/18/docker) and refuse a volume at the old path; older
// ones mount /var/lib/postgresql/data itself.
func dataDir(major string) string {
	if n, _ := strconv.Atoi(major); n >= 18 {
		return "/var/lib/postgresql"
	}
	return "/var/lib/postgresql/data"
}

// Status is what the picker shows beside a version: whether dbc's
// container for it exists, runs, and on which host port.
type Status struct {
	Major   string
	Exists  bool
	Running bool
	Port    int // 0 while stopped: Docker lists no ports for a stopped container
}

// Text is a status in a few words, for a picker row ("" when there is no
// container yet).
func (s Status) Text() string {
	switch {
	case s.Running && s.Port > 0:
		return fmt.Sprintf("running on :%d", s.Port)
	case s.Running:
		return "running"
	case s.Exists:
		return "stopped"
	}
	return ""
}

// Result is a started (or found running) server.
type Result struct {
	Major     string
	Container string
	Port      int
	Password  string
	Created   bool // a new container; false: one already there was used
	Pulled    bool // the image was downloaded first
	DSN       string
}

// Docker runs the docker CLI. The zero value finds the binary itself; tests
// set Run (and Ping) to stand in for Docker and the server.
type Docker struct {
	// Bin is the docker binary; "" finds it (FindBin).
	Bin string
	// Run runs docker with args and returns its stdout; nil runs Bin. Its
	// error carries docker's stderr, which says what went wrong.
	Run func(ctx context.Context, args ...string) (string, error)
	// Ping reports whether a server answers on dsn; Start polls it until it
	// does. Required: the caller passes its own (db.Probe), which keeps
	// this package free of the drivers.
	Ping func(ctx context.Context, dsn string) error
	// ReadyTimeout bounds the wait for the server to answer; 0 means 90 s
	// — a first start initialises the data directory, which on a slow disk
	// or an emulated architecture takes a while.
	ReadyTimeout time.Duration
	// Progress, when set, is told each slow step as it starts (a pull, the
	// wait for the server), for a UI to show something while it waits.
	Progress func(string)
}

// ErrNoDocker means no docker binary was found; ErrNotRunning that one was,
// but its daemon did not answer. Both are the user's to fix, and the UIs
// show their messages as they are.
var (
	ErrNoDocker   = errors.New("docker was not found — install Docker Desktop (or colima) to run Postgres in a container")
	ErrNotRunning = errors.New("docker is installed but not running — start Docker Desktop (or colima) and try again")
)

// binDirs are where docker is installed when it is not on PATH. A GUI app
// (dbc's macOS app, launched from Finder) inherits launchd's short PATH,
// not the shell's, so LookPath alone would miss a docker that a terminal
// finds.
var binDirs = []string{
	"/usr/local/bin",
	"/opt/homebrew/bin",
	"~/.docker/bin",
	"/Applications/Docker.app/Contents/Resources/bin",
	"~/.rd/bin", // Rancher Desktop
}

// FindBin finds the docker binary: on PATH, else in binDirs.
func FindBin() (string, error) {
	if p, err := exec.LookPath("docker"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, d := range binDirs {
		if rest, ok := strings.CutPrefix(d, "~/"); ok {
			if home == "" {
				continue
			}
			d = filepath.Join(home, rest)
		}
		p := filepath.Join(d, "docker")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", ErrNoDocker
}

// run runs one docker command.
func (d *Docker) run(ctx context.Context, args ...string) (string, error) {
	if d.Run != nil {
		return d.Run(ctx, args...)
	}
	if d.Bin == "" {
		bin, err := FindBin()
		if err != nil {
			return "", err
		}
		d.Bin = bin
	}
	cmd := exec.CommandContext(ctx, d.Bin, args...)
	// docker's credential helpers (docker-credential-desktop) are found on
	// PATH; a binary found in binDirs means that PATH lacks its directory
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(d.Bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), serr.Wrap(errors.New(msg), "op", "docker "+args[0])
	}
	return out.String(), nil
}

// Check reports whether Docker can be used: a binary, and a daemon that
// answers. It is what a picker asks before offering to start anything.
func (d *Docker) Check(ctx context.Context) error {
	_, err := d.run(ctx, "version", "--format", "{{.Server.Version}}")
	if err == nil || errors.Is(err, ErrNoDocker) {
		return err
	}
	// the client ran but could not reach a server: "Cannot connect to the
	// Docker daemon", "error during connect", a missing socket. Usually
	// that is a stopped Docker, so that is what it says first; docker's own
	// first line follows, for when it is not (a docker context pointing at
	// a socket that is gone).
	words, _, _ := strings.Cut(err.Error(), "\n")
	return fmt.Errorf("%w (docker: %s)", ErrNotRunning, words)
}

// Statuses is the state of dbc's container for each major that has one,
// by major. A version not in the map has no container.
func (d *Docker) Statuses(ctx context.Context) (map[string]Status, error) {
	out, err := d.run(ctx, "ps", "-a", "--filter", "label="+Label,
		"--format", `{{.Label "`+Label+`"}}	{{.State}}	{{.Ports}}`)
	if err != nil {
		return nil, err
	}
	sts := map[string]Status{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		st := Status{Major: f[0], Exists: true, Running: f[1] == "running"}
		if len(f) > 2 {
			st.Port = portIn(f[2])
		}
		sts[st.Major] = st
	}
	return sts, nil
}

// portRE finds the host port mapped to the server's 5432 in docker ps's
// Ports column: "127.0.0.1:5518->5432/tcp".
var portRE = regexp.MustCompile(`:(\d+)->5432/tcp`)

func portIn(ports string) int {
	m := portRE.FindStringSubmatch(ports)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// container is the part of `docker inspect` Start reads back.
type container struct {
	State struct {
		Running bool
	}
	Config struct {
		Env    []string
		Labels map[string]string
	}
	HostConfig struct {
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string
		}
	}
}

// inspect reads the container named; found is false when there is none.
func (d *Docker) inspect(ctx context.Context, name string) (c container, found bool, err error) {
	out, err := d.run(ctx, "container", "inspect", name)
	if err != nil {
		// "No such container" is the answer, not a failure
		if strings.Contains(strings.ToLower(err.Error()), "no such") {
			return c, false, nil
		}
		return c, false, err
	}
	var cs []container
	if err = json.Unmarshal([]byte(out), &cs); err != nil || len(cs) == 0 {
		return c, false, serr.Wrap(fmt.Errorf("unreadable docker inspect output: %v", err), "container", name)
	}
	return cs[0], true, nil
}

// env is the value of variable k in the container's environment.
func (c container) env(k string) string {
	for _, kv := range c.Config.Env {
		if v, ok := strings.CutPrefix(kv, k+"="); ok {
			return v
		}
	}
	return ""
}

// hostPort is the host port the container's 5432 is published on (0: none).
// It is read from HostConfig, which holds the binding asked for at creation
// and so answers for a stopped container too.
func (c container) hostPort() int {
	for _, b := range c.HostConfig.PortBindings["5432/tcp"] {
		if n, _ := strconv.Atoi(b.HostPort); n > 0 {
			return n
		}
	}
	return 0
}

// Start brings up dbc's container for major — created, started, or found
// running — and waits until its server answers. See the package comment
// for the steps.
//
// A container under dbc's name that dbc did not label is refused rather
// than used: its password and port are not dbc's to guess, and starting
// someone's container is not what the user asked for.
func (d *Docker) Start(ctx context.Context, major string) (Result, error) {
	if !Known(major) {
		return Result{}, fmt.Errorf("PostgreSQL %q is not a version dbc offers (%s)", major, majors())
	}
	if d.Ping == nil {
		return Result{}, errors.New("pgdocker: Docker.Ping is not set")
	}
	if err := d.Check(ctx); err != nil {
		return Result{}, err
	}
	name := ContainerName(major)
	res := Result{Major: major, Container: name}

	c, found, err := d.inspect(ctx, name)
	if err != nil {
		return res, err
	}
	switch {
	case !found:
		if res.Pulled, err = d.ensureImage(ctx, major); err != nil {
			return res, err
		}
		if res.Port, res.Password, err = d.create(ctx, major); err != nil {
			return res, err
		}
		res.Created = true
	case c.Config.Labels[Label] == "":
		return res, fmt.Errorf("a container named %s exists that dbc did not create — "+
			"rename or remove it (docker rm %s) and try again", name, name)
	default:
		res.Port, res.Password = c.hostPort(), c.env("POSTGRES_PASSWORD")
		if res.Port == 0 || res.Password == "" {
			return res, fmt.Errorf("container %s has no published port or no POSTGRES_PASSWORD — "+
				"remove it (docker rm %s; its data volume %s stays) and try again", name, name, VolumeName(major))
		}
		if !c.State.Running {
			d.say("starting container " + name)
			if _, err = d.run(ctx, "start", name); err != nil {
				return res, err
			}
		}
	}
	res.DSN = DSN(res.Port, res.Password)
	return res, d.waitReady(ctx, res)
}

// Stop stops dbc's container for major. wasRunning is false when it was
// already stopped, which is no error: the user asked for it to be stopped,
// and it is. A container that is gone, or under dbc's name but not dbc's,
// is an error that says so — the latter is never stopped.
func (d *Docker) Stop(ctx context.Context, major string) (wasRunning bool, err error) {
	if err = d.Check(ctx); err != nil {
		return false, err
	}
	name := ContainerName(major)
	c, found, err := d.inspect(ctx, name)
	switch {
	case err != nil:
		return false, err
	case !found:
		return false, fmt.Errorf("there is no container %s — it was removed (its data volume %s may still be there)",
			name, VolumeName(major))
	case c.Config.Labels[Label] == "":
		return false, fmt.Errorf("container %s was not created by dbc — not stopping it", name)
	case !c.State.Running:
		return false, nil
	}
	d.say("stopping container " + name)
	if _, err = d.run(ctx, "stop", name); err != nil {
		return false, err
	}
	return true, nil
}

// majors lists Versions' majors, for a refusal.
func majors() string {
	var out []string
	for _, v := range Versions {
		out = append(out, v.Major)
	}
	return strings.Join(out, ", ")
}

// DSN is the connection string for a container's server: the image's
// superuser and default database, on loopback, without TLS (the image's
// server has no certificate). The password is hex (newPassword), so it
// needs no escaping in the URL.
func DSN(port int, password string) string {
	return fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", password, port)
}

func (d *Docker) say(s string) {
	if d.Progress != nil {
		d.Progress(s)
	}
}

// ensureImage pulls the version's image unless Docker has it already. The
// pull is said first: it is the one step that can take minutes.
func (d *Docker) ensureImage(ctx context.Context, major string) (pulled bool, err error) {
	img := Image(major)
	if _, err = d.run(ctx, "image", "inspect", "--format", "{{.Id}}", img); err == nil {
		return false, nil
	}
	d.say("pulling " + img + " — the first start of a version downloads its image (about 150 MB)")
	if _, err = d.run(ctx, "pull", "--quiet", img); err != nil {
		return false, err
	}
	return true, nil
}

// create runs a new container for major and returns its port and password.
// A run that fails after Docker created the container (the port taken in
// the moment since freePort looked) leaves it behind, created but never
// started; it is removed, so the next pick starts clean rather than
// finding a container with a port that never worked.
func (d *Docker) create(ctx context.Context, major string) (port int, password string, err error) {
	port, err = freePort(PreferredPort(major))
	if err != nil {
		return 0, "", err
	}
	password, err = newPassword()
	if err != nil {
		return 0, "", err
	}
	name := ContainerName(major)
	d.say(fmt.Sprintf("creating container %s on 127.0.0.1:%d", name, port))
	_, err = d.run(ctx, "run", "--detach",
		"--name", name,
		"--label", Label+"="+major,
		"--env", "POSTGRES_PASSWORD="+password,
		"--publish", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"--volume", VolumeName(major)+":"+dataDir(major),
		Image(major))
	if err != nil {
		_, _ = d.run(context.WithoutCancel(ctx), "rm", "--force", name)
		return 0, "", err
	}
	return port, password, nil
}

// freePort is want when nothing listens on it on loopback, else a port the
// OS picks. The listener is closed at once; Docker binds the port next.
func freePort(want int) (int, error) {
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", want)); err == nil {
		_ = l.Close()
		return want, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, serr.Wrap(err, "op", "find a free port")
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// newPassword is 16 random bytes in hex: strong, and safe in a URL as is.
func newPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", serr.Wrap(err, "op", "generate a password")
	}
	return hex.EncodeToString(b), nil
}

// waitReady polls the server until it answers, or ReadyTimeout passes.
//
// It connects over TCP, as dbc will, rather than asking pg_isready inside
// the container: a first start runs the image's init scripts on a server
// that listens on its unix socket only, then restarts it, so a TCP answer
// is the first sign the server is the one that stays. Docker's port
// forwarder accepts a connection before the server listens and then drops
// it, so a failure is simply retried.
func (d *Docker) waitReady(ctx context.Context, res Result) error {
	limit := d.ReadyTimeout
	if limit <= 0 {
		limit = 90 * time.Second
	}
	d.say("waiting for PostgreSQL " + res.Major + " to accept connections")
	deadline := time.Now().Add(limit)
	var last error
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		last = d.Ping(pctx, res.DSN)
		cancel()
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return serr.Wrap(fmt.Errorf("PostgreSQL %s in %s did not accept connections within %s "+
				"(docker logs %s says why): %s", res.Major, res.Container, limit, res.Container,
				last.Error()), "port", strconv.Itoa(res.Port))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
