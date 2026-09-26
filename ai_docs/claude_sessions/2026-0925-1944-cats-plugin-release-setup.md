# Cats plugin parity with ced, and release CI

Session: 4c1535ca-9ab9-455c-b5db-de748bf3bbac
Date: 2026-09-25

## Ask

"Ensure dbc is properly set up as a plugin for cats, similar to ../ced/. Also,
should we release a new version?" After the review, the user said: commit,
sess-wrap, and release v0.2.0.

## Findings

- `cats-plugin.toml` was already a valid manifest. `catctl plugin link .`
  loaded it (`rohanthewiz.dbc v0.1.0 [db_client]`, actions `tui` and `web`),
  and `db_client` is in cats' `wire.KnownPluginTypes`.
- **The linked plugin's binary was stale.** `bin/dbc` dated from 2026-09-24,
  so `~/.cats/bin/dbc` was missing N-057 and N-059. Re-linking rebuilt it.
- **Static completions had drifted from `newCLI`.** They were missing `--tx`,
  `-k`/`--keep-going` and explain's `--theme`.
- **Compared with ced, dbc lacked:** a version constant in the binary, a test
  that keeps the manifest version in step with it, release automation
  (a `release` branch workflow plus goreleaser), and a test workflow. ced's
  `install.sh` wasn't copied, because the cats plugin and `go install` cover
  installs.
- There were about 20 substantive commits since `v0.1.0`, among them all of
  `dbc web` (Phases 1–6), adding and editing connections in the browser,
  `connections.toml` (N-057), plan PDF/image/Mermaid export and light plan
  pictures (N-059). The recommendation was a minor bump to **v0.2.0**.

## Changes (`153a171`)

| Piece | Change |
|---|---|
| `version/version.go` | New package holding `const Version`. A top-level package, matching dbc's layout (ced uses `internal/version`). |
| `main.go` | `Version: version.Version` on the root command. The cli package then adds `--version`/`-v` (`dbc version 0.1.0`). |
| `manifest_test.go` | Covers four things. `TestVersion_IsSemver` requires a bare x.y.z. `TestVersion_MatchesCatsManifest` compares the manifest version with the constant. `TestCatsManifest_CompletionsCoverCLI` checks both directions: every subcommand and every flag name or alias from the root and the subcommands, plus `--help`, `--version` and `-v`, must be in the manifest, and nothing extra may be. `TestCatsManifest_BinMatchesBuild` requires id `rohanthewiz.dbc` and bin `./bin/dbc`. The tests decode only these keys with BurntSushi/toml, so a new host key won't break them. |
| `cats-plugin.toml` | Completions gain `--tx`, `-k`, `--keep-going`, `--theme`, `-v` and `--version`. New comments explain where the version comes from and point to the test. |
| `.github/workflows/release.yml` | Adapted from ced. It runs on a push to `release`. It runs `go test ./...` first, which ced doesn't. It then bumps the patch number in `version/version.go` and `cats-plugin.toml` with a `[skip ci]` commit, unless the pushed commit hand-edited `version.go`, in which case it uses that number as-is. Then it tags `v<x.y.z>` and runs goreleaser. The git identity is set unconditionally (ced learned that the hard way). The header has a branch diagram. |
| `.goreleaser.yml` | linux/darwin × amd64/arm64 with `CGO_ENABLED=0`. Archives hold `README*` and `dbc.example.toml`. `LICENSE*` was dropped because dbc has none (N-060). The changelog also filters out `Session doc:` and `Next list:` commits. |
| `.github/workflows/test.yml` | tidy check, build, vet and `go test -race ./...` on ubuntu and macOS. The `db/live_*` tests skip without their DSNs. |
| `README.md` | The Build section now lists `go install …@latest`, releases and `--version`, and has a new "Releasing" subsection. The Cats plugin section mentions `catctl plugin update` and `--ref vX.Y.Z` pinning. |

Verified locally, with `CATS_*` stripped: `go mod tidy` leaves no diff,
`go vet ./...` passes, and `go test ./...` and `go test -race ./...` are all
green. `catctl plugin link .` validated the manifest and rebuilt the plugin.
Not verified locally: goreleaser isn't installed (the config is checked only
against ced's working one), and neither workflow has run on GitHub yet.

## Release v0.2.0

The auto-bump only produces a patch release (0.1.1), so v0.2.0 needs a
hand bump. The hand bump is only used when the *last* pushed commit edits
`version.go` (the workflow diffs `HEAD~1..HEAD`), so the order is:

1. This session doc is committed and pushed to `main`.
2. A final commit sets `0.2.0` in both `version/version.go` and
   `cats-plugin.toml`.
3. `git push origin main main:release`. This creates `release` and starts the
   workflow.
4. Once the workflow finishes, `release` is merged back into `main` (a
   no-op on a hand bump, since CI makes no bump commit then).

## Next

Closed: None. Declined: None. Raised: N-060.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
