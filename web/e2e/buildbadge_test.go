package e2e

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/go-rod/rod"
)

// buildBadge locks down the build badge beside the topbar brand
// (github.com/rohanthewiz/cats/buildbadge, mounted into .topbar-build):
//
//  1. It shows the short hash of the commit dbc was built from. The harness
//     builds unstamped (build in harness_test.go), so the hash comes from
//     the VCS info the toolchain records — HEAD of this checkout, with "*"
//     when the tree is modified, which a working tree under test often is.
//  2. Hovering it opens the card, whose BUILD row names that same hash;
//     moving away closes it again, so it never lingers over later steps.
func buildBadge(t *testing.T, _ *env, p *rod.Page) {
	waitFor(t, p, "the build badge", `() => !!document.querySelector(".topbar-build .build-badge")`)
	got := evalStr(t, p, `() => document.querySelector(".topbar-build .build-badge").textContent`)
	if !regexp.MustCompile(`^[0-9a-f]{7}\*?$`).MatchString(got) {
		t.Fatalf("badge = %q, want a 7-character hash, * when modified", got)
	}
	hash := strings.TrimSuffix(got, "*")
	// git is on PATH for every run of this suite: the toolchain needs it to
	// record the VCS info the badge reads in the first place
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if !strings.HasPrefix(string(head), hash) {
		t.Fatalf("badge = %q, want HEAD %s", got, strings.TrimSpace(string(head)))
	}

	p.MustElement(".topbar-build .build-badge").MustHover()
	waitFor(t, p, "the card naming the build", `(h) => {
	  const c = document.querySelector(".bb-tip.show");
	  return !!c && c.textContent.includes(h); }`, hash)
	shot(t, p, "build-badge") // its look against the theme: no predicate says that
	p.Mouse.MustMoveTo(0, 0)
	waitFor(t, p, "the card closed", `() => !document.querySelector(".bb-tip.show")`)
}
