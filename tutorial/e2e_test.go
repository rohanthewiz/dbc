package tutorial

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTutorialEndToEnd builds dbc and does the whole tutorial with it, as
// `dbc tutorial init` and then `dbc tutorial step N --run` for every lesson
// would: each lesson must end with every check passing. So a dbc change
// that breaks the tutorial fails here, not in front of a learner.
//
// It builds the binary and runs ~30 dbc processes, so -short skips it.
func TestTutorialEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds dbc and runs every lesson; skipped with -short")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH to build dbc with")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "dbc")
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Dir = ".." // the module root: package main
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// A HOME of its own keeps the run off the user's ~/.config/dbc (saved
	// connections, history), and CATS_* is dropped so no child talks to a
	// host Cats pane's control socket.
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CATS_") && !strings.HasPrefix(kv, "HOME=") {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+home)

	ws := filepath.Join(tmp, "ws")
	if err := Init(ws, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	e := &Env{Ctx: ctx, Dir: ws, Dbc: ExecRunner(bin, env)}

	for _, l := range Lessons() {
		var out bytes.Buffer
		if !Run(e, l, false, &out, &out) {
			t.Fatalf("lesson %d (%s) failed:\n%s", l.N, l.Title, &out)
		}
		// Run warns when an earlier lesson isn't done; in order, none is.
		if strings.Contains(out.String(), "isn't done yet") {
			t.Errorf("lesson %d: an earlier lesson reads as not done:\n%s", l.N, &out)
		}
	}

	// Every lesson passes at the end, too: a later lesson must not undo
	// an earlier one's state.
	var out bytes.Buffer
	if !Status(e, true, &out) {
		t.Errorf("not every lesson passes after the whole tutorial:\n%s", &out)
	}
	// And a second run of a done lesson runs nothing.
	out.Reset()
	l, _ := Find(0)
	if !Run(e, l, false, &out, &out) || !strings.Contains(out.String(), "done already") {
		t.Errorf("re-running lesson 0:\n%s", &out)
	}
}
