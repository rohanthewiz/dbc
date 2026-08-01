package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func tempHistory(t *testing.T) *history {
	t.Helper()
	return loadHistory(filepath.Join(t.TempDir(), "history.jsonl"))
}

// The history survives a restart, newest last on disk and newest first in the
// list the recall modal shows.
func TestHistoryRoundTrips(t *testing.T) {
	h := tempHistory(t)
	at := time.Date(2026, 7, 31, 19, 30, 0, 0, time.UTC)
	for i, sql := range []string{"SELECT 1", "SELECT 2", "SELECT 3"} {
		if err := h.add("demo", sql, at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	reloaded := loadHistory(h.path)
	got := make([]string, 0, 3)
	for _, e := range reloaded.recent() {
		got = append(got, e.SQL)
	}
	if want := []string{"SELECT 3", "SELECT 2", "SELECT 1"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("recent() = %v, want newest first %v", got, want)
	}
	if e := reloaded.recent()[0]; e.Conn != "demo" || !e.At.Equal(at.Add(2*time.Minute)) {
		t.Errorf("entry lost its connection or time: %+v", e)
	}
}

// A multi-line statement is exactly what a line-oriented history file would
// mangle, so it is the one that has to come back verbatim.
func TestHistoryKeepsMultiLineSQL(t *testing.T) {
	h := tempHistory(t)
	sql := "SELECT name,\n       age\nFROM cats\nWHERE breed = 'Tabby'"
	if err := h.add("demo", sql, time.Now()); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := loadHistory(h.path).recent()[0].SQL; got != sql {
		t.Errorf("recalled %q, want %q", got, sql)
	}
}

// Re-running the same query while watching a table change must not bury the
// rest of the history.
func TestHistorySkipsConsecutiveDuplicates(t *testing.T) {
	h := tempHistory(t)
	at := time.Now()
	for _, sql := range []string{"SELECT 1", "SELECT 1", "  SELECT 1  ", "SELECT 2", "SELECT 1"} {
		if err := h.add("demo", sql, at); err != nil {
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
	if err := h.add("demo", "   ", at); err != nil {
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
	h := &history{path: path}
	at := time.Now()
	for i := range histMax + 20 {
		if err := h.add("demo", "SELECT "+strconv.Itoa(i), at); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	trimmed := loadHistory(path)
	if got := len(trimmed.entries); got != histMax {
		t.Fatalf("loaded %d entries, want the cap of %d", got, histMax)
	}
	if got := trimmed.recent()[0].SQL; got != "SELECT "+strconv.Itoa(histMax+19) {
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
	good, err := json.Marshal(histEntry{At: time.Now(), Conn: "demo", SQL: "SELECT 1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(good) + "\n" + `{"at":"2026-07-31T19:` + "\n" + string(good) + "\n"
	if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := len(loadHistory(path).entries); got != 2 {
		t.Errorf("kept %d entries, want the 2 readable ones", got)
	}
}

// A history with nowhere to live still works for the session.
func TestHistoryWithoutAFile(t *testing.T) {
	h := loadHistory("")
	if err := h.add("demo", "SELECT 1", time.Now()); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := len(h.recent()); got != 1 {
		t.Errorf("in-memory history kept %d entries, want 1", got)
	}
}

// The history file collects real values, so it is nobody else's business.
func TestHistoryFileIsUserOnly(t *testing.T) {
	h := tempHistory(t)
	if err := h.add("demo", "SELECT 'hunter2'", time.Now()); err != nil {
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
	entries := []histEntry{
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
		if got := len(matchHistory(entries, c.q)); got != c.want {
			t.Errorf("matchHistory(%q) kept %d, want %d", c.q, got, c.want)
		}
	}
	// filtering must not disturb the entries it was given
	if len(entries) != 3 {
		t.Errorf("matchHistory clobbered its input: %v", entries)
	}
}
