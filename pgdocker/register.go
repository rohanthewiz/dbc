package pgdocker

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/connedit"
	"github.com/rohanthewiz/dbc/db"
)

// New is a Docker that checks readiness the way the connection form's Test
// does — db.Probe: open, ping, close — so "ready" means dbc can connect,
// not merely that the port is open. progress may be nil.
func New(progress func(string)) *Docker {
	return &Docker{Progress: progress, Ping: func(ctx context.Context, dsn string) error {
		_, err := db.Probe(ctx, config.Connection{Name: "postgres in docker", Driver: "postgres", DSN: dsn}, 5*time.Second)
		return err
	}}
}

// Registered says what Register did with the connection.
type Registered struct {
	Name     string
	Added    bool // a new connection; false: an existing one kept or updated
	Updated  bool // an existing one's DSN was replaced (its container was recreated)
	Warnings []string
}

// Register makes the connection for a started server: ConnName(major)
// ("docker-pg18"), saved like any connection added in the form, so it
// survives restarts and is edited or removed the same way.
//
//	no connection by that name ─────────────► Add
//	one added in dbc, same DSN ─────────────► nothing to do (a second pick)
//	one added in dbc, other DSN ────────────► Edit its DSN: the container
//	                                          was recreated (new port and
//	                                          password); its ai_rows stay
//	one from the config file ───────────────► Add as docker-pg18-2, -3, …
//	                                          (the file's is the user's)
//
// inUse is the UI's in-use rule (connedit.Editor.Edit): a DSN change under
// a session is refused with its words, and the user disconnects first.
func Register(ed *connedit.Editor, res Result, inUse func(string) error) (Registered, error) {
	base := ConnName(res.Major)
	name := base
	for i := 2; ; i++ {
		cn, exists := ed.Cfg.ConnByName(name)
		if !exists || cn.Base != "" {
			// free — or only resolvable as another connection's database
			// ("docker-pg18" never is: it has no separator), so free too
			break
		}
		if cn.Web {
			return update(ed, name, res, inUse)
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	warns, err := ed.Add(connedit.Form{Name: name, Driver: "postgres", DSN: res.DSN})
	if err != nil {
		return Registered{}, err
	}
	return Registered{Name: name, Added: true, Warnings: warns}, nil
}

// update points the saved connection name at res's server, unless it
// already does. The DSN is compared as saved — as typed, ${VAR}s and all —
// which for a connection Register made is the DSN it wrote.
func update(ed *connedit.Editor, name string, res Result, inUse func(string) error) (Registered, error) {
	sc, found, err := ed.Saved.Get(name)
	if err != nil {
		return Registered{}, err
	}
	if found && sc.DSN == res.DSN {
		return Registered{Name: name}, nil
	}
	f := connedit.Form{Name: name, Driver: "postgres", DSN: res.DSN}
	if found {
		f.AIRows = sc.AIRows
	}
	er, err := ed.Edit(name, f, inUse)
	if err != nil {
		return Registered{}, err
	}
	return Registered{Name: name, Updated: true, Warnings: er.Warnings}, nil
}

// connRE is the names Register gives: ConnName(major), and the "-2", "-3" …
// it falls back to when the config file has that name.
var connRE = regexp.MustCompile(`^docker-pg(\d+)(?:-\d+)?$`)

// MajorOf is the PostgreSQL major of a connection Register made, from its
// name: "docker-pg18" and "docker-pg18-2" are 18's. ok is false for any
// other name, or a major dbc does not offer.
//
// The name is the link, not the DSN's port: a menu asks for every row it
// draws, without a docker call, and a stop then checks the container itself
// (Docker.Stop), so a name that only looks like one costs a clear refusal.
func MajorOf(conn string) (major string, ok bool) {
	m := connRE.FindStringSubmatch(conn)
	if m == nil || !Known(m[1]) {
		return "", false
	}
	return m[1], true
}

// StopTarget is the major whose container stopping connection conn means:
// one Register made (MajorOf) and still a connection added in dbc — a
// config-file connection under such a name is the user's own, whatever it
// points at.
func StopTarget(ed *connedit.Editor, conn string) (major string, err error) {
	major, ok := MajorOf(conn)
	cn, known := ed.Cfg.ConnByName(conn)
	switch {
	case !known:
		return "", &connedit.Error{Kind: connedit.NotFound, Msg: fmt.Sprintf("no connection named %q", conn)}
	case !ok || !cn.Web || cn.Base != "":
		return "", &connedit.Error{Kind: connedit.Invalid,
			Msg: fmt.Sprintf("%q is not a connection dbc made for a Postgres in Docker container", conn)}
	}
	return major, nil
}
