package jobs

import (
	"testing"
	"time"
)

// FuzzParseCron parses fuzzed cron expressions (N-302). An accepted one
// must give fire times that are in the future, in order, on a whole
// minute, and each one a time the schedule itself matches; and finding
// them must end — the scheduler asks for the next fire time of every job
// on every tick. Run at length by hand:
//
//	go test -run '^$' -fuzz '^FuzzParseCron$' -fuzztime 5m ./jobs
func FuzzParseCron(f *testing.F) {
	for _, s := range []string{
		"* * * * *", "0 0 * * *", "*/15 9-17 * * 1-5", "0 0 29 2 *", "0 0 31 4 *", "5,10-20/3 * * * 0,7",
		"@daily", "@hourly", "@weekly", "0 0 1 1 *", "59 23 31 12 7", "0 0 * * sun", "0 0 1 jan *",
		"60 * * * *", "* * * *", "@nope", "", "1-0 * * * *", "*/0 * * * *",
	} {
		f.Add(s, int64(0))
		f.Add(s, int64(1_700_000_000))
	}
	loc, err := time.LoadLocation("America/New_York") // a zone with DST gaps and repeats
	if err != nil {
		loc = time.UTC
	}
	f.Fuzz(func(t *testing.T, expr string, at int64) {
		s, err := ParseCron(expr, loc)
		if err != nil {
			return
		}
		// a start between 1970 and 2100: the zones' modern rules (before
		// 1883 New York kept local mean time, an offset in seconds)
		if at < 0 {
			at = -(at + 1)
		}
		after := time.Unix(at%(130*365*24*3600), 0).In(loc)
		prev := after
		for _, next := range s.NextN(after, 4) {
			if next.IsZero() {
				// a schedule that never fires (Feb 30th) has no next time
				return
			}
			if !next.After(prev) {
				t.Fatalf("%q: %v is not after %v", expr, next, prev)
			}
			if next.Second() != 0 || next.Nanosecond() != 0 {
				t.Fatalf("%q: %v is not on a whole minute", expr, next)
			}
			if !fires(s, next) {
				t.Fatalf("%q: %v is a fire time the schedule does not match", expr, next)
			}
			prev = next
		}
	})
}

// fires reports whether t is a time s may fire at: its wall clock matches
// every field, or it is the instant the clocks jumped over a wall time
// that would have (Next's rule for a skipped time).
func fires(s *Schedule, t time.Time) bool {
	l := t.In(s.loc)
	w := naive(l)
	if s.month.has(int(w.Month())) && s.dayMatches(w) && s.hour.has(w.Hour()) && s.minute.has(w.Minute()) {
		return true
	}
	start, _ := l.ZoneBounds()
	return t.Equal(start)
}
