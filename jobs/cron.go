package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// A small five-field cron parser, in house rather than a dependency for
// ~200 lines (see the plan's decisions), with one job: given an instant,
// when does the schedule fire next?
//
//	┌──────── minute        0–59
//	│ ┌────── hour          0–23
//	│ │ ┌──── day of month  1–31
//	│ │ │ ┌── month         1–12 or jan–dec
//	│ │ │ │ ┌ day of week   0–7 or sun–sat (0 and 7 are Sunday)
//	│ │ │ │ │
//	0 2 * * *          02:00 every day
//	*/15 9-17 * * 1-5  every quarter hour, 09:00–17:45, Monday to Friday
//	0 0 1,15 * *       midnight on the 1st and the 15th
//
// Each field is a comma list of `*`, `N`, `N-M`, with an optional `/step`
// (`*/15`, `10-50/20`, and `5/15`, which is `5-59/15`). Names are
// case-insensitive. The macros @yearly (@annually), @monthly, @weekly,
// @daily (@midnight) and @hourly stand for their five fields.
//
// DAY OF MONTH AND DAY OF WEEK follow Vixie cron, as every crontab does:
// when both are restricted (neither starts with `*`) a day matches if
// EITHER does — "0 0 1 * mon" is the 1st and every Monday — and otherwise
// both must.
//
// WALL CLOCK AND DST. A schedule is wall-clock time in its location (the
// job's "tz", else the process's local zone), so "0 2 * * *" stays 02:00
// across a daylight-saving change. The two days a year that clock is not a
// plain line:
//
//	clocks go forward (02:00 → 03:00): a time inside the skipped hour
//	    fires once, at the moment the clocks jump — "30 2 * * *" fires at
//	    03:00 that day — as Vixie cron runs such jobs "right after the
//	    change", rather than silently missing a nightly run
//	clocks go back (02:00 → 01:00): a time inside the repeated hour fires
//	    once, the first time round
//
// The search works in wall-clock terms (a naive calendar, in UTC, where
// every day has 24 hours) and maps each matching wall time to an instant
// in the location; see Next.

// Schedule is one parsed cron expression.
type Schedule struct {
	expr                         string
	minute, hour, dom, month, dw bits
	domStar, dowStar             bool // the field began with '*': see the Vixie rule above
	loc                          *time.Location
}

// bits is a set of small integers (0–63).
type bits uint64

func (b bits) has(n int) bool { return b&(1<<uint(n)) != 0 }

// field is one position of the expression: its range and its names.
type field struct {
	name     string
	min, max int
	names    []string // index + min is the value (months from 1, days from 0)
}

var (
	fMinute = field{name: "minute", min: 0, max: 59}
	fHour   = field{name: "hour", min: 0, max: 23}
	fDom    = field{name: "day of month", min: 1, max: 31}
	fMonth  = field{name: "month", min: 1, max: 12,
		names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}}
	// 7 is Sunday as well as 0 (folded into 0 after parsing)
	fDow = field{name: "day of week", min: 0, max: 7,
		names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}}
)

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *",
	"@monthly": "0 0 1 * *", "@weekly": "0 0 * * 0",
	"@daily": "0 0 * * *", "@midnight": "0 0 * * *",
	"@hourly": "0 * * * *",
}

// ParseCron parses a five-field expression (or a macro) to fire in loc;
// nil loc is time.Local. The error says which field is wrong and why.
func ParseCron(expr string, loc *time.Location) (*Schedule, error) {
	if loc == nil {
		loc = time.Local
	}
	text := strings.TrimSpace(expr)
	if m, ok := macros[strings.ToLower(text)]; ok {
		text = m
	} else if strings.HasPrefix(text, "@") {
		return nil, serr.New("unknown cron macro (@yearly, @monthly, @weekly, @daily, @hourly)", "cron", expr)
	}
	parts := strings.Fields(text)
	if len(parts) != 5 {
		return nil, serr.New(fmt.Sprintf("a cron expression has 5 fields (minute hour day-of-month month day-of-week), not %d", len(parts)),
			"cron", expr)
	}
	s := &Schedule{expr: strings.TrimSpace(expr), loc: loc}
	var err error
	if s.minute, err = parseField(parts[0], fMinute); err != nil {
		return nil, serr.Wrap(err, "cron", expr)
	}
	if s.hour, err = parseField(parts[1], fHour); err != nil {
		return nil, serr.Wrap(err, "cron", expr)
	}
	if s.dom, err = parseField(parts[2], fDom); err != nil {
		return nil, serr.Wrap(err, "cron", expr)
	}
	if s.month, err = parseField(parts[3], fMonth); err != nil {
		return nil, serr.Wrap(err, "cron", expr)
	}
	if s.dw, err = parseField(parts[4], fDow); err != nil {
		return nil, serr.Wrap(err, "cron", expr)
	}
	if s.dw.has(7) {
		s.dw = s.dw&^(1<<7) | 1
	}
	s.domStar = strings.HasPrefix(parts[2], "*")
	s.dowStar = strings.HasPrefix(parts[4], "*")
	return s, nil
}

// parseField reads one field: a comma list of ranges with optional steps.
func parseField(text string, f field) (bits, error) {
	var out bits
	for item := range strings.SplitSeq(text, ",") {
		if item == "" {
			return 0, serr.New(f.name + ": an empty item in the list")
		}
		rng, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return 0, serr.New(fmt.Sprintf("%s: the step in %q is not a positive number", f.name, item))
			}
			step = n
		}
		var lo, hi int
		switch {
		case rng == "*":
			lo, hi = f.min, f.max
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, err
			}
			if hi, err = f.value(b); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, serr.New(fmt.Sprintf("%s: the range %q runs backwards", f.name, rng))
			}
		default:
			var err error
			if lo, err = f.value(rng); err != nil {
				return 0, err
			}
			hi = lo
			// "5/15" is "5-max/15", as Vixie cron reads it
			if hasStep {
				hi = f.max
			}
		}
		for n := lo; n <= hi; n += step {
			out |= 1 << uint(n)
		}
	}
	return out, nil
}

// value is one number or name of the field, in range.
func (f field) value(s string) (int, error) {
	for i, nm := range f.names {
		if strings.EqualFold(s, nm) {
			return i + f.min, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, serr.New(fmt.Sprintf("%s: %q is not a number or a name", f.name, s))
	}
	if n < f.min || n > f.max {
		return 0, serr.New(fmt.Sprintf("%s: %d is out of range %d–%d", f.name, n, f.min, f.max))
	}
	return n, nil
}

// String is the expression as written.
func (s *Schedule) String() string { return s.expr }

// Location is where the schedule's wall clock is read.
func (s *Schedule) Location() *time.Location { return s.loc }

// searchYears bounds Next: an expression that matches no day in this many
// years (the 30th of February) never fires. Four years covers the 29th of
// February; one more covers a leap day that falls on a given weekday.
const searchYears = 9

// Next is the first instant strictly after `after` at which the schedule
// fires, or the zero time when it never does.
//
//  1. w = the wall-clock minute after `after`, in the schedule's location,
//     as a naive calendar time (UTC, so every day is 24 hours)
//  2. advance w to the next wall time the fields match: a month that does
//     not match jumps to the next month's first minute, a day to the next
//     day's, an hour to the next hour's — a handful of steps, not a scan
//  3. map w to an instant in the location (see instant): the first
//     instant showing that wall time, or for a time the clocks skipped,
//     the instant they jumped. Fall-back's repeated hour is never fired
//     twice: w only moves forward, so a wall time once passed is not
//     seen again
//  4. an instant not after `after` (the first pass of a repeated hour,
//     reached from the second) is no fire: step w on and go again
func (s *Schedule) Next(after time.Time) time.Time {
	a := after.In(s.loc)
	w := time.Date(a.Year(), a.Month(), a.Day(), a.Hour(), a.Minute(), 0, 0, time.UTC).Add(time.Minute)
	limit := w.AddDate(searchYears, 0, 0)
	for w.Before(limit) {
		w = s.nextWall(w, limit)
		if w.IsZero() {
			return time.Time{}
		}
		if t := instant(w, s.loc); t.After(after) {
			return t
		}
		w = w.Add(time.Minute)
	}
	return time.Time{}
}

// nextWall is the first naive wall time at or after w that the fields
// match, or zero past limit.
func (s *Schedule) nextWall(w, limit time.Time) time.Time {
	for w.Before(limit) {
		switch {
		case !s.month.has(int(w.Month())):
			w = time.Date(w.Year(), w.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.dayMatches(w):
			w = time.Date(w.Year(), w.Month(), w.Day()+1, 0, 0, 0, 0, time.UTC)
		case !s.hour.has(w.Hour()):
			w = time.Date(w.Year(), w.Month(), w.Day(), w.Hour()+1, 0, 0, 0, time.UTC)
		case !s.minute.has(w.Minute()):
			w = w.Add(time.Minute)
		default:
			return w
		}
	}
	return time.Time{}
}

// dayMatches applies the Vixie rule: either day field when both are
// restricted, both otherwise (a '*' field matches every day anyway).
func (s *Schedule) dayMatches(w time.Time) bool {
	dom, dow := s.dom.has(w.Day()), s.dw.has(int(w.Weekday()))
	if !s.domStar && !s.dowStar {
		return dom || dow
	}
	return dom && dow
}

// instant maps naive wall time w to an instant in loc: the earliest
// instant whose wall clock there reads w — the first pass, when the clocks
// went back over it — or, when no instant does (the clocks went forward
// over it), the instant of that jump.
//
// time.Date alone cannot say which: for a repeated or a skipped wall time
// it picks one of the zones around the change without promising which. So
// every offset the location uses near w is tried; a transition is never
// more than a day from either side.
func instant(w time.Time, loc *time.Location) time.Time {
	guess := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), 0, 0, loc)
	var best time.Time
	for _, probe := range []time.Time{guess.Add(-26 * time.Hour), guess, guess.Add(26 * time.Hour)} {
		_, off := probe.Zone()
		t := w.Add(-time.Duration(off) * time.Second).In(loc)
		if sameWall(t, w) && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	if !best.IsZero() {
		return best
	}
	// a skipped wall time: the clocks jumped over w at the start of the
	// zone the later side is in. guess is on one side of the jump: past it
	// (its wall clock reads after w), the jump began its zone; before it,
	// the jump ends its zone.
	start, end := guess.ZoneBounds()
	if naive(guess).After(w) {
		return start
	}
	return end
}

// naive is t's wall clock as a naive time, comparable with w.
func naive(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

func sameWall(t, w time.Time) bool { return naive(t).Equal(w) }

// NextN is the next n fire times after `after`, fewer when the schedule
// stops firing — what an editor shows under a cron line, so the user sees
// what they wrote means.
func (s *Schedule) NextN(after time.Time, n int) []time.Time {
	var out []time.Time
	for len(out) < n {
		t := s.Next(after)
		if t.IsZero() {
			break
		}
		out = append(out, t)
		after = t
	}
	return out
}
