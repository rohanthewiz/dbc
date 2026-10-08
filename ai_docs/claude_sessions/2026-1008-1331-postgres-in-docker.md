# Postgres in Docker from the connections menu

Session: `59967169-68ca-4752-833d-c2c860d291d0`

## Ask

1. Dropped in from the cats-todo backlog: "As part of the connections menu
   add a feature to spin up a postgres image in docker and auto-setup the
   connection for it. Give me supported versions of PG in a dropdown".
2. "Add a 'Stop container' entry in the menu and why not add the docker
   menu item under the '+' too?"
3. `/sess-wrap`.

## Decisions

No questions were asked. These were judgement calls:

- **Both UIs, one shared package.** The TUI and dbc web each have a
  connections menu, and the repo keeps them in step (as `connedit` does for
  the form). The Docker steps, the names and the connection registration
  live in a new package, `pgdocker`. `tui/pgdocker.go` and
  `web/pgdocker.go` are only UI.
- **Supported versions = postgresql.org's support table**, checked live:
  18, 17, 16, 15 and 14 (14's EOL is 2026-11-12). 19 was beta only
  (`19beta1` on Docker Hub, no `19`), so it is left out. Each `Version`
  carries its EOL, and `Supported(now)` drops a version past it, so 14
  leaves the list by itself. In the TUI the "dropdown" is a floating menu,
  the TUI's equivalent; dbc web has a `<select>`.
- **One container per major**, labelled `dbc.pgdocker=<major>`:
  - container `dbc-pg18`, volume `dbc-pg18-data`, connection `docker-pg18`;
  - host port `5500 + major` (5518), falling back to any free port, bound
    to `127.0.0.1` only;
  - a random hex password (no URL escaping needed), read back from the
    container's `POSTGRES_PASSWORD` on later starts.
- **PG 18 moved its data directory.** Checked with
  `docker buildx imagetools inspect`: 18 has `PGDATA=/var/lib/postgresql/18/docker`
  and `VOLUME /var/lib/postgresql`; 17 and older use
  `/var/lib/postgresql/data`. `dataDir(major)` picks the mount point.
- **Ready means a TCP connect through `db.Probe`**, polled every 500 ms
  for up to 90 s. The image's init server listens on the unix socket only,
  and Docker's port proxy accepts a connection before the server listens,
  so a TCP answer through the driver is the reliable signal.
- **Driven through the docker CLI**, not the Engine API: no new
  dependency, and the user's contexts, colima and Docker Desktop all work.
  `FindBin` also looks in the usual install directories (`/usr/local/bin`,
  `/opt/homebrew/bin`, `~/.docker/bin`, Docker.app, Rancher), because the
  macOS app gets launchd's short PATH.
- **Registering the connection** (`Register`):
  - no such name: Add;
  - same DSN: nothing to do;
  - a different DSN (the container was recreated): Edit, keeping `ai_rows`;
  - a config-file connection already has the name: add `docker-pg18-2`.
- **A container under dbc's name that dbc didn't label** is refused, never
  started or stopped.
- **Docker failures are answers, not 500s**, in the web: `ok: false` plus
  docker's words, like a connection test's failure. "Not running" also
  carries docker's first line. The e2e's scratch HOME hid Docker Desktop's
  context and showed this case.
- **One start at a time**: the TUI has `pgDockerBusy` (rows dim with why),
  the web `Server.pgDockerOne.TryLock` (409).
- **Stop container** is on the right-click menu of a connection that
  `Register` made (`MajorOf`: `docker-pg(\d+)(-\d+)?`, a known major,
  `Web`). It is refused while dbc is on that connection, the same rule as
  Remove: the TUI's `connInUse`, the web's `inUseRefusal` across windows.
  The JS mirrors the name pattern only to decide whether to show the row;
  the server checks it with `StopTarget`. After a stop the pool is dropped.
  A container that is already stopped is "was not running", not an error.
- **+ became a dropdown.** In dbc web, **+** opens a menu: Add a
  connection… (first and focused, so + Enter is still the form) and
  Postgres in Docker…. The TUI has no + button, so its `+` key opens the
  same two-row menu and `a` still opens the form directly.

## Changes

- **New `pgdocker/`.**
  - `pgdocker.go`: `Versions`/`Supported`/`Known`, the names,
    `PreferredPort`, `dataDir`, `Docker{Bin, Run, Ping, ReadyTimeout,
    Progress}`, `FindBin`, `Check`, `Statuses` (`docker ps` filtered by
    label), `Start` (inspect → pull/create | start | reuse → `waitReady`),
    `Stop`, `DSN`.
  - `register.go`: `New(progress)` (Ping = `db.Probe`), `Register`,
    `MajorOf`, `StopTarget`.
- **TUI.**
  - `tui/pgdocker.go`: the menu row, the version menu (with the
    containers' state on the right, and "(support ends Nov 2026)" for a
    version ending within the year), the start (progress through `m.send`
    into the log), Register, and connecting; the Stop row and the stop.
  - `tui/connform.go`: `connAddItems`, used at the foot of the
    connections menu and by `+`, and the Stop row.
  - `tui/app.go`: new messages and the `pgDockerBusy` field.
  - `tui/help.go`: rows updated.
- **Web.**
  - `web/pgdocker.go`: `GET /api/v1/pgdocker` (versions and states),
    `POST /api/v1/pgdocker` (start and register, broadcasts "conns") and
    `POST /api/v1/pgdocker/stop`. Routes are in `server.go`.
  - `conns.js`: `openPGDocker` (a dialog with a select, Start, a message
    while it waits, closing it mid-start logs the outcome), `stopPGDocker`,
    `stopItem`, `addItems`, the + dropdown.
  - `workbench.go`: the +'s title and `aria-haspopup`; `app.js` help rows.
- **Docs.**
  - A README "Postgres in Docker" section.
  - The menu and keys tables, and the dbc web "Adding connections" text,
    updated.
  - The Tests section now lists four opt-in suites, including the new
    live Docker test.

## Verification

- **Checks.** `gofmt` is clean, `go vet ./...` passes, and
  `go test ./...` passes.
- **`pgdocker` unit tests** (fake CLI):
  - Supported/EOL; names, ports and dataDir; ps parsing; not running.
  - Start's create/pull/wait, removing a container that failed to run,
    reuse of a stopped or running one, refusing a foreign container or an
    unknown version, and the ready timeout.
  - Stop's four cases; MajorOf/StopTarget.
  - Register's add, keep, update (keeping ai_rows, the in-use refusal) and
    the config-file clash.
- **Live** (`DBC_LIVE_DOCKER=1`): pass on PG 17, and on PG 18 including
  the pull (21 s). Start, Register, a connect over the registered DSN,
  Statuses, Stop twice, restart with the same DSN, no re-register. The
  container and volume are removed afterwards. `postgres:18` stays pulled
  on this machine.
- **TUI tests.**
  - `TestPGDockerStartsAndAddsTheConnection`,
    `TestPGDockerSaysWhenDockerIsDown`, `TestPGDockerOneStartAtATime`.
  - `TestPGDockerStopFromTheMenu`: dimmed while on it, inspects and never
    stops an unseen container, no row on other connections.
  - `TestConnFormRefusalStaysInForm` now goes `+` → menu → Enter → form.
- **Web tests.** `TestPGDockerListAndStart`, `TestPGDockerDown` and
  `TestPGDockerStop` (ok, 400 for a connection that isn't a Docker one,
  404 for a missing one).
- **Web e2e** (`DBC_E2E=1`): the full suite passes. The new step
  `pgDockerDialog` checks the right-click row, then opens the dialog from
  +, and checks the dropdown fills with Start enabled (or the Docker error
  with Start off), then Cancel. It never presses Start. With
  `DOCKER_CONFIG=~/.docker` the dropdown path was exercised. `connForm`
  now goes through the + menu.

## Next

Closed: None. Declined: None. Raised: N-160, N-161, N-162.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
