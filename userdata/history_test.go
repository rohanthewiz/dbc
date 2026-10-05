package userdata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func tempHistory(t *testing.T) *History {
	t.Helper()
	return LoadHistory(filepath.Join(t.TempDir(), "history.jsonl"))
}

// The history survives a restart, newest last on disk and newest first in the
// list the recall modal shows.
func TestHistoryRoundTrips(t *testing.T) {
	h := tempHistory(t)
	at := time.Date(2026, 7, 31, 19, 30, 0, 0, time.UTC)
	for i, sql := range []string{"SELECT 1", "SELECT 2", "SELECT 3"} {
		if err := h.Add("demo", sql, at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	reloaded := LoadHistory(h.path)
	got := make([]string, 0, 3)
	for _, e := range reloaded.Recent() {
		got = append(got, e.SQL)
	}
	if want := []string{"SELECT 3", "SELECT 2", "SELECT 1"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("recent() = %v, want newest first %v", got, want)
	}
	if e := reloaded.Recent()[0]; e.Conn != "demo" || !e.At.Equal(at.Add(2*time.Minute)) {
		t.Errorf("entry lost its connection or time: %+v", e)
	}
}

// A multi-line statement is exactly what a line-oriented history file would
// mangle, so it is the one that has to come back verbatim.
func TestHistoryKeepsMultiLineSQL(t *testing.T) {
	h := tempHistory(t)
	sql := "SELECT name,\n       age\nFROM cats\nWHERE breed = 'Tabby'"
	if err := h.Add("demo", sql, time.Now()); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := LoadHistory(h.path).Recent()[0].SQL; got != sql {
		t.Errorf("recalled %q, want %q", got, sql)
	}
}

// Re-running the same query while watching a table change must not bury the
// rest of the history.
func TestHistorySkipsConsecutiveDuplicates(t *testing.T) {
	h := tempHistory(t)
	at := time.Now()
	for _, sql := range []string{"SELECT 1", "SELECT 1", "  SELECT 1  ", "SELECT 2", "SELECT 1"} {
		if err := h.Add("demo", sql, at); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if got := len(h.entries); got != 3 {
		t.Errorf("kept %d entries, want 3 — %v", got, h.entries)
	}
	// but a repeat that is not consecutive is a real recurrence
	if last := h.entries[len(h.entries)-1].SQL; last != "SELECT 1" {
		t.Errorf("last entry = %q, want the re-run SELECT 1", last)
	}
	if err := h.Add("demo", "   ", at); err != nil {
		t.Fatalf("add blank: %v", err)
	}
	if got := len(h.entries); got != 3 {
		t.Errorf("a blank statement was recorded: %v", h.entries)
	}
}

// The file is bounded: an over-long history is trimmed to the newest histMax
// entries at load, and the trim is written back rather than re-done forever.
func TestHistoryTrimsAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	h := &History{path: path}
	at := time.Now()
	for i := range histMax + 20 {
		if err := h.Add("demo", "SELECT "+strconv.Itoa(i), at); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	trimmed := LoadHistory(path)
	if got := len(trimmed.entries); got != histMax {
		t.Fatalf("loaded %d entries, want the cap of %d", got, histMax)
	}
	if got := trimmed.Recent()[0].SQL; got != "SELECT "+strconv.Itoa(histMax+19) {
		t.Errorf("newest entry = %q, want the last one written", got)
	}
	// the trim reached the file, so it does not have to happen again
	bs, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if got := strings.Count(strings.TrimSpace(string(bs)), "\n") + 1; got != histMax {
		t.Errorf("file holds %d lines, want %d", got, histMax)
	}
}

// A process killed mid-write leaves a half-written last line. That costs one
// entry, not the whole history.
func TestHistorySurvivesACorruptLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	good, err := json.Marshal(Entry{At: time.Now(), Conn: "demo", SQL: "SELECT 1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(good) + "\n" + `{"at":"2026-07-31T19:` + "\n" + string(good) + "\n"
	if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := len(LoadHistory(path).entries); got != 2 {
		t.Errorf("kept %d entries, want the 2 readable ones", got)
	}
}

// A history with nowhere to live still works for the session.
func TestHistoryWithoutAFile(t *testing.T) {
	h := LoadHistory("")
	if err := h.Add("demo", "SELECT 1", time.Now()); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := len(h.Recent()); got != 1 {
		t.Errorf("in-memory history kept %d entries, want 1", got)
	}
}

// The history file collects real values, so it is nobody else's business.
func TestHistoryFileIsUserOnly(t *testing.T) {
	h := tempHistory(t)
	if err := h.Add("demo", "SELECT 'hunter2'", time.Now()); err != nil {
		t.Fatalf("add: %v", err)
	}
	fi, err := os.Stat(h.path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("history file mode is %o, want 600", perm)
	}
}

func TestMatchHistory(t *testing.T) {
	entries := []Entry{
		{SQL: "SELECT * FROM cats", Conn: "demo"},
		{SQL: "UPDATE dogs SET good = 1", Conn: "prod"},
		{SQL: "SELECT count(*) FROM DOGS", Conn: "demo"},
	}
	cases := []struct {
		q    string
		want int
	}{
		{"", 3},
		{"  ", 3},
		{"dogs", 2},   // case-insensitive, both spellings
		{"DOGS", 2},   //
		{"prod", 1},   // the connection name matches too
		{"select", 2}, //
		{"nothing here", 0},
	}
	for _, c := range cases {
		if got := len(MatchHistory(entries, c.q)); got != c.want {
			t.Errorf("MatchHistory(%q) kept %d, want %d", c.q, got, c.want)
		}
	}
	// filtering must not disturb the entries it was given
	if len(entries) != 3 {
		t.Errorf("MatchHistory clobbered its input: %v", entries)
	}
}

// The database rides along in the file and back (N-101); an entry written
// before the field existed loads with none, and HistoryIn keeps it out of
// every database's scope while "" keeps everything. The same statement on
// two databases is two entries, not a repeat.
func TestHistoryScopedByDatabase(t *testing.T) {
	h := tempHistory(t)
	old := `{"at":"2026-07-31T19:30:00Z","conn":"demo","sql":"SELECT 'old'"}` + "\n"
	if err := os.WriteFile(h.path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	h = LoadHistory(h.path)
	at := time.Now()
	for _, e := range []Entry{
		{At: at, Conn: "a", SQL: "SELECT 1", DB: "localhost_5432/app"},
		{At: at, Conn: "b", SQL: "SELECT 1", DB: "localhost_5432/other"},
		{At: at, Conn: "a2", SQL: "SELECT 1", DB: "localhost_5432/other"}, // a repeat on the same database
	} {
		if err := h.AddEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	all := LoadHistory(h.path).Recent()
	if len(all) != 3 {
		t.Fatalf("reloaded %d entries, want 3: %+v", len(all), all)
	}
	if all[2].DB != "" || all[0].DB != "localhost_5432/other" {
		t.Errorf("databases did not round-trip: %+v", all)
	}
	if got := HistoryIn(all, "localhost_5432/app"); len(got) != 1 || got[0].Conn != "a" {
		t.Errorf("HistoryIn(app) = %+v", got)
	}
	if got := HistoryIn(all, ""); len(got) != 3 {
		t.Errorf("HistoryIn(\"\") kept %d, want all", len(got))
	}
	// the file stays readable as before: no "db" on an entry without one
	bs, _ := os.ReadFile(h.path)
	if first := strings.SplitN(string(bs), "\n", 2)[0]; strings.Contains(first, `"db"`) {
		t.Errorf("an entry with no database grew a db field: %s", first)
	}
	if (ConsoleDB{}).Key() != "" || ConsoleDBOf("h:1", "app").Key() != "h_1/app" {
		t.Errorf("Key: %q, %q", (ConsoleDB{}).Key(), ConsoleDBOf("h:1", "app").Key())
	}
}
