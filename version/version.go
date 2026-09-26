// Package version holds dbc's release version. It is its own tiny package so
// the release workflow (.github/workflows/release.yml) can bump it with a
// one-line sed and the diff stays trivial to review.
//
// Three things carry this number and must agree: this constant, the
// `version = ` line in cats-plugin.toml, and the v<x.y.z> git tag. Release CI
// rewrites the first two in one commit and then cuts the tag;
// TestVersion_MatchesCatsManifest (main_test.go) fails the tree when a hand
// edit touches only one of them.
package version

// Version is the dbc release version, printed by `dbc --version`. Plain
// major.minor.patch with no pre-release suffix: the release workflow's
// auto-bump parses exactly that shape.
const Version = "0.2.0"
