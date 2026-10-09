// Package e2e drives dbc web in a headless Chrome: the page's JavaScript,
// end to end, against a dbc binary built from this checkout. The Go route
// tests in web/ stop at the HTTP layer; this is what catches a regression
// in app.js, grid.js or conns.js before a person clicks into it.
//
// It is opt-in, like the db/live_* tests, and skipped unless asked for:
//
//	cd web/e2e
//	DBC_E2E=1 go test -count=1 -v .
//
// What it needs and what it reads:
//
//	DBC_E2E=1           run at all
//	DBC_E2E_CHROME      the browser binary; else Chrome's macOS path, else
//	                    the first Chrome/Chromium on PATH (never a download)
//	DBC_E2E_HEADFUL=1   show the browser window, to watch a run or debug one
//	DBC_LIVE_PG_DSN     also check the Postgres schema picker (the same DSN
//	                    the db/live_* tests use; see the live-db recipe)
//	DBC_E2E_STEPS       only the steps whose names contain one of these
//	                    comma-separated words (sign-in and boot always run),
//	                    e.g. DBC_E2E_STEPS=script
//	DBC_E2E_SHOTS       a directory: steps that take screenshots (shot)
//	                    write them there as PNGs, for a person to look at
//
// Everything else is self-contained: the test builds dbc, writes a config
// with two file-backed SQLite connections under a temporary HOME, seeds them
// with the headless `dbc "SQL"` mode, and runs `dbc web` on a free loopback
// port with a fixed secret. No docker is needed for the core checks.
//
// WHY A NESTED MODULE. go-rod and its dependencies are test tooling for one
// opt-in suite; as a module of its own here they never enter dbc's go.mod,
// go.sum or build graph, and `go test ./...` at the repository root does
// not descend into it (a directory with its own go.mod is another module).
// The test does not import dbc's packages at all — it drives the built
// binary as a user would — so it needs no replace directive either.
package e2e
