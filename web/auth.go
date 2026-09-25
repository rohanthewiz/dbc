package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"
)

// Access control. dbc web is a local tool for the person who started it, so
// the scheme is deliberately small — the page can still run any SQL with the
// credentials in the config, so it is not nothing:
//
//	who                         how it gets in
//	──────────────────────────  ─────────────────────────────────────────────
//	the browser dbc opened      /login?s=<secret> → session cookie → /
//	curl, a script, a test      Authorization: Bearer <secret>
//	anything else               401
//
// Three checks run before any handler (see guard):
//
//   - HOST. The Host header must name the loopback address dbc is bound to.
//     This is the DNS-rebinding guard: a hostile page can point its own
//     hostname at 127.0.0.1, but its requests then carry that hostname.
//   - ORIGIN, on anything but GET/HEAD. A browser always sends Origin on a
//     cross-site POST; one that is not this server is refused before it can
//     run a statement. The cookie is SameSite=Strict as well, so the browser
//     would not have attached it anyway — two locks, each one line.
//   - SECRET. A session cookie, or the secret itself as a Bearer token.
//
// The secret is generated per launch (or given with --secret) and the
// cookie is a separate random value, also per launch: nothing is written to
// disk, and a restart signs every browser out. The login URL's secret leaves
// the address bar at once (the login redirects), and Referrer-Policy keeps
// it out of any Referer.

// GenerateSecret returns a random URL-safe secret (24 characters, 18 bytes).
func GenerateSecret() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", serr.Wrap(err, "op", "generate secret")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// auth holds the launch's secret and session value. Read-only after
// newAuth, so every request may use it without a lock.
type auth struct {
	secret  []byte
	session []byte // the cookie's value
}

func newAuth(secret string) (*auth, error) {
	if secret == "" {
		return nil, serr.New("empty secret")
	}
	sess, err := GenerateSecret()
	if err != nil {
		return nil, err
	}
	return &auth{secret: []byte(secret), session: []byte(sess)}, nil
}

// checkSecret compares in constant time, so response timing does not leak
// how much of a guess was right.
func (a *auth) checkSecret(s string) bool {
	return subtle.ConstantTimeCompare([]byte(s), a.secret) == 1
}

func (a *auth) checkSession(s string) bool {
	return subtle.ConstantTimeCompare([]byte(s), a.session) == 1
}

// checkBearer reads "Authorization: Bearer <secret>".
func (a *auth) checkBearer(header string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	return a.checkSecret(strings.TrimSpace(header[len(prefix):]))
}

// cookieName carries the port. Cookies are not port-scoped, so two dbc web
// processes on 127.0.0.1 would otherwise overwrite each other's session,
// each signing the other's browser out.
func (s *Server) cookieName() string {
	return "dbc_session_" + s.rw.GetListenPort()
}

// publicPath lists what is served without a session: the health check (a
// liveness probe for scripts and a future app wrapper), the login itself,
// and the static assets the "you need to log in" page is styled with.
// None of it reveals anything about the databases.
func publicPath(p string) bool {
	return p == "/api/v1/health" || p == "/login" || p == "/favicon.ico" ||
		p == "/theme.css" || strings.HasPrefix(p, "/static/")
}

// guard is the middleware every request passes through; see the checks at
// the top of the file.
func (s *Server) guard(ctx rweb.Context) error {
	req := ctx.Request()
	securityHeaders(ctx)
	if !s.hostOK(req.Host()) {
		return plain(ctx, http.StatusForbidden, "forbidden: unexpected Host header")
	}
	if m := req.Method(); m != http.MethodGet && m != http.MethodHead && !originOK(req.Header("Origin"), req.Host()) {
		return plain(ctx, http.StatusForbidden, "forbidden: cross-site request")
	}
	if publicPath(req.Path()) || s.authed(ctx) {
		return ctx.Next()
	}
	if strings.HasPrefix(req.Path(), "/api/") {
		return writeJSON(ctx, http.StatusUnauthorized, envelope{Error: "not signed in — open the URL dbc web printed"})
	}
	return writePage(ctx, http.StatusUnauthorized, signInPage())
}

func (s *Server) authed(ctx rweb.Context) bool {
	if s.auth.checkBearer(ctx.Request().Header("Authorization")) {
		return true
	}
	c, err := ctx.GetCookie(s.cookieName())
	return err == nil && s.auth.checkSession(c)
}

// login trades the launch secret for the session cookie and sends the
// browser on to the workbench, taking the secret out of the address bar.
func (s *Server) login(ctx rweb.Context) error {
	if !s.auth.checkSecret(ctx.Request().QueryParam("s")) {
		return writePage(ctx, http.StatusUnauthorized, signInPage())
	}
	err := ctx.SetCookieWithOptions(&rweb.Cookie{
		Name: s.cookieName(), Value: string(s.auth.session), Path: "/",
		HttpOnly: true, SameSite: rweb.SameSiteStrictMode,
	})
	if err != nil {
		return serr.Wrap(err, "op", "set session cookie")
	}
	return ctx.Redirect(http.StatusSeeOther, "/")
}

// hostOK is the DNS-rebinding guard. The port must be ours, and the name a
// loopback one — or, when dbc was asked to listen on a specific non-loopback
// address, that address. Listening on every interface (0.0.0.0, ::) accepts
// any name: the user chose to be reachable by whatever name the network
// gives this machine, and the secret still stands guard.
func (s *Server) hostOK(host string) bool {
	name, port, err := net.SplitHostPort(host)
	if err != nil || port != s.rw.GetListenPort() {
		return false
	}
	switch name {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	bound, _, _ := net.SplitHostPort(s.rw.GetListenAddr())
	if ip := net.ParseIP(bound); ip != nil && ip.IsUnspecified() {
		return true
	}
	return name == bound
}

// originOK reports whether a state-changing request is same-origin. A client
// that is not a browser sends no Origin and passes here; it still needs the
// secret.
func originOK(origin, host string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == host
}

// securityHeaders go on every response. The CSP allows nothing but this
// server's own files: no inline script, no inline style attribute, no
// framing — the page holds database results, and nothing of it needs more.
func securityHeaders(ctx rweb.Context) {
	h := ctx.Response()
	h.SetHeader("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.SetHeader("X-Content-Type-Options", "nosniff")
	h.SetHeader("Referrer-Policy", "no-referrer")
	h.SetHeader("X-Frame-Options", "DENY")
}

func plain(ctx rweb.Context, status int, msg string) error {
	ctx.SetStatus(status)
	return ctx.WriteText(msg + "\n")
}
