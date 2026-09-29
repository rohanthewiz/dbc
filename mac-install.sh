#!/usr/bin/env bash
#
# mac-install.sh — build dbc and install it as a macOS app (dbc.app) that
# runs `dbc web` in a window of its own instead of a browser tab.
#
# The app is macapp/DbcApp.swift, a small Swift/WebKit shell: it starts the
# bundled dbc as `dbc web` on a free loopback port with a secret of its own,
# polls /api/v1/health, and signs its window in. See that file's header.
#
# Where the source comes from:
#
#   ./mac-install.sh               inside a dbc checkout: builds that checkout,
#                                  as it is (uncommitted changes included)
#   curl -fsSL https://raw.githubusercontent.com/rohanthewiz/dbc/main/mac-install.sh | bash
#                                  clones main into ~/.dbc-src, or updates it
#
# WARNING: in the second form this script owns ~/.dbc-src and runs
# `git reset --hard` there on every update — do not keep edits in it. Your
# config, connections, history and tabs live in ~/.config/dbc either way, and
# the app shares them with the dbc on your PATH.
#
# Built here and signed ad hoc, never downloaded: the app is not a release
# artifact, so there is nothing to notarize and no second release pipeline
# to keep. Re-run the script to update it.
#
# Env overrides:
#   DBC_APP_SRC     source dir   (default: this checkout, else $HOME/.dbc-src)
#   DBC_APP_REPO    git remote   (default: https://github.com/rohanthewiz/dbc.git)
#   DBC_APP_BRANCH  branch       (default: main)
#   DBC_APP_GO_DIR  private Go   (default: $HOME/.local/go), fetched only when
#                                the Go on PATH is older than go.mod asks for
#   DBC_APP_DIR     install dir  (default: $HOME/Applications)
#   DBC_APP_NAME    app name     (default: dbc)
#
# (DBC_APP_* rather than DBC_*: dbc itself reads DBC_DEMO, DBC_WEB_SECRET, …)

set -euo pipefail

DBC_APP_REPO="${DBC_APP_REPO:-https://github.com/rohanthewiz/dbc.git}"
DBC_APP_BRANCH="${DBC_APP_BRANCH:-main}"
DBC_APP_GO_DIR="${DBC_APP_GO_DIR:-$HOME/.local/go}"
DBC_APP_DIR="${DBC_APP_DIR:-$HOME/Applications}"
DBC_APP_NAME="${DBC_APP_NAME:-dbc}"

BUNDLE_ID="com.github.rohanthewiz.dbc"

# ---- output helpers --------------------------------------------------------

if [ -t 1 ]; then
  C_BLUE=$'\033[34m'; C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'
  C_GREEN=$'\033[32m'; C_DIM=$'\033[2m'; C_RESET=$'\033[0m'
else
  C_BLUE=""; C_YELLOW=""; C_RED=""; C_GREEN=""; C_DIM=""; C_RESET=""
fi

info() { printf '%s==>%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf '%s ok%s %s\n'  "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%swarn%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
die()  { printf '%serror%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

# ---- platform and tools ----------------------------------------------------

detect_platform() {
  [ "$(uname -s)" = "Darwin" ] || die "mac-install.sh builds a macOS app; this is $(uname -s)"
  case "$(uname -m)" in
    x86_64|amd64)  ARCH="amd64";;
    aarch64|arm64) ARCH="arm64";;
    *) die "unsupported arch: $(uname -m) (need amd64 or arm64)";;
  esac
}

# swiftc, iconutil and codesign all come with the Xcode Command Line Tools.
# dbc itself is pure Go (CGO_ENABLED=0, as goreleaser builds it), so no C
# compiler is asked for.
require_tools() {
  command -v swiftc >/dev/null 2>&1 ||
    die "swiftc not found. Install the Xcode Command Line Tools: xcode-select --install"
}

# download URL FILE — curl, or wget when there is no curl.
download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 --retry-delay 2 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1"
  else
    die "neither curl nor wget found; cannot download $1"
  fi
}

# ---- source ----------------------------------------------------------------

# resolve_source sets SRC. A script run from a file inside a dbc checkout
# builds that checkout: that is a developer's tree, and touching its git state
# (or making them push first) would be rude. Piped through bash there is no
# file, so the script keeps a checkout of its own.
resolve_source() {
  if [ -n "${DBC_APP_SRC:-}" ]; then
    SRC="$DBC_APP_SRC"
    [ -f "$SRC/go.mod" ] || die "DBC_APP_SRC=$SRC holds no go.mod"
    SRC_KIND="given"
    return
  fi
  local here=""
  if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
    here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  fi
  if [ -n "$here" ] && grep -qx 'module github.com/rohanthewiz/dbc' "$here/go.mod" 2>/dev/null; then
    SRC="$here"
    SRC_KIND="checkout"
    return
  fi
  SRC="$HOME/.dbc-src"
  SRC_KIND="owned"
  sync_repo
}

sync_repo() {
  command -v git >/dev/null 2>&1 || die "git not found. Install with: xcode-select --install"
  if [ -d "$SRC/.git" ]; then
    info "updating $SRC from origin/$DBC_APP_BRANCH"
    git -C "$SRC" remote set-url origin "$DBC_APP_REPO"
    git -C "$SRC" fetch --depth=1 origin "$DBC_APP_BRANCH"
    git -C "$SRC" checkout -q -B "$DBC_APP_BRANCH" "origin/$DBC_APP_BRANCH"
    git -C "$SRC" reset -q --hard "origin/$DBC_APP_BRANCH"
  else
    [ -e "$SRC" ] && die "$SRC exists but is not a git checkout; refusing to overwrite it. Remove it or set DBC_APP_SRC."
    info "cloning $DBC_APP_REPO into $SRC"
    git clone -q --depth=1 --branch "$DBC_APP_BRANCH" "$DBC_APP_REPO" "$SRC"
  fi
}

# ---- Go --------------------------------------------------------------------

# version_ge HAVE WANT — 0 when HAVE >= WANT, comparing up to three numbers.
version_ge() {
  local have="$1" want="$2" h w i
  IFS=. read -r -a h <<< "$have"
  IFS=. read -r -a w <<< "$want"
  for i in 0 1 2; do
    local hi="${h[$i]:-0}" wi="${w[$i]:-0}"
    hi="${hi%%[^0-9]*}"; wi="${wi%%[^0-9]*}"
    hi="${hi:-0}"; wi="${wi:-0}"
    if   [ "$hi" -gt "$wi" ]; then return 0
    elif [ "$hi" -lt "$wi" ]; then return 1
    fi
  done
  return 0
}

go_version_of() {
  local v
  v="$("$1" env GOVERSION 2>/dev/null || true)"
  printf '%s' "${v#go}"
}

# resolve_go sets GO_BIN to a Go at least as new as go.mod's `go` line: the
# one on PATH, else a private copy in DBC_APP_GO_DIR (fetched once, reused).
resolve_go() {
  local want have
  want="$(sed -nE 's/^go ([0-9.]+).*/\1/p' "$SRC/go.mod" | head -1)"
  [ -n "$want" ] || die "no go directive in $SRC/go.mod"

  if command -v go >/dev/null 2>&1; then
    have="$(go_version_of "$(command -v go)")"
    if [ -n "$have" ] && version_ge "$have" "$want"; then
      GO_BIN="$(command -v go)"
      ok "Go $have ($GO_BIN)"
      return
    fi
    warn "Go $have on PATH is older than go.mod's $want"
  fi

  if [ -x "$DBC_APP_GO_DIR/bin/go" ]; then
    have="$(go_version_of "$DBC_APP_GO_DIR/bin/go")"
    if [ -n "$have" ] && version_ge "$have" "$want"; then
      GO_BIN="$DBC_APP_GO_DIR/bin/go"
      ok "Go $have ($GO_BIN)"
      return
    fi
  fi

  local tmp url
  url="https://go.dev/dl/go${want}.darwin-${ARCH}.tar.gz"
  info "downloading Go $want"
  printf '    %s%s%s\n' "$C_DIM" "$url" "$C_RESET"
  tmp="$(mktemp -d)"
  download "$url" "$tmp/go.tar.gz"
  tar -xzf "$tmp/go.tar.gz" -C "$tmp"
  [ -x "$tmp/go/bin/go" ] || die "the Go archive has no go/bin/go"
  mkdir -p "$(dirname "$DBC_APP_GO_DIR")"
  rm -rf "$DBC_APP_GO_DIR"
  mv "$tmp/go" "$DBC_APP_GO_DIR"
  rm -rf "$tmp"
  GO_BIN="$DBC_APP_GO_DIR/bin/go"
  ok "Go $(go_version_of "$GO_BIN") ($GO_BIN)"
}

# ---- build -----------------------------------------------------------------

# Everything is built into STAGE and the finished bundle moved into place at
# the end, so a failed build leaves the installed app as it was.
build_all() {
  STAGE="$(mktemp -d)"
  trap 'rm -rf "$STAGE"' EXIT

  APP="$STAGE/$DBC_APP_NAME.app"
  local contents="$APP/Contents"
  mkdir -p "$contents/MacOS" "$contents/Helpers" "$contents/Resources"

  info "building dbc"
  ( cd "$SRC" && CGO_ENABLED=0 "$GO_BIN" build -trimpath -ldflags "-s -w" -o "$contents/Helpers/dbc" . )
  VERSION="$("$contents/Helpers/dbc" version 2>/dev/null | awk '{print $NF}')"
  [ -n "$VERSION" ] || die "the built dbc does not report a version"
  ok "dbc $VERSION"

  info "building the app shell"
  swiftc -O "$SRC/macapp/DbcApp.swift" -o "$contents/MacOS/dbc-app" \
    -framework AppKit -framework WebKit
  ok "dbc-app"

  ICON_PLIST=""
  info "drawing the icon"
  if build_icon "$contents/Resources"; then
    ICON_PLIST=$'  <key>CFBundleIconFile</key>\n  <string>dbc</string>'
    ok "dbc.icns"
  else
    warn "could not draw the icon (no window server? over SSH?) — installing without one"
  fi

  write_plist "$contents/Info.plist"
  sign_app
}

# build_icon DIR — renders DIR/dbc.icns; non-zero when it could not.
build_icon() {
  local tmp="$STAGE/icon"
  mkdir -p "$tmp"
  swiftc "$SRC/macapp/MakeIcon.swift" -o "$tmp/makeicon" -framework AppKit >/dev/null 2>&1 || return 1
  "$tmp/makeicon" "$tmp/dbc.iconset" >/dev/null 2>&1 || return 1
  iconutil -c icns "$tmp/dbc.iconset" -o "$1/dbc.icns" >/dev/null 2>&1 || return 1
  [ -f "$1/dbc.icns" ]
}

# CFBundleVersion carries the commit (and "+dirty" for an uncommitted tree),
# so About says which build this is; the short version is dbc's own.
write_plist() {
  local build="dev"
  if git -C "$SRC" rev-parse --short HEAD >/dev/null 2>&1; then
    build="$(git -C "$SRC" rev-parse --short HEAD)"
    [ -z "$(git -C "$SRC" status --porcelain 2>/dev/null)" ] || build="$build+dirty"
  fi
  cat > "$1" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>en</string>
  <key>CFBundleDisplayName</key>
  <string>$DBC_APP_NAME</string>
  <key>CFBundleExecutable</key>
  <string>dbc-app</string>
  <key>CFBundleIdentifier</key>
  <string>$BUNDLE_ID</string>
$ICON_PLIST
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>$DBC_APP_NAME</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>$VERSION</string>
  <key>CFBundleVersion</key>
  <string>$build</string>
  <key>LSApplicationCategoryType</key>
  <string>public.app-category.developer-tools</string>
  <key>LSMinimumSystemVersion</key>
  <string>12.0</string>
  <key>NSHighResolutionCapable</key>
  <true/>
  <key>NSHumanReadableCopyright</key>
  <string>MIT License</string>
</dict>
</plist>
EOF
}

# sign_app signs ad hoc: the helper first, as nested code must be, then the
# bundle, which seals Info.plist and Resources. A locally built app is not
# quarantined, so Gatekeeper does not ask; the signature is what keeps macOS
# (and the keychain, should an agent use it) treating every run as the same
# app. A failure here is a warning: the app still runs unsigned.
sign_app() {
  info "signing (ad hoc)"
  if codesign --force --sign - --timestamp=none "$APP/Contents/Helpers/dbc" >/dev/null 2>&1 &&
     codesign --force --sign - --timestamp=none "$APP" >/dev/null 2>&1 &&
     codesign --verify --strict "$APP" >/dev/null 2>&1; then
    ok "signed"
  else
    warn "codesign failed; the app is installed unsigned"
  fi
}

# ---- install ---------------------------------------------------------------

install_app() {
  local dest="$DBC_APP_DIR/$DBC_APP_NAME.app"
  if pgrep -f "$dest/Contents/MacOS/dbc-app" >/dev/null 2>&1; then
    warn "$DBC_APP_NAME is running: it keeps the old build until you quit and reopen it"
  fi
  mkdir -p "$DBC_APP_DIR"
  rm -rf "$dest"
  mv "$APP" "$dest"
  # re-register, so Finder, Spotlight and the Dock pick up a new icon/version
  /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister \
    -f "$dest" >/dev/null 2>&1 || true
  INSTALLED="$dest"
}

main() {
  detect_platform
  require_tools
  resolve_source
  info "source: $SRC ($SRC_KIND)"
  resolve_go
  build_all
  install_app

  printf '\n%s✓ %s %s installed%s\n' "$C_GREEN" "$DBC_APP_NAME" "$VERSION" "$C_RESET"
  printf '  app:    %s\n' "$INSTALLED"
  printf '  source: %s\n' "$SRC"
  printf '  data:   ~/.config/dbc (shared with the dbc CLI and TUI)\n'
  printf '  log:    ~/Library/Logs/dbc/dbc-web.log\n'
  printf '\nOpen it from Finder, Spotlight or the Dock, or: open "%s"\n' "$INSTALLED"
  printf 'Re-run this script to update it.\n'
}

main "$@"
