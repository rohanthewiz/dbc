package pages

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"sync"

	"github.com/rohanthewiz/cats/buildbadge"
)

// The build badge (github.com/rohanthewiz/cats/buildbadge) arrives as one
// block of <head> markup: its stylesheet, a <script> publishing this build's
// identity, and a <script> holding the badge library. Both scripts are
// inline, and dbc web's CSP allows no inline script (securityHeaders in
// web/auth.go), so the header names each one by hash instead — the same
// approach as the standalone plan page (explain.ScriptHash). The policy
// stays "this server's files plus exactly these scripts"; nothing injected
// beside them would run.
//
// The page and the header must hash the very same bytes, so the markup is
// resolved once and both read it from here:
//
//	buildbadge.Head() ──once──▶ badgeHead ──▶ head()          (the page)
//	                                     └──▶ BadgeScriptSrc  (the CSP)
//
// Head() is constant for the life of the binary anyway (the stamp is link
// time, the VCS info is read once), so caching it costs nothing and
// guarantees the two can't drift.
var badgeHead = sync.OnceValue(buildbadge.Head)

// inlineScript matches an inline <script>'s body. The badge's scripts carry
// no src, and its library contains no "</script>" (it could not be inlined
// at all if it did), so a lazy match to the first close tag is the body.
var inlineScript = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)

// BadgeScriptSrc is the CSP script-src sources for the badge's inline
// scripts — " 'sha256-…' 'sha256-…'", with its leading space, ready to
// append after 'self' — or "" for a build with no known hash, whose page
// carries no badge and so needs nothing allowed.
var BadgeScriptSrc = sync.OnceValue(func() string {
	var b strings.Builder
	for _, m := range inlineScript.FindAllStringSubmatch(badgeHead(), -1) {
		sum := sha256.Sum256([]byte(m[1]))
		b.WriteString(" 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'")
	}
	return b.String()
})
