package web

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/pgdump"
)

// dumpResp is POST /api/v1/dump's and GET's answer.
type dumpResp struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Out     string `json:"out"`
	Running *struct {
		Conn, Out, Format string
	} `json:"running"`
}

// dumpEvent is a "dump" event's data.
type dumpEvent struct {
	Level   string          `json:"level"`
	Text    string          `json:"text"`
	Running json.RawMessage `json:"running"`
}

// fakeDumpTool writes a shell-script pg_dump that writes "-- dumped" to
// its --file after sleeping secs, and swaps in a Prepare that hands back a
// Run using it — no server needed.
func fakeDumpTool(t *testing.T, secs string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake pg_dump is a shell script")
	}
	tool := filepath.Join(t.TempDir(), "pg_dump")
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --file=*) out=\"${a#--file=}\";; esac; done\n" +
		"echo '-- dumped' > \"$out\"\nsleep " + secs + "\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := prepareDump
	prepareDump = func(_ context.Context, _ *config.Config, _ *db.Manager, conn string, o pgdump.Options) (*pgdump.Run, error) {
		return &pgdump.Run{Name: conn, ServerMajor: 17, Tools: pgdump.Tools{Dump: tool, Version: "17.2"}, Opts: o}, nil
	}
	t.Cleanup(func() { prepareDump = prev })
}

// awaitDump reads "dump" events until one that says no dump is running,
// returning the log lines seen on the way.
func awaitDump(t *testing.T, s *stream) []string {
	t.Helper()
	var lines []string
	for {
		ev, _ := s.await(t, "dump")
		var d dumpEvent
		_ = json.Unmarshal(ev.Data, &d)
		if d.Text != "" {
			lines = append(lines, d.Level+": "+d.Text)
		}
		if string(d.Running) == "null" {
			return lines
		}
	}
}

// The dialog's suggestion, and what is refused before anything runs.
func TestDumpInfoAndRefusals(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	e := newTestEnv(t)
	r := decodeData[dumpResp](t, e.api("GET", "/api/v1/dump?conn=pg", "", 200))
	if !strings.HasPrefix(r.Out, filepath.Join(os.Getenv("HOME"), "pg-")) || !strings.HasSuffix(r.Out, ".sql") || r.Running != nil {
		t.Fatalf("info: %+v", r)
	}

	// not Postgres: an answer for the dialog, not a fault
	r = decodeData[dumpResp](t, e.api("POST", "/api/v1/dump",
		`{"conn":"demo-sqlite","format":"plain","out":"~/x.sql"}`, 200))
	if r.OK || !strings.Contains(r.Error, "Postgres") {
		t.Fatalf("sqlite: %+v", r)
	}
	r = decodeData[dumpResp](t, e.api("POST", "/api/v1/dump",
		`{"conn":"demo-sqlite","format":"directory","out":"~/d","jobs":"many"}`, 200))
	if r.OK || !strings.Contains(r.Error, "jobs") {
		t.Fatalf("jobs: %+v", r)
	}
	// a connection that does not exist is the request's fault
	e.api("POST", "/api/v1/dump", `{"conn":"nope","format":"plain","out":"~/x.sql"}`, 400)
	e.api("POST", "/api/v1/dump", `{"format":"plain"}`, 400)
}

// A dump runs in the background: the POST answers at once, every window
// hears its lines and its end, and the file is there after.
func TestDumpRunsInTheBackground(t *testing.T) {
	fakeDumpTool(t, "0")
	e := newTestEnv(t)
	_, s := e.open()
	out := filepath.Join(t.TempDir(), "app.sql")
	r := decodeData[dumpResp](t, e.api("POST", "/api/v1/dump",
		`{"conn":"demo-sqlite","format":"plain","out":"`+out+`"}`, 200))
	if !r.OK || r.Out != out {
		t.Fatalf("start: %+v", r)
	}
	lines := strings.Join(awaitDump(t, s), "\n")
	if !strings.Contains(lines, "info: dumping demo-sqlite (PostgreSQL 17) as plain to "+out) ||
		!strings.Contains(lines, "ok: dumped demo-sqlite to "+out) {
		t.Fatalf("lines:\n%s", lines)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "-- dumped\n" {
		t.Fatalf("out: %q %v", b, err)
	}
	if r = decodeData[dumpResp](t, e.api("GET", "/api/v1/dump", "", 200)); r.Running != nil {
		t.Fatalf("still running: %+v", r.Running)
	}
}

// One at a time; Stop interrupts the running one, and nothing of it stays.
func TestDumpOneAtATimeAndStop(t *testing.T) {
	fakeDumpTool(t, "30")
	e := newTestEnv(t)
	_, s := e.open()
	out := filepath.Join(t.TempDir(), "app.sql")
	body := `{"conn":"demo-sqlite","format":"plain","out":"` + out + `"}`
	if r := decodeData[dumpResp](t, e.api("POST", "/api/v1/dump", body, 200)); !r.OK {
		t.Fatalf("start: %+v", r)
	}
	if r := decodeData[dumpResp](t, e.api("GET", "/api/v1/dump", "", 200)); r.Running == nil || r.Running.Conn != "demo-sqlite" {
		t.Fatalf("running: %+v", r)
	}
	e.api("POST", "/api/v1/dump", body, 409)

	start := time.Now()
	e.api("POST", "/api/v1/dump/stop", "", 200)
	lines := strings.Join(awaitDump(t, s), "\n")
	if !strings.Contains(lines, "warn: dump of demo-sqlite stopped") {
		t.Fatalf("lines:\n%s", lines)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("the stop took the whole sleep")
	}
	for _, p := range []string{out, out + ".partial"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", p, err)
		}
	}
}

// ⤓ Download (N-171): the dump goes to a temp file of the server, the
// page that asked is told its token is ready, and the file is sent once,
// as an attachment, then removed; a directory format does not download.
func TestDumpDownload(t *testing.T) {
	fakeDumpTool(t, "0")
	e := newTestEnv(t)
	_, s := e.open()
	type started struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Out   string `json:"out"`
		Token string `json:"token"`
	}
	r := decodeData[started](t, e.api("POST", "/api/v1/dump", `{"conn":"demo-sqlite","format":"directory","out":"~/d","download":true}`, 200))
	if r.OK || !strings.Contains(r.Error, "single-file") {
		t.Fatalf("a directory download: %+v", r)
	}
	r = decodeData[started](t, e.api("POST", "/api/v1/dump", `{"conn":"demo-sqlite","format":"plain","download":true}`, 200))
	if !r.OK || len(r.Token) != 32 || !strings.Contains(r.Out, "dbc-download-") {
		t.Fatalf("start: %+v", r)
	}
	// its "dump" event names the token, the file's name and size
	var ready struct {
		Ready, Name string
		Size        int64
	}
	for ready.Ready == "" {
		ev, _ := s.await(t, "dump")
		_ = json.Unmarshal(ev.Data, &ready)
	}
	if ready.Ready != r.Token || ready.Name != filepath.Base(r.Out) || ready.Size != int64(len("-- dumped\n")) {
		t.Fatalf("ready = %+v", ready)
	}
	res := e.req("GET", "/api/v1/dump/file/"+r.Token, "", nil)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "-- dumped\n" ||
		res.Header.Get("Content-Disposition") != `attachment; filename="`+ready.Name+`"` {
		t.Fatalf("download = %d %q %q", res.StatusCode, body, res.Header.Get("Content-Disposition"))
	}
	if _, err := os.Stat(filepath.Dir(r.Out)); !os.IsNotExist(err) {
		t.Errorf("the temp directory is still there: %v", err)
	}
	e.api("GET", "/api/v1/dump/file/"+r.Token, "", 404) // once
}
