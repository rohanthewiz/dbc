#!/bin/sh
# stamp.sh prints the -ldflags -X values that link this checkout's commit (hash,
# subject, commit time) into dbc web's build badge:
#
#   go build -ldflags "$(sh scripts/stamp.sh)" .
#
# The badge is github.com/rohanthewiz/cats/buildbadge. Rather than repeat its
# variable names here, this runs the stamp.sh that ships inside the exact
# version go.mod pins, so the names can never drift from the package being
# linked. `go mod download -json` both fetches that version (a no-op once it is
# in the module cache, which any build of dbc needs anyway) and reports where
# it lives.
#
# Run from the repo root: go.mod picks the version, and the git commands inside
# the delegated script read the current directory's checkout — dbc's, not
# cats'. Set GO to use a specific toolchain (mac-install.sh does).
#
# Offline or otherwise unresolvable, it prints nothing and the build goes ahead
# unstamped. The badge then still shows the hash and commit time, taken from
# the VCS info the Go toolchain records on its own; only the subject is missing.
go=${GO:-go}
dir=$("$go" mod download -json github.com/rohanthewiz/cats 2>/dev/null |
  sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
[ -n "$dir" ] && [ -f "$dir/buildbadge/stamp.sh" ] || exit 0
exec sh "$dir/buildbadge/stamp.sh"
