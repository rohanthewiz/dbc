package pgdocker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// TestLiveDocker starts a real container, registers its connection and
// connects to it, stops it, starts it again (the reuse path) and removes it.
//
// Opt-in, like the db/live_* tests: DBC_LIVE_DOCKER=1, with Docker running.
// DBC_LIVE_DOCKER_PG picks the major (default: the newest supported); one
// whose image is already pulled runs in seconds. It never touches a
// container of that name it did not create — one already there skips the
// test — and removes its own container and volume at the end.
//
//	DBC_LIVE_DOCKER=1 DBC_LIVE_DOCKER_PG=17 go test -run LiveDocker -v ./pgdocker
func TestLiveDocker(t *testing.T) {
	if os.Getenv("DBC_LIVE_DOCKER") == "" {
		t.Skip("set DBC_LIVE_DOCKER=1 (with Docker running) to start a real container")
	}
	major := os.Getenv("DBC_LIVE_DOCKER_PG")
	if major == "" {
		major = Supported(time.Now())[0].Major
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	d := New(func(s string) { t.Log(s) })
	if err := d.Check(ctx); err != nil {
		t.Skip(err)
	}
	name := ContainerName(major)
	if _, found, err := d.inspect(ctx, name); err != nil || found {
		t.Skipf("container %s already exists (or inspect failed: %v) — not touching it", name, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = d.run(c, "rm", "--force", name)
		_, _ = d.run(c, "volume", "rm", VolumeName(major))
	})

	res, err := d.Start(ctx, major)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Errorf("first start did not create: %+v", res)
	}
	ed := newEditor(t)
	reg, err := Register(ed, res, nil)
	if err != nil || !reg.Added {
		t.Fatalf("Register = %+v, %v", reg, err)
	}
	cn, _ := ed.Cfg.ConnByName(reg.Name)
	pr, err := db.Probe(ctx, config.Connection{Name: cn.Name, Driver: cn.Driver, DSN: cn.DSN}, 10*time.Second)
	if err != nil || !pr.OK {
		t.Fatalf("connect over the registered DSN: %+v, %v", pr, err)
	}

	sts, err := d.Statuses(ctx)
	if err != nil || !sts[major].Running || sts[major].Port != res.Port {
		t.Errorf("Statuses = %+v, %v; want %s running on %d", sts, err, major, res.Port)
	}

	// stopped (Stop, as the menu does), and a second Stop is no error
	if was, err := d.Stop(ctx, major); err != nil || !was {
		t.Fatalf("Stop = %v, %v", was, err)
	}
	if was, err := d.Stop(ctx, major); err != nil || was {
		t.Fatalf("second Stop = %v, %v; want not running, no error", was, err)
	}
	// picked again: started, same port and password, and the connection
	// is left as it was
	again, err := d.Start(ctx, major)
	if err != nil || again.Created || again.DSN != res.DSN {
		t.Fatalf("restart = %+v, %v; want the same DSN %q", again, err, res.DSN)
	}
	if reg, err = Register(ed, again, nil); err != nil || reg.Added || reg.Updated {
		t.Errorf("re-Register = %+v, %v; want no change", reg, err)
	}
}
