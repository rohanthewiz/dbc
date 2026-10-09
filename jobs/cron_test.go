package jobs

import (
	"strings"
	"testing"
	"time"
)

// TestCronNext is the table the plan asks for: expressions against
// instants, month ends, leap days, the Vixie day rule, and both DST days in
// New York (2026-03-08 the clocks skip 02:00–03:00; 2026-11-01 they repeat
// 01:00–02:00).
func TestCronNext(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	utc := time.UTC
	at := func(loc *time.Location, s string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		expr  string
		loc   *time.Location
		after string
		want  string // in loc; "" never
		// wantUTC, when set, pins the instant itself (DST cases, where the
		// wall clock alone is ambiguous)
		wantUTC string
	}{
		{"0 2 * * *", utc, "2026-10-09 01:59", "2026-10-09 02:00", ""},
		{"0 2 * * *", utc, "2026-10-09 02:00", "2026-10-10 02:00", ""}, // strictly after
		{"*/15 * * * *", utc, "2026-10-09 10:07", "2026-10-09 10:15", ""},
		{"*/15 9-17 * * 1-5", utc, "2026-10-09 17:50", "2026-10-12 09:00", ""}, // Fri evening → Mon
		{"5/20 * * * *", utc, "2026-10-09 10:46", "2026-10-09 11:05", ""},
		{"10-50/20 * * * *", utc, "2026-10-09 10:31", "2026-10-09 10:50", ""},
		{"0 0 1,15 * *", utc, "2026-10-09 00:00", "2026-10-15 00:00", ""},
		{"0 0 31 * *", utc, "2026-04-01 00:00", "2026-05-31 00:00", ""}, // April has no 31st
		{"0 0 29 2 *", utc, "2026-03-01 00:00", "2028-02-29 00:00", ""}, // the next leap day
		{"0 0 30 2 *", utc, "2026-03-01 00:00", "", ""},                 // never
		{"0 12 * jan-mar mon", utc, "2026-10-09 00:00", "2027-01-04 12:00", ""},
		{"0 0 * * 7", utc, "2026-10-09 00:00", "2026-10-11 00:00", ""}, // 7 is Sunday
		{"0 0 * * SUN", utc, "2026-10-09 00:00", "2026-10-11 00:00", ""},
		// both day fields restricted: either matches (the 13th, or a Friday)
		{"0 0 13 * fri", utc, "2026-10-09 00:00", "2026-10-13 00:00", ""},
		{"0 0 13 * fri", utc, "2026-10-13 00:00", "2026-10-16 00:00", ""},
		// a day-of-month step starts with '*': both must match
		{"0 0 */2 * fri", utc, "2026-10-09 00:00", "2026-10-23 00:00", ""},
		{"@daily", utc, "2026-10-09 13:00", "2026-10-10 00:00", ""},
		{"@hourly", utc, "2026-10-09 13:00", "2026-10-09 14:00", ""},
		{"@monthly", utc, "2026-12-15 00:00", "2027-01-01 00:00", ""},
		// spring forward: 02:30 does not exist on 2026-03-08 in New York;
		// it fires when the clocks jump, 03:00 EDT (07:00Z)
		{"30 2 * * *", ny, "2026-03-07 12:00", "", "2026-03-08 07:00"},
		{"0 2 * * *", ny, "2026-03-08 03:00", "2026-03-09 02:00", ""}, // and back to normal
		{"*/15 * * * *", ny, "2026-03-08 01:50", "", "2026-03-08 07:00"},
		{"*/15 * * * *", ny, "2026-03-08 03:00", "2026-03-08 03:15", ""},
		// fall back: 01:30 happens twice on 2026-11-01; it fires on the
		// first pass (EDT, 05:30Z) only
		{"30 1 * * *", ny, "2026-10-31 12:00", "", "2026-11-01 05:30"},
		{"30 1 * * *", ny, "2026-11-01 05:30 UTC", "2026-11-02 01:30", ""},
		{"30 1 * * *", ny, "2026-11-01 06:10 UTC", "2026-11-02 01:30", ""}, // from the second pass
		{"0 2 * * *", ny, "2026-10-31 12:00", "", "2026-11-01 07:00"},      // 02:00 EST
	}
	for _, c := range cases {
		s, err := ParseCron(c.expr, c.loc)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		var after time.Time
		if a, ok := strings.CutSuffix(c.after, " UTC"); ok {
			after = at(utc, a)
		} else {
			after = at(c.loc, c.after)
		}
		got := s.Next(after)
		switch {
		case c.wantUTC != "":
			if want := at(utc, c.wantUTC); !got.Equal(want) {
				t.Errorf("%s after %s = %s, want %s", c.expr, c.after, got.UTC(), want)
			}
		case c.want == "":
			if !got.IsZero() {
				t.Errorf("%s after %s = %s, want never", c.expr, c.after, got)
			}
		default:
			if want := at(c.loc, c.want); !got.Equal(want) {
				t.Errorf("%s after %s = %s, want %s", c.expr, c.after, got.In(c.loc), want)
			}
		}
	}
}

// The fires through a whole fall-back night: one per wall time, so the
// repeated hour adds nothing and loses nothing but its second pass.
func TestCronFallBackFiresOnce(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	s, err := ParseCron("30 * * * *", ny)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	var got []string
	for _, f := range s.NextN(start, 4) {
		got = append(got, f.In(ny).Format("15:04 MST"))
	}
	want := "00:30 EDT,01:30 EDT,02:30 EST,03:30 EST"
	if strings.Join(got, ",") != want {
		t.Errorf("fires = %v, want %s", got, want)
	}
}

func TestCronParseErrors(t *testing.T) {
	for expr, msg := range map[string]string{
		"0 2 * *":       "5 fields",
		"60 * * * *":    "minute: 60 is out of range",
		"* 24 * * *":    "hour: 24 is out of range",
		"* * 0 * *":     "day of month: 0 is out of range",
		"* * * 13 *":    "month: 13 is out of range",
		"* * * * 8":     "day of week: 8 is out of range",
		"* * * foo *":   `month: "foo" is not a number`,
		"*/0 * * * *":   "not a positive number",
		"5-1 * * * *":   "runs backwards",
		"1,,2 * * * *":  "empty item",
		"@fortnightly":  "unknown cron macro",
		"* * * * mon-x": `"x" is not a number`,
	} {
		_, err := ParseCron(expr, time.UTC)
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: err = %v, want %q", expr, err, msg)
		}
	}
}
