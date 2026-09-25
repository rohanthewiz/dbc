// Package web is `dbc web`: the workbench in a browser.
//
// It is a local tool for the person who started it. The server listens on
// loopback, opens the browser on a one-time login URL, and serves a page
// that runs statements through the same workspace package the TUI uses — so
// the run slot, the pinned session, cancel and history behave identically in
// both.
//
//	browser tab ──POST /api/v1/ws/:id/run──► handler ──► workspace.RunEditor
//	     ▲                                        │  refused? → 409 / 400 at once
//	     │                                        ▼
//	     │                                  go Job() ── runs, lands the outcome
//	     └──── SSE /api/v1/ws/:id/events ◄──── deliver: "run" {status, hasResult}
//	                   │
//	                   └─► GET /api/v1/ws/:id/result → the table, rendered here
//
// Built the way the author's other rweb apps are (gonotes, herdr-web): rweb
// for HTTP and SSE, element for server-side HTML, vanilla JS and CSS embedded
// with go:embed, serr for errors, nothing fetched from the network.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/theme"
	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/web/pages"
	"github.com/rohanthewiz/dbc/workspace"
)

// DefaultListen is where dbc web listens unless told otherwise: loopback,
// and a fixed port so a bookmark survives a restart (clear of gonotes' 8444
// and herdr-web's 8421). When it is taken, a free port is used instead.
const DefaultListen = "127.0.0.1:8450"

// memPoolTabs is the in-memory SQLite pool cap dbc web asks for: the anchor,
// a pinned session per tab for up to 14 tabs, and one for the pool. The
// TUI's cap of 3 assumes one session; past this many tabs, a run waits for
// a connection (cancelable), it does not fail.
const memPoolTabs = 16

// Options configure a Server.
type Options struct {
	// Listen is the address to bind. "" means DefaultListen, falling back
	// to a free loopback port when 8450 is taken; an explicit address is
	// used as given, and a busy one is an error.
	Listen string
	// Secret is the launch secret; "" generates one.
	Secret string
	// Store keeps tabs and layout; nil means a memory-only store.
	Store *Store
	// History is the query history every tab records into — the TUI's
	// file, so a query run in either shows up in both. nil keeps it in
	// memory.
	History *userdata.History
	// Ready, when set, is called once the server is listening, with the
	// login URL (the secret in it) — to print it and open a browser.
	Ready func(loginURL string)
	// Logf receives the server's own messages (shutdown progress). nil
	// discards them.
	Logf func(format string, args ...any)

	// ChatsDir is where assistant conversations are kept — the TUI's
	// archive (userdata.ChatsDir), so a conversation had in either is
	// offered in both. "" keeps them in memory only.
	ChatsDir string
	// StartChat starts an assistant conversation; nil is ai.Start. Tests
	// hand in a scripted agent (package aitest): the real one would need
	// installing and signing in, and would spend Copilot requests.
	StartChat func(ai.Agent, ai.Options) *ai.Chat
	// BeginSignIn starts the agent's device-flow sign-in; nil is
	// ai.BeginSignIn. Tests hand in a scripted server for the same reason.
	BeginSignIn func(ai.Agent, string) (*ai.SignIn, error)
}

// Server is a running (or ready-to-run) dbc web.
type Server struct {
	cfg   *config.Config
	mgr   *db.Manager
	opt   Options
	store *Store
	auth  *auth
	hub   *hub
	rw    *rweb.Server
	ready chan struct{}
	ver   string // asset version for cache busting
}

//go:embed all:static
var staticFiles embed.FS

// New builds the server over the app's config and connection manager. It
// does no network IO; Run listens.
func New(cfg *config.Config, mgr *db.Manager, opt Options) (*Server, error) {
	if opt.Secret == "" {
		sec, err := GenerateSecret()
		if err != nil {
			return nil, err
		}
		opt.Secret = sec
	}
	a, err := newAuth(opt.Secret)
	if err != nil {
		return nil, err
	}
	if opt.Store == nil {
		opt.Store, _ = OpenStore("") // memory only; cannot fail
	}
	if opt.History == nil {
		opt.History = userdata.LoadHistory("")
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	if opt.StartChat == nil {
		opt.StartChat = ai.Start
	}
	if opt.BeginSignIn == nil {
		opt.BeginSignIn = ai.BeginSignIn
	}
	addr, err := resolveListen(opt.Listen)
	if err != nil {
		return nil, err
	}
	mgr.SetMemoryPool(memPoolTabs)

	s := &Server{cfg: cfg, mgr: mgr, opt: opt, store: opt.Store, auth: a,
		ready: make(chan struct{}, 1), ver: assetVersion()}
	s.hub = newHub(func(sink func(workspace.Event)) *workspace.Workspace {
		return workspace.New(cfg, mgr, opt.History, workspace.Options{Sink: sink})
	}, cfg.ConnIdleTimeout)
	s.hub.newChat = func(w *window) *assistant { return newAssistant(s, w) }
	s.rw = rweb.NewServer(rweb.ServerOptions{Address: addr, ReadyChan: s.ready})
	s.rw.Use(s.guard)
	s.routes()
	// after the file's connections (Load) and the demos (db.openDemos), so a
	// saved one never displaces either; see mergeSavedConns. Its warnings go
	// to the terminal and join the config's, which a new window's page logs
	// (handleOpen) — appending is safe here, as nothing is serving yet.
	for _, w := range s.mergeSavedConns() {
		opt.Logf("warning: %s", w)
		cfg.Warnings = append(cfg.Warnings, w)
	}
	return s, nil
}

// resolveListen picks the address to bind. Only the default falls back: an
// address the user typed is theirs to fix if it is busy, and silently
// moving it would break whatever expected it there.
func resolveListen(listen string) (string, error) {
	if listen != "" {
		if _, _, err := net.SplitHostPort(listen); err != nil {
			return "", serr.Wrap(err, "listen", listen)
		}
		return listen, nil
	}
	// probe the default port: bind, then let go. The window before rweb
	// binds it again is a few microseconds on an otherwise idle port.
	l, err := net.Listen("tcp", DefaultListen)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return "127.0.0.1:0", nil
		}
		return "", serr.Wrap(err, "listen", DefaultListen)
	}
	_ = l.Close()
	return DefaultListen, nil
}

// routes registers every route; guard (auth.go) runs before all of them.
// Literal paths come before :id ones, as rweb matches a literal first anyway
// and the list reads better that way.
func (s *Server) routes() {
	r := s.rw
	r.Get("/", s.handlePage)
	r.Get("/login", s.login)
	r.Get("/favicon.ico", handleFavicon)
	r.Get("/theme.css", handleTheme)
	r.Get("/static/*", handleStatic)

	r.Get("/api/v1/health", s.handleHealth)
	r.Get("/api/v1/conns", s.handleConns)
	r.Post("/api/v1/conns", s.handleConnAdd)
	r.Post("/api/v1/conns/test", s.handleConnTest)
	r.Delete("/api/v1/conns/:name", s.handleConnDelete)
	r.Get("/api/v1/tabs", s.handleTabs)
	r.Put("/api/v1/tabs/:id", s.handleSaveTab)
	r.Delete("/api/v1/tabs/:id", s.handleDeleteTab)
	r.Get("/api/v1/layout", s.handleLayout)
	r.Put("/api/v1/layout", s.handleSaveLayout)
	r.Get("/api/v1/history", s.handleHistory)
	r.Post("/api/v1/stmt", s.handleStmt)
	r.Get("/api/v1/chats", s.handleChats)
	r.Delete("/api/v1/chats/:id", s.handleChatDelete)
	r.Get("/api/v1/scripts", s.handleScripts)

	r.Get("/api/v1/win/:id", s.handleWindow)
	r.Get("/api/v1/win/:id/events", s.handleWindowEvents)
	r.Post("/api/v1/win/:id/tabs", s.handleClaim)
	r.Post("/api/v1/win/:id/release", s.handleRelease)

	r.Post("/api/v1/ws", s.handleOpen)
	r.Get("/api/v1/ws/:id", s.handleState)
	r.Delete("/api/v1/ws/:id", s.handleClose)
	r.Get("/api/v1/ws/:id/events", s.handleEvents)
	r.Get("/api/v1/ws/:id/result", s.handleResult)
	r.Get("/api/v1/ws/:id/export", s.handleExport)
	r.Post("/api/v1/ws/:id/copy", s.handleCopy)
	r.Post("/api/v1/ws/:id/connect", s.handleConnect)
	r.Post("/api/v1/ws/:id/run", s.handleRun)
	r.Post("/api/v1/ws/:id/preview", s.handlePreview)
	r.Post("/api/v1/ws/:id/explain", s.handleExplain)
	r.Get("/api/v1/ws/:id/plan", s.handlePlan)
	r.Get("/api/v1/ws/:id/plan/text", s.handlePlanText)
	r.Get("/api/v1/ws/:id/plan.html", s.handlePlanPage)
	r.Post("/api/v1/ws/:id/cancel", s.handleCancel)
	r.Post("/api/v1/ws/:id/script", s.handleScript)

	// the assistant pane (chat.go)
	r.Get("/api/v1/ws/:id/chat", s.handleChat)
	r.Post("/api/v1/ws/:id/chat/open", s.handleChatOpen)
	r.Post("/api/v1/ws/:id/chat/ask", s.handleChatAsk)
	r.Post("/api/v1/ws/:id/chat/context", s.handleChatContext)
	r.Post("/api/v1/ws/:id/chat/stop", s.handleChatStop)
	r.Post("/api/v1/ws/:id/chat/new", s.handleChatNew)
	r.Post("/api/v1/ws/:id/chat/model", s.handleChatModel)
	r.Post("/api/v1/ws/:id/chat/agent", s.handleChatAgent)
	r.Post("/api/v1/ws/:id/chat/load", s.handleChatLoad)
	r.Post("/api/v1/ws/:id/chat/delete", s.handleChatDeleteLive)
	r.Post("/api/v1/ws/:id/chat/signin", s.handleChatSignIn)
	r.Post("/api/v1/ws/:id/chat/signin/cancel", s.handleChatSignInCancel)
}

// Run listens and serves until Ctrl+C (rweb handles SIGINT and SIGTERM
// itself and returns), then shuts down: every tab's run is stopped and its
// session released — rolling back what it left open — within a grace period.
func (s *Server) Run() error {
	errc := make(chan error, 1)
	go func() { errc <- s.rw.Run() }()
	select {
	case <-s.ready:
	case err := <-errc:
		return serr.Wrap(err, "op", "listen")
	}
	reapCtx, stopReap := context.WithCancel(context.Background())
	defer stopReap()
	go s.reapLoop(reapCtx)
	if s.opt.Ready != nil {
		s.opt.Ready(s.LoginURL())
	}
	err := <-errc
	s.Shutdown()
	return err
}

// shutdownGrace bounds how long shutdown waits for statements to return
// from their cancel.
const shutdownGrace = 5 * time.Second

// Shutdown stops every tab's work and releases its session. The listener is
// rweb's to close (it already has, when Run calls this).
func (s *Server) Shutdown() {
	s.opt.Logf("stopping: canceling runs and releasing sessions…")
	if s.hub.closeAll(shutdownGrace) {
		s.opt.Logf("some sessions did not close within %s — exiting anyway", shutdownGrace)
	}
}

// URL is the server's base URL, once listening.
func (s *Server) URL() string {
	host, port, _ := net.SplitHostPort(s.rw.GetListenAddr())
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1" // bound everywhere: loopback reaches it too
	}
	return "http://" + net.JoinHostPort(host, port)
}

// LoginURL is the URL that signs a browser in: open it once.
func (s *Server) LoginURL() string { return s.URL() + "/login?s=" + string(s.auth.secret) }

// ---------------------------------------------------------------------------
// Pages and assets
// ---------------------------------------------------------------------------

// handlePage serves the workbench. The saved theme is rendered into the
// page rather than applied by script once the layout loads: a light-mode
// user would otherwise see a dark flash on every load, and the CSP allows
// no inline script to apply it earlier.
func (s *Server) handlePage(ctx rweb.Context) error {
	l, _ := s.store.Layout() // a store that cannot be read just means the defaults
	return writePage(ctx, http.StatusOK, pages.Workbench{
		Conns: s.cfg.Conns(), Active: s.defaultConn(), Ver: s.ver, Theme: l["theme"],
	}.Render())
}

// defaultConn is the connection a new workspace starts on — the one
// workspace.New picks.
func (s *Server) defaultConn() string {
	if _, ok := s.cfg.ConnByName(s.cfg.DefaultConnection); ok {
		return s.cfg.DefaultConnection
	}
	if conns := s.cfg.Conns(); len(conns) > 0 {
		return conns[0].Name
	}
	return ""
}

func writePage(ctx rweb.Context, status int, html string) error {
	ctx.SetStatus(status)
	ctx.Response().SetHeader("Cache-Control", "no-store")
	return ctx.WriteHTML(html)
}

// signInPage is the notice for a browser with no session.
func signInPage() string { return pages.SignIn(assetVersion()) }

// handleTheme serves the palettes as CSS variables — the theme package is
// the one source of truth for dbc's colors, in the terminal and here. Dark
// (the TUI's) is the root's; light applies under data-theme="light" on the
// root element, which the page's ◐ toggle sets and the layout remembers.
// color-scheme follows, so the browser's own scrollbars and form controls
// match.
func handleTheme(ctx rweb.Context) error {
	vars := func(p theme.Palette) string {
		return fmt.Sprintf("--bg:%s;--panel:%s;--panel2:%s;--sel:%s;--line:%s;"+
			"--fg:%s;--muted:%s;--accent:%s;--warn:%s;--err:%s",
			p.Bg, p.Panel, p.Panel2, p.Sel, p.Line, p.Fg, p.Muted, p.Accent, p.Warn, p.Err)
	}
	// The plan view keeps a toggle of its own (◐ in its header), so both
	// palettes are also scoped to it: in a light workbench, a plan flipped
	// to dark must get dark values, not inherit the root's light ones.
	css := ":root{" + vars(theme.Default()) + ";color-scheme:dark}\n" +
		`:root[data-theme="light"]{` + vars(theme.Light()) + ";color-scheme:light}\n" +
		`.dbc-plan[data-theme="dark"]{` + vars(theme.Default()) + "}\n" +
		`.dbc-plan[data-theme="light"]{` + vars(theme.Light()) + "}\n"
	ctx.Response().SetHeader("Content-Type", "text/css; charset=utf-8")
	ctx.Response().SetHeader("Cache-Control", "no-cache")
	return ctx.WriteString(css)
}

// favicon is dbc's mark in the theme's accent, inline so no file ships.
var favicon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">` +
	`<rect width="64" height="64" rx="12" fill="` + theme.Bg + `"/>` +
	`<text x="32" y="42" font-family="ui-monospace,monospace" font-weight="700" font-size="26" ` +
	`fill="` + theme.Accent + `" text-anchor="middle">dbc</text></svg>`

func handleFavicon(ctx rweb.Context) error {
	ctx.Response().SetHeader("Content-Type", "image/svg+xml")
	ctx.Response().SetHeader("Cache-Control", "public, max-age=86400")
	return ctx.WriteString(favicon)
}

// handleStatic serves the embedded assets. The pages ask for them with
// ?v=<hash of every asset>, so a new build is a new URL and the old files
// can be cached for good.
func handleStatic(ctx rweb.Context) error {
	name := strings.TrimPrefix(ctx.Request().Path(), "/static/")
	if name == "" || strings.Contains(name, "..") {
		return plain(ctx, http.StatusNotFound, "not found")
	}
	b, ok := sharedAssets[name]
	if !ok {
		var err error
		if b, err = fs.ReadFile(staticFiles, "static/"+name); err != nil {
			return plain(ctx, http.StatusNotFound, "not found")
		}
	}
	ctx.Response().SetHeader("Content-Type", contentType(name))
	if ctx.Request().QueryParam("v") != "" {
		ctx.Response().SetHeader("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		ctx.Response().SetHeader("Cache-Control", "no-cache")
	}
	return ctx.Bytes(b)
}

func contentType(name string) string {
	switch path.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".ttf":
		return "font/ttf" // Monaco's icon font
	}
	return "application/octet-stream"
}

// sharedAssets are static files that live in another package: the plan
// view, which the standalone plan page inlines and the Plan tab loads — one
// script for both (see explain/html.go).
var sharedAssets = map[string][]byte{
	"js/plan.js":   []byte(explain.PlanJS),
	"css/plan.css": []byte(explain.PlanCSS),
}

// assetVersion hashes every embedded asset: any change to any file busts
// every cached one, which for a handful of small files is simpler than
// tracking them one by one.
func assetVersion() string {
	h := sha256.New()
	for _, name := range []string{"js/plan.js", "css/plan.css"} {
		_, _ = io.WriteString(h, name)
		_, _ = h.Write(sharedAssets[name])
	}
	_ = fs.WalkDir(staticFiles, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, err := staticFiles.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, _ = io.WriteString(h, p)
		_, err = io.Copy(h, f)
		return err
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// ---------------------------------------------------------------------------
// The idle reaper
// ---------------------------------------------------------------------------

// reapEvery is how often idle tabs are checked; the idle rules are in
// minutes and hours, so a minute's granularity is plenty.
const reapEvery = time.Minute

// reapLoop runs the hub's idle rules until ctx ends.
func (s *Server) reapLoop(ctx context.Context) {
	t := time.NewTicker(reapEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.hub.reap(now)
		}
	}
}
