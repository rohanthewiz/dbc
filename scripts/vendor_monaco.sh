#!/bin/bash
# Vendor the Monaco editor into web/static/vendor/monaco, so dbc web's SQL
# editor works with no network: the files are embedded in the dbc binary
# (go:embed of web/static), and the page loads nothing from a CDN — its CSP
# would refuse it anyway.
#
# Keep MONACO_VERSION in sync with MONACO_VERSION in web/static/js/editor.js.
# gonotes vendors the same version, so the author's apps share one editor.
#
# TRIMMED. The full min build is ~14 MB; dbc edits SQL and nothing else, so
# only what a SQL editor loads is kept (~4.5 MB):
#
#   vs/loader.js                     the AMD loader
#   vs/editor/editor.main.{js,css}   the editor itself
#   vs/base/worker/workerMain.js     the editor worker (diffs, word suggestions)
#   vs/base/browser/ui/codicons/…    the icon font the editor's widgets use
#   vs/basic-languages/{sql,mysql,pgsql}  the three SQL tokenizers
#
# Dropped: the TypeScript/CSS/HTML/JSON language services (vs/language, most
# of the size), every other tokenizer, and the translated UI strings. Monaco
# loads a language only when a model of it is created, so a language that is
# not here is never asked for.
#
# The npm tarball is fetched directly (curl + tar), so the script needs no
# Node toolchain.

set -euo pipefail

MONACO_VERSION="0.52.2"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VENDOR_DIR="$SCRIPT_DIR/../web/static/vendor/monaco"
TARBALL_URL="https://registry.npmjs.org/monaco-editor/-/monaco-editor-${MONACO_VERSION}.tgz"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading monaco-editor@${MONACO_VERSION}..."
curl -sSL "$TARBALL_URL" -o "$TMP_DIR/monaco.tgz"

echo "Extracting the min build and license..."
tar -xzf "$TMP_DIR/monaco.tgz" -C "$TMP_DIR" package/min/vs package/LICENSE

SRC="$TMP_DIR/package/min/vs"
KEEP=(
  loader.js
  editor/editor.main.js
  editor/editor.main.css
  base/worker/workerMain.js
  base/browser/ui/codicons/codicon/codicon.ttf
  basic-languages/sql/sql.js
  basic-languages/mysql/mysql.js
  basic-languages/pgsql/pgsql.js
)

# Replace the vendored copy wholesale so removed files don't linger
rm -rf "$VENDOR_DIR"
for f in "${KEEP[@]}"; do
  mkdir -p "$VENDOR_DIR/vs/$(dirname "$f")"
  cp "$SRC/$f" "$VENDOR_DIR/vs/$f"
done
cp "$TMP_DIR/package/LICENSE" "$VENDOR_DIR/LICENSE"
echo "$MONACO_VERSION" > "$VENDOR_DIR/VERSION"

echo "Vendored monaco-editor@${MONACO_VERSION} -> $VENDOR_DIR ($(du -sh "$VENDOR_DIR" | cut -f1))"
echo "Rebuild dbc to embed the new files."
