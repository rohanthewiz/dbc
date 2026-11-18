package explain

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// widePlan is an Append over n partition scans, the cost of scan i
// growing with i, so the heaviest are the last ones.
func widePlan(t *testing.T, n int) *Plan {
	t.Helper()
	var kids []string
	for i := range n {
		kids = append(kids, fmt.Sprintf(`{"Node Type": "Seq Scan", "Relation Name": "events_p%d", "Alias": "e",
		  "Startup Cost": 0, "Total Cost": %d, "Plan Rows": %d, "Plan Width": 40}`, i, 10+i, 100+i))
	}
	js := `[{"Plan": {"Node Type": "Append", "Startup Cost": 0, "Total Cost": 99999, "Plan Rows": 50000, "Plan Width": 40,
	  "Plans": [` + strings.Join(kids, ",") + `]}}]`
	p, err := ParsePostgresJSON([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	p.Finalize()
	return p
}

// A step with a long run of children draws its heaviest few and one
// "+N more" box (N-301): an Append over 200 partitions was a 20000×199 px
// strip. Its picture is now about as wide as eight cards, at full scale.
func TestPictureCapsSiblings(t *testing.T) {
	for _, n := range []int{200, 1000} {
		p := widePlan(t, n)
		img, err := p.Picture(PictureOptions{Scale: 1})
		if err != nil {
			t.Fatal(err)
		}
		b := img.Bounds()
		if b.Dx() > 2600 || b.Dy() < 300 {
			t.Errorf("%d children: %d×%d px, want a picture about eight cards wide and full height", n, b.Dx(), b.Dy())
		}
		l := &picLayout{plan: p, metric: p.Metric}
		l.capKids()
		if got := len(l.more[p.Root.ID]); got != n-picShowKids {
			t.Errorf("%d children: %d hidden, want %d", n, got, n-picShowKids)
		}
		// the heaviest are the ones kept: the last scans, costliest
		for _, c := range l.shownKids(p.Root) {
			if !strings.HasPrefix(c.Relation, "events_p") || c.ID < p.Root.Children[n-picShowKids].ID {
				t.Errorf("%d children: kept %s, not one of the heaviest", n, c.Relation)
			}
		}
		if out := os.Getenv("DBC_PIC_OUT"); out != "" && n == 200 {
			png, _ := p.PNG(PictureOptions{})
			_ = os.WriteFile(out, png, 0o644)
		}
	}
	// at picMaxKids or fewer nothing is capped
	p := widePlan(t, picMaxKids)
	l := &picLayout{plan: p, metric: p.Metric}
	l.capKids()
	if len(l.more) != 0 {
		t.Errorf("%d children capped", picMaxKids)
	}
}
