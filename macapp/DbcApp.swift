// DbcApp — `dbc web` in a macOS window of its own.
//
// mac-install.sh compiles this file into dbc.app/Contents/MacOS/dbc-app and
// puts the dbc binary beside it in Contents/Helpers. The app owns one
// `dbc web` process for as long as it runs:
//
//	dbc-app                                         dbc web (Contents/Helpers/dbc)
//	───────────────────────────────────────────     ──────────────────────────────
//	1. read the login shell's environment
//	2. pick a free loopback port and a secret
//	3. spawn ─────────────────────────────────────► web --no-open --exit-on-eof
//	     env DBC_WEB_SECRET=<secret>                    --listen 127.0.0.1:<port>
//	     stdin = pipe (the app keeps the write end)
//	     stdout+stderr → ~/Library/Logs/dbc/dbc-web.log
//	4. poll GET /api/v1/health ─────────────────────► 200 once listening
//	5. load /login?s=<secret> ──────────────────────► cookie, 303 → /
//	6. Quit: SIGTERM ───────────────────────────────► cancel runs, release sessions
//	   crash / Force Quit: the kernel closes the pipe ► EOF → the same shutdown
//
// Why these choices:
//
//   - A port and secret of the app's own, never an already-running dbc web.
//     A running server's secret is unknown to the app, so it could not sign
//     in; and a terminal `dbc web` on 8450 goes on working beside the app.
//     The secret travels in the environment, not argv, so `ps` does not show
//     it. Only sessionStorage is used by the page, so a new port per launch
//     loses nothing.
//   - The login shell's environment. An app started from Finder or the Dock
//     gets launchd's bare PATH and none of the variables set in ~/.zshrc,
//     but the assistant's agents are npm binaries (node on PATH), a DSN may
//     say ${PGPASS}, and an agent may need ANTHROPIC_API_KEY. Terminal
//     editors' GUI builds solve this the same way (`$SHELL -ilc env`).
//   - The stdin pipe (`--exit-on-eof`, webcmd.go). A signal needs the app
//     alive to send it; a pipe's EOF does not, so a crashed app leaves no
//     orphaned server holding web.bytdb and database sessions.
//   - A non-persistent website data store. The session cookie is per
//     launch, tabs and layout live in the server's web.bytdb, and the page
//     keeps nothing else in the browser, so nothing is worth writing to
//     disk.

import AppKit
import Darwin
import WebKit

// MARK: - Paths and names

enum Paths {
    static let home = FileManager.default.homeDirectoryForCurrentUser
    static let logDir = home.appendingPathComponent("Library/Logs/dbc", isDirectory: true)
    static let log = logDir.appendingPathComponent("dbc-web.log")
    static let downloads = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first
        ?? home.appendingPathComponent("Downloads", isDirectory: true)
    // Contents/Helpers rather than Resources: codesign treats Helpers as
    // nested code and signs it as such, where a Mach-O in Resources breaks
    // the bundle's seal.
    static let helper = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/dbc")
}

/// The app's display name: CFBundleName, so an install with
/// DBC_APP_NAME=something shows that name in its menus and alerts.
let appName = Bundle.main.object(forInfoDictionaryKey: "CFBundleName") as? String ?? "dbc"

/// dbc's theme background (theme.Bg), for the window behind the page while
/// it loads — the page itself is dark by default, so white would flash.
let themeBg = NSColor(srgbRed: 0x1f / 255.0, green: 0x24 / 255.0, blue: 0x20 / 255.0, alpha: 1)

// MARK: - The login shell's environment

/// loginEnvironment runs the user's shell as an interactive login shell and
/// returns the environment it ends up with, or nil when that fails or takes
/// longer than `timeout` (a ~/.zshrc that waits on something).
///
/// The output is framed by a marker because an interactive shell may print
/// anything first (a banner, a fortune, a warning); `env -0` separates
/// entries with NUL, so a value holding a newline survives. Reading runs as
/// the output arrives rather than after exit: a large environment would
/// otherwise fill the pipe and block the shell. The wait is on the shell's
/// exit, not on EOF, because a daemon it starts (an ssh-agent, say) may hold
/// stdout open long after.
func loginEnvironment(timeout: TimeInterval = 10) -> [String: String]? {
    let marker = "__DBC_ENV_\(UUID().uuidString)__"
    let shell = userShell()
    let p = Process()
    p.executableURL = URL(fileURLWithPath: shell)
    // printf and /usr/bin/env mean the same in sh, bash, zsh and fish
    p.arguments = ["-i", "-l", "-c", "printf '%s' '\(marker)'; /usr/bin/env -0"]
    p.currentDirectoryURL = Paths.home
    p.standardInput = FileHandle.nullDevice
    p.standardError = FileHandle.nullDevice
    let out = Pipe()
    p.standardOutput = out

    let lock = NSLock()
    var data = Data()
    out.fileHandleForReading.readabilityHandler = { h in
        let chunk = h.availableData
        lock.lock(); data.append(chunk); lock.unlock()
    }
    let exited = DispatchSemaphore(value: 0)
    p.terminationHandler = { _ in exited.signal() }
    do {
        try p.run()
    } catch {
        out.fileHandleForReading.readabilityHandler = nil
        return nil
    }
    if exited.wait(timeout: .now() + timeout) == .timedOut {
        p.terminate()
        out.fileHandleForReading.readabilityHandler = nil
        return nil
    }
    // what the shell wrote just before exiting may still be in the pipe
    Thread.sleep(forTimeInterval: 0.05)
    out.fileHandleForReading.readabilityHandler = nil
    lock.lock(); let got = data; lock.unlock()

    guard p.terminationStatus == 0,
          let at = got.range(of: Data(marker.utf8), options: .backwards) else { return nil }
    var env: [String: String] = [:]
    for entry in got[at.upperBound...].split(separator: 0) {
        let s = String(decoding: entry, as: UTF8.self)
        guard let eq = s.firstIndex(of: "="), eq != s.startIndex else { continue }
        env[String(s[..<eq])] = String(s[s.index(after: eq)...])
    }
    return env.isEmpty ? nil : env
}

/// userShell is the login shell from the user database — what Terminal
/// starts — falling back to $SHELL and then zsh, macOS's default.
func userShell() -> String {
    if let pw = getpwuid(getuid()), let sh = pw.pointee.pw_shell {
        let s = String(cString: sh)
        if !s.isEmpty { return s }
    }
    return ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
}

/// serverEnvironment is what dbc web runs with: the app's own environment,
/// overlaid with the login shell's. Variables that describe a terminal
/// session are dropped from both — they would be wrong in a process that is
/// not one, and `open` run in a terminal hands the app that terminal's (its
/// PWD, its TERM) — and so is the cats host's (CATS_*): dbc would report to
/// a pane it is not running in.
func serverEnvironment(secret: String) -> [String: String] {
    var env = ProcessInfo.processInfo.environment
    if let login = loginEnvironment() {
        env.merge(login) { _, shell in shell }
    } else {
        // no shell environment: at least find Homebrew's and npm's usual homes
        let path = env["PATH"] ?? "/usr/bin:/bin:/usr/sbin:/sbin"
        env["PATH"] = "/opt/homebrew/bin:/usr/local/bin:" + path
    }
    let session: Set<String> = ["PWD", "OLDPWD", "SHLVL", "_", "TERM", "TERM_PROGRAM",
                                "TERM_PROGRAM_VERSION", "TERM_SESSION_ID", "COLORTERM"]
    for k in env.keys where session.contains(k) || k.hasPrefix("CATS_") { env[k] = nil }
    env["DBC_WEB_SECRET"] = secret
    return env
}

// MARK: - Port and secret

/// freeLoopbackPort asks the kernel for an unused port by binding
/// 127.0.0.1:0 and reading back what it chose. The socket is closed before
/// dbc binds the port again; the gap is a few milliseconds, and should
/// another process take the port in it, dbc fails to listen, exits, and the
/// app says so (see WebServer.onExit).
func freeLoopbackPort() -> UInt16? {
    let fd = socket(AF_INET, SOCK_STREAM, 0)
    guard fd >= 0 else { return nil }
    defer { close(fd) }
    var addr = sockaddr_in()
    addr.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
    addr.sin_family = sa_family_t(AF_INET)
    addr.sin_addr.s_addr = inet_addr("127.0.0.1")
    addr.sin_port = 0
    var len = socklen_t(MemoryLayout<sockaddr_in>.size)
    let bound = withUnsafeMutablePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, len) == 0 && getsockname(fd, $0, &len) == 0 }
    }
    return bound ? UInt16(bigEndian: addr.sin_port) : nil
}

/// makeSecret returns 32 URL-safe characters (192 bits).
/// SystemRandomNumberGenerator is the kernel's CSPRNG on Apple platforms,
/// and 64 divides 2^64, so `% 64` picks every character with equal odds.
func makeSecret() -> String {
    let alphabet = Array("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_")
    var rng = SystemRandomNumberGenerator()
    return String((0..<32).map { _ in alphabet[Int(rng.next() % 64)] })
}

// MARK: - The dbc web process

final class WebServer {
    let port: UInt16
    let secret: String
    var baseURL: URL { URL(string: "http://127.0.0.1:\(port)")! }
    var loginURL: URL { URL(string: "http://127.0.0.1:\(port)/login?s=\(secret)")! }

    /// onExit is called on the main queue when dbc web exits, for any reason.
    var onExit: ((Int32) -> Void)?
    private(set) var process: Process?
    /// The write end of dbc web's stdin. Held, never written: the app's exit
    /// closes it, and dbc web stops on the EOF (--exit-on-eof).
    private var lifeline: Pipe?

    init(port: UInt16, secret: String) {
        self.port = port
        self.secret = secret
    }

    var isRunning: Bool { process?.isRunning ?? false }

    func start(environment: [String: String]) throws {
        let log = try openLog()
        let p = Process()
        p.executableURL = Paths.helper
        p.arguments = ["web", "--no-open", "--exit-on-eof", "--listen", "127.0.0.1:\(port)"]
        p.environment = environment
        // As if `dbc web` were typed in a new Terminal window: ./dbc.toml is
        // looked up here before ~/.config/dbc/config.toml.
        p.currentDirectoryURL = Paths.home
        let pipe = Pipe()
        p.standardInput = pipe
        p.standardOutput = log
        p.standardError = log
        p.terminationHandler = { [weak self] proc in
            DispatchQueue.main.async { self?.onExit?(proc.terminationStatus) }
        }
        try p.run()
        try? log.close() // the child has its own copy
        process = p
        lifeline = pipe
    }

    /// stop asks dbc web to shut down as Ctrl+C would; onExit follows.
    func stop() {
        guard let p = process, p.isRunning else { return }
        p.terminate()
    }

    /// openLog starts a fresh log for this launch and keeps the previous
    /// one as dbc-web.log.1: enough to read what went wrong last time,
    /// without a file that grows forever. The log holds this launch's login
    /// link (dbc web prints it), so it is readable by the user alone.
    private func openLog() throws -> FileHandle {
        let fm = FileManager.default
        try fm.createDirectory(at: Paths.logDir, withIntermediateDirectories: true)
        let prev = Paths.log.appendingPathExtension("1")
        if fm.fileExists(atPath: Paths.log.path) {
            try? fm.removeItem(at: prev)
            try? fm.moveItem(at: Paths.log, to: prev)
        }
        guard fm.createFile(atPath: Paths.log.path, contents: nil, attributes: [.posixPermissions: 0o600]) else {
            throw NSError(domain: "dbc", code: 1, userInfo: [NSLocalizedDescriptionKey: "cannot create \(Paths.log.path)"])
        }
        return try FileHandle(forWritingTo: Paths.log)
    }

    /// waitUntilHealthy polls /api/v1/health — the one public endpoint — until
    /// it answers 200, the process exits, or the time is up. dbc opens its
    /// demo databases before it listens, so the first second or so is spent
    /// there.
    func waitUntilHealthy(timeout: TimeInterval) async -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        var req = URLRequest(url: baseURL.appendingPathComponent("api/v1/health"))
        req.timeoutInterval = 0.5
        while Date() < deadline && isRunning {
            if let (_, resp) = try? await URLSession.shared.data(for: req),
               (resp as? HTTPURLResponse)?.statusCode == 200 {
                return true
            }
            try? await Task.sleep(nanoseconds: 150_000_000)
        }
        return false
    }

    /// owns reports whether url is this server's, so it stays in the app;
    /// anything else goes to the default browser.
    func owns(_ url: URL) -> Bool {
        guard let scheme = url.scheme?.lowercased() else { return false }
        switch scheme {
        case "about", "blob", "data":
            return true // the page's own blank windows and object URLs
        case "http":
            return (url.host == "127.0.0.1" || url.host == "localhost") && url.port == Int(port)
        default:
            return false
        }
    }
}

// MARK: - Windows

/// BrowserWindow is one window holding a WKWebView: the workbench, or a
/// window the page opened itself (the plan's "open page"). All of them share
/// one WKWebViewConfiguration, so they share the session cookie.
///
/// It is the window's controller, which puts it in the responder chain
/// behind the web view: the View menu's zoom items reach the key window's
/// page without the menu knowing which window that is.
final class BrowserWindow: NSWindowController, NSWindowDelegate, WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate {
    let webView: WKWebView
    let isMain: Bool
    private unowned let app: AppDelegate
    private var titleWatch: NSKeyValueObservation?
    private var status: NSTextField?
    /// where each download in flight is going, for the Downloads-stack bounce
    private var downloads: [ObjectIdentifier: URL] = [:]

    init(app: AppDelegate, configuration: WKWebViewConfiguration, isMain: Bool) {
        self.app = app
        self.isMain = isMain
        webView = WKWebView(frame: .zero, configuration: configuration)
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: isMain ? 1400 : 1100, height: isMain ? 900 : 760),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered, defer: false)
        super.init(window: window)

        window.title = appName
        window.backgroundColor = themeBg
        window.isReleasedWhenClosed = false
        window.delegate = self
        window.tabbingMode = .disallowed
        window.contentView = webView
        if isMain {
            window.setFrameAutosaveName("dbc.workbench")
            if !window.setFrameUsingName("dbc.workbench") { window.center() }
        } else {
            window.cascadeTopLeft(from: NSApp.keyWindow?.frame.origin ?? .zero)
        }

        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.allowsMagnification = true
        if #available(macOS 13.3, *) {
            webView.isInspectable = true // Safari's Develop menu can attach
        }
        titleWatch = webView.observe(\.title, options: [.new]) { [weak self] wv, _ in
            guard let t = wv.title, !t.isEmpty else { return }
            self?.window?.title = t
        }
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) { fatalError("not from a nib") }

    // MARK: status text, while the server starts

    /// showStatus puts a line of text where the page will be, until the
    /// first page has loaded (the web view stays hidden until then, so a
    /// white web view does not flash over the dark window).
    func showStatus(_ text: String) {
        guard let content = window?.contentView else { return }
        if status == nil {
            let label = NSTextField(labelWithString: "")
            label.font = .monospacedSystemFont(ofSize: 13, weight: .regular)
            label.textColor = NSColor(srgbRed: 0x4d / 255.0, green: 0xb3 / 255.0, blue: 0x80 / 255.0, alpha: 1)
            label.translatesAutoresizingMaskIntoConstraints = false
            content.addSubview(label)
            NSLayoutConstraint.activate([
                label.centerXAnchor.constraint(equalTo: content.centerXAnchor),
                label.centerYAnchor.constraint(equalTo: content.centerYAnchor),
            ])
            status = label
            webView.isHidden = true
        }
        status?.stringValue = text
    }

    // MARK: NSWindowDelegate

    func windowWillClose(_ notification: Notification) {
        if isMain {
            // The workbench is the app: with it gone, a plan window left
            // open would be all there is, with no road back.
            NSApp.terminate(nil)
        } else {
            app.forget(self)
        }
    }

    // MARK: WKNavigationDelegate

    func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if navigationAction.shouldPerformDownload {
            decisionHandler(.download) // <a download>: the grid's exports, the plan's files
            return
        }
        guard let url = navigationAction.request.url, let server = app.server, !server.owns(url) else {
            decisionHandler(.allow)
            return
        }
        // a link off the workbench (docs, GitHub's sign-in page for the
        // assistant): the browser, not a window here
        NSWorkspace.shared.open(url)
        decisionHandler(.cancel)
    }

    func webView(_ webView: WKWebView, decidePolicyFor navigationResponse: WKNavigationResponse,
                 decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        // dbc marks what is meant to be saved (?download=1, the exports) as
        // an attachment; a browser saves it even when it could show it.
        if let http = navigationResponse.response as? HTTPURLResponse,
           let cd = http.value(forHTTPHeaderField: "Content-Disposition"),
           cd.lowercased().hasPrefix("attachment") {
            decisionHandler(.download)
            return
        }
        decisionHandler(navigationResponse.canShowMIMEType ? .allow : .download)
    }

    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        if let s = status {
            s.removeFromSuperview()
            status = nil
            webView.isHidden = false
        }
    }

    /// The page's process died (out of memory, a WebKit bug): load it again.
    /// The cookie is kept by the data store, so the reload is still signed
    /// in, and the page reattaches to its tabs as after any reload.
    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        webView.reload()
    }

    // MARK: WKUIDelegate

    /// window.open: the plan's standalone page opens in a window here, as it
    /// needs the session cookie; anywhere else goes to the browser.
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = navigationAction.request.url, let server = app.server, !server.owns(url) {
            NSWorkspace.shared.open(url)
            return nil
        }
        // WebKit loads the request into the returned view itself
        return app.openWindow(configuration: configuration).webView
    }

    func webViewDidClose(_ webView: WKWebView) {
        if !isMain { window?.close() }
    }

    // dbc's page asks with its own dialogs; these cover anything that still
    // calls alert() or confirm(), which WebKit otherwise answers silently.
    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let a = NSAlert()
        a.messageText = message
        guard let w = window else { a.runModal(); completionHandler(); return }
        a.beginSheetModal(for: w) { _ in completionHandler() }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.addButton(withTitle: "OK")
        a.addButton(withTitle: "Cancel")
        guard let w = window else { completionHandler(a.runModal() == .alertFirstButtonReturn); return }
        a.beginSheetModal(for: w) { completionHandler($0 == .alertFirstButtonReturn) }
    }

    // MARK: WKDownloadDelegate — saved to ~/Downloads, as a browser would

    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse,
                  suggestedFilename: String, completionHandler: @escaping (URL?) -> Void) {
        let dest = uniqueDownloadURL(suggestedFilename)
        downloads[ObjectIdentifier(download)] = dest
        completionHandler(dest)
    }

    func downloadDidFinish(_ download: WKDownload) {
        guard let dest = downloads.removeValue(forKey: ObjectIdentifier(download)) else { return }
        // the Dock's Downloads stack bounces, as it does for Safari's
        DistributedNotificationCenter.default().post(
            name: Notification.Name("com.apple.DownloadFileFinished"), object: dest.path)
    }

    func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
        downloads.removeValue(forKey: ObjectIdentifier(download))
        NSSound.beep()
        FileHandle.standardError.write(Data("download failed: \(error.localizedDescription)\n".utf8))
    }

    /// uniqueDownloadURL numbers a name that is taken the way Safari does:
    /// "plan.pdf", then "plan-2.pdf", "plan-3.pdf", …
    private func uniqueDownloadURL(_ suggested: String) -> URL {
        let name = suggested.isEmpty ? "download" : (suggested as NSString).lastPathComponent
        let base = (name as NSString).deletingPathExtension
        let ext = (name as NSString).pathExtension
        var url = Paths.downloads.appendingPathComponent(name)
        var n = 2
        while FileManager.default.fileExists(atPath: url.path) {
            url = Paths.downloads.appendingPathComponent(ext.isEmpty ? "\(base)-\(n)" : "\(base)-\(n).\(ext)")
            n += 1
        }
        return url
    }

    // MARK: View menu actions (reached through the responder chain)

    @objc func dbcActualSize(_ sender: Any?) { webView.pageZoom = 1 }
    @objc func dbcZoomIn(_ sender: Any?) { webView.pageZoom = min(webView.pageZoom + 0.1, 3) }
    @objc func dbcZoomOut(_ sender: Any?) { webView.pageZoom = max(webView.pageZoom - 0.1, 0.5) }
}

// MARK: - The app

final class AppDelegate: NSObject, NSApplicationDelegate {
    private(set) var server: WebServer?
    private var main: BrowserWindow!
    private var popups: [BrowserWindow] = []
    private let config: WKWebViewConfiguration = {
        let c = WKWebViewConfiguration()
        c.websiteDataStore = .nonPersistent()
        return c
    }()
    private var quitting = false
    private var failed = false

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.regular)
        applyAppIcon()
        buildMenu()
        main = BrowserWindow(app: self, configuration: config, isMain: true)
        main.showStatus("starting dbc web…")
        main.showWindow(nil)
        NSApp.activate(ignoringOtherApps: true)

        guard FileManager.default.isExecutableFile(atPath: Paths.helper.path) else {
            fail("The dbc binary is missing from the app (\(Paths.helper.path)). Run mac-install.sh again.")
            return
        }
        guard let port = freeLoopbackPort() else {
            fail("No free port on 127.0.0.1 to serve dbc web on.")
            return
        }
        let server = WebServer(port: port, secret: makeSecret())
        server.onExit = { [weak self] status in self?.serverExited(status) }
        self.server = server

        Task { @MainActor in
            // the shell can take a second or two: off the main thread
            let env = await Task.detached { serverEnvironment(secret: server.secret) }.value
            do {
                try server.start(environment: env)
            } catch {
                fail("dbc web could not start: \(error.localizedDescription)")
                return
            }
            if await server.waitUntilHealthy(timeout: 30) {
                main.webView.load(URLRequest(url: server.loginURL))
            } else if server.isRunning {
                server.stop()
                fail("dbc web did not answer within 30 seconds.")
            } // else it exited, and serverExited has said so
        }
    }

    // MARK: quitting

    /// Quit waits for dbc web to finish its shutdown (runs canceled,
    /// sessions released and rolled back — up to 5s, web.Shutdown's grace),
    /// so an app opened again right away finds web.bytdb free.
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard let server, server.isRunning else { return .terminateNow }
        quitting = true
        var replied = false
        let reply = {
            if replied { return }
            replied = true
            NSApp.reply(toApplicationShouldTerminate: true)
        }
        server.onExit = { _ in reply() }
        server.stop()
        // a server stuck past its own grace should not hold the app hostage
        DispatchQueue.main.asyncAfter(deadline: .now() + 8, execute: reply)
        return .terminateLater
    }

    private func serverExited(_ status: Int32) {
        if quitting { return }
        fail("dbc web stopped (exit status \(status)).")
    }

    /// fail tells the user once, offering the log, and quits: without its
    /// server the window has nothing to show.
    private func fail(_ message: String) {
        if failed || quitting { return }
        failed = true
        let a = NSAlert()
        a.alertStyle = .critical
        a.messageText = "\(appName) could not run dbc web"
        a.informativeText = message + "\n\nThe log is \(Paths.log.path)."
        a.addButton(withTitle: "Quit")
        a.addButton(withTitle: "Show Log")
        if a.runModal() == .alertSecondButtonReturn { showLog(nil) }
        server?.stop()
        quitting = true
        NSApp.terminate(nil)
    }

    // MARK: windows

    func openWindow(configuration: WKWebViewConfiguration) -> BrowserWindow {
        let w = BrowserWindow(app: self, configuration: configuration, isMain: false)
        popups.append(w)
        w.showWindow(nil)
        return w
    }

    func forget(_ w: BrowserWindow) {
        popups.removeAll { $0 === w }
    }

    // MARK: menu

    // Key equivalents are chosen around the page's own: ⌘R runs, ⌘K stops,
    // ⌘P is history, ⌘E export, ⌘B the sidebar, ⌘I the assistant, ⌘O
    // scripts. The web view sees a key before the menu does, and the page
    // takes those, so a menu item on them would only ever fire when the page
    // let one through — Reload therefore has none.
    private func buildMenu() {
        let bar = NSMenu()

        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "About \(appName)",
                        action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Hide \(appName)", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(withTitle: "Hide Others", action: #selector(NSApplication.hideOtherApplications(_:)),
                        keyEquivalent: "h").keyEquivalentModifierMask = [.command, .option]
        appMenu.addItem(withTitle: "Show All", action: #selector(NSApplication.unhideAllApplications(_:)),
                        keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Quit \(appName)", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        bar.addItem(submenu(appMenu))

        // without these, ⌘C/⌘V/⌘A do nothing in a web view outside Monaco
        let edit = NSMenu(title: "Edit")
        edit.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
        edit.addItem(withTitle: "Redo", action: Selector(("redo:")),
                     keyEquivalent: "z").keyEquivalentModifierMask = [.command, .shift]
        edit.addItem(.separator())
        edit.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        bar.addItem(submenu(edit))

        let view = NSMenu(title: "View")
        view.addItem(withTitle: "Reload Page", action: #selector(WKWebView.reload(_:)), keyEquivalent: "")
        view.addItem(.separator())
        view.addItem(withTitle: "Actual Size", action: #selector(BrowserWindow.dbcActualSize(_:)), keyEquivalent: "0")
        view.addItem(withTitle: "Zoom In", action: #selector(BrowserWindow.dbcZoomIn(_:)), keyEquivalent: "=")
        view.addItem(withTitle: "Zoom Out", action: #selector(BrowserWindow.dbcZoomOut(_:)), keyEquivalent: "-")
        view.addItem(.separator())
        view.addItem(withTitle: "Enter Full Screen", action: #selector(NSWindow.toggleFullScreen(_:)),
                     keyEquivalent: "f").keyEquivalentModifierMask = [.command, .control]
        view.addItem(.separator())
        let browser = view.addItem(withTitle: "Open in Browser", action: #selector(openInBrowser(_:)), keyEquivalent: "")
        browser.target = self
        let log = view.addItem(withTitle: "Show Log", action: #selector(showLog(_:)), keyEquivalent: "")
        log.target = self
        bar.addItem(submenu(view))

        let win = NSMenu(title: "Window")
        win.addItem(withTitle: "Minimize", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        win.addItem(withTitle: "Zoom", action: #selector(NSWindow.performZoom(_:)), keyEquivalent: "")
        win.addItem(.separator())
        win.addItem(withTitle: "Close Window", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        bar.addItem(submenu(win))
        NSApp.windowsMenu = win

        NSApp.mainMenu = bar
    }

    private func submenu(_ m: NSMenu) -> NSMenuItem {
        let item = NSMenuItem()
        item.submenu = m
        return item
    }

    /// Open in Browser signs the default browser in with this launch's
    /// secret, as `dbc web` itself would — the same server, a second window
    /// onto it.
    @objc func openInBrowser(_ sender: Any?) {
        guard let server, server.isRunning else { NSSound.beep(); return }
        NSWorkspace.shared.open(server.loginURL)
    }

    @objc func showLog(_ sender: Any?) {
        NSWorkspace.shared.open(Paths.log) // Console, for a .log
    }

    // The Dock and ⌘-Tab draw the running app's applicationIconImage, which
    // macOS caches by bundle id: after a reinstall they can keep an old or
    // generic icon that Finder has already replaced. Setting it at launch
    // sidesteps that cache.
    private func applyAppIcon() {
        guard let url = Bundle.main.url(forResource: "dbc", withExtension: "icns"),
              let icon = NSImage(contentsOf: url) else { return }
        NSApp.applicationIconImage = icon
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.run()
