# Reference Preview Manifests

This page is the cookbook: two complete, working `.forge/preview.yaml` manifests
with every field explained, plus the database lifecycle scripts that make the
second one work. [preview-manifest.md](preview-manifest.md) is the reference —
every field, every default, every validation rule. Read that when you need to
know what a field *means*; read this when you need something to copy.

Both manifests on this page are test fixtures
([`internal/kiln/testdata/manifests/`](../internal/kiln/testdata/manifests/)):
a unit test parses them through the real Kiln loader and asserts that the YAML
printed here is byte-identical to the file it parsed, so these examples cannot
drift away from the schema they claim to satisfy.

> Hytte's own `.forge/preview.yaml` lives in the Hytte repository, not here —
> a manifest is read from the anvil it belongs to. Example A below is shaped
> like it, but it is an example, not that file.

## Before either example works

1. `settings.preview_enabled: true` in `~/.forge/config.yaml`, and the anvil has
   not set `preview_enabled: false` — see
   [Preview Environments (Kiln)](configuration.md#preview-environments-kiln).
2. The manifest is committed to the anvil's **main branch** (see
   [The manifest is loaded from main](#the-manifest-is-loaded-from-main)).
3. Whatever the commands need — `go`, `npm`, `dotnet`, a database server — is
   installed on the Forge box and on the daemon's `PATH`.

---

## Example A — Go API + Vite client

A backend that owns its own storage (a SQLite file) and a Vite dev server in
front of it. Nothing outside the preview worktree is touched, so there is no
`setup` and no `teardown`: the whole preview is two processes and one file that
is deleted with the checkout.

```yaml
# Go API + Vite client — a project whose preview needs no database server.
#
# Copy to <anvil>/.forge/preview.yaml ON THE MAIN BRANCH and adjust the paths.
version: 1

services:
  api:
    command: "go run ./cmd/server"
    env:
      # Kiln allocates the port; the app is told which one it got.
      PORT: "{{.Port}}"
      APP_ENV: "preview"
      # A SQLite file inside the preview worktree. The worktree is deleted when
      # the preview stops, so the database dies with it — that is why this
      # manifest needs no setup/teardown at all.
      DATABASE_PATH: "./preview-{{.PreviewID}}.db"
      SESSION_SECRET: "preview-only-{{.PreviewID}}"
    health: "/healthz"
    # `go run` compiles the whole module the first time a preview runs on a
    # cold build cache. Be generous here rather than debugging a false failure.
    ready_timeout: 180s

  web:
    # `--host {{.BindHost}}` follows preview_bind_host, so widening that setting
    # to reach previews from a LAN does not also mean editing this manifest.
    command: "npm run dev -- --host {{.BindHost}} --port {{.Port}} --strictPort"
    dir: "web"
    env:
      # The api's port is not known until the preview starts, so the client is
      # told where to find it instead of hardcoding one.
      VITE_API_URL: "http://{{.Host}}:{{.ServicePort \"api\"}}"
      BROWSER: "none"
    # This service's URL is the preview link.
    entry: true
    ready_timeout: 90s
```

**What each piece does**

- **`api.command`** — `go run ./cmd/server` runs from the worktree root (no
  `dir`, so the default applies). The command goes through `sh -c` (`cmd /c` on
  Windows), so shell syntax is available.
- **`api.env.PORT: "{{.Port}}"`** — Kiln allocates a free port per service out
  of `preview_port_range` *before* any command runs, and `{{.Port}}` is this
  service's. The same value also arrives as `FORGE_PREVIEW_PORT_API`, so an app
  that already reads a `FORGE_*` variable does not need the `env:` entry at all.
- **`api.env.DATABASE_PATH`** — `{{.PreviewID}}` is the bead id reduced to a
  safe identifier (lowercased, everything else folded to `_`, never leading with
  a digit). Two previews of two beads therefore never share a file. The path is
  relative, so the file lands in the preview worktree and is removed with it.
- **`api.health: "/healthz"`** — Kiln issues `GET /healthz` against the
  service's allocated port until it answers or `ready_timeout` expires. Omit
  `health` and "the port accepts a connection" becomes the readiness signal —
  fine for a dev server, too weak for an API that opens a database first.
- **`api.ready_timeout: 180s`** — must be a duration **string**. A bare `180`
  is rejected at load time, because in YAML that decodes to 180 nanoseconds.
- **`web.dir: "web"`** — relative to the preview worktree; it may not be
  absolute and may not climb out with `..`.
- **`web.command --host {{.BindHost}}`** — `preview_bind_host`, the address Kiln
  expects services to listen on. Passing it through means the dev server binds
  where Kiln probes, instead of the manifest hardcoding an address that
  disagrees with the setting. See
  [Bind host vs. public host](#bind-host-vs-public-host).
- **`web.env.VITE_API_URL`** — `{{.ServicePort "api"}}` is how one service is
  told where another listens. Referencing a service the manifest does not
  declare is a load-time error, not a silently empty string. `{{.Host}}` is
  `preview_public_host` falling back to `preview_bind_host` — the name a browser
  would use, which is what belongs in a URL the *client* hands to the browser.
- **`web.entry: true`** — exactly one service must be marked `entry` when there
  is more than one. Its URL is the "Open preview" link in Hearth. (A manifest
  with a single service gets this implicitly.)
- **No `version` drama** — `version: 1` is the only supported value and the
  default; writing it is a habit worth keeping for when there is a version 2.

**Why the client is the entry and the API is not.** The preview link is what a
reviewer clicks. For a split frontend/backend app that is the dev server; the
API is reachable through it. If your Go binary serves the built SPA itself, the
manifest collapses to one service and `entry` becomes implicit.

---

## Example B — .NET API + React client + shared MSSQL

The realistic corporate shape: a database server that is far too heavy to start
per preview, so it runs once and each preview gets its own **database** on it.
`setup` creates and migrates that database; `teardown` drops it.

```yaml
# .NET API + React client + a database on a SHARED MSSQL server.
#
# The database server is NOT a preview service: it runs once on the Forge box
# and every preview gets its own database on it, created by `setup` and dropped
# by `teardown`.
#
# Copy to <anvil>/.forge/preview.yaml ON THE MAIN BRANCH and adjust the paths.
version: 1

setup: "./scripts/preview-db-setup.sh"
teardown: "./scripts/preview-db-teardown.sh"

services:
  api:
    # The connection string is assembled by the shell rather than declared under
    # `env:`, so the password comes from the daemon's environment (~/.forge/env)
    # instead of being committed in the manifest. Manifest `env:` values are
    # literal — "$VAR" in one stays the four characters "$VAR" — while a
    # `command` runs through `sh -c` (see the Windows note in the docs).
    command: >-
      ConnectionStrings__Default="Server=$PREVIEW_MSSQL_HOST;Database=app_preview_{{.PreviewID}};User Id=$PREVIEW_MSSQL_USER;Password=$PREVIEW_MSSQL_PASSWORD;TrustServerCertificate=True"
      dotnet run --project src/Api --no-launch-profile
    env:
      ASPNETCORE_ENVIRONMENT: "Preview"
      # {{.BindHost}} is preview_bind_host — Kestrel binds exactly where Kiln
      # probes, whether that is loopback or 0.0.0.0 for a LAN.
      ASPNETCORE_URLS: "http://{{.BindHost}}:{{.Port}}"
    health: "/health/ready"
    # First `dotnet run` after a checkout restores and builds.
    ready_timeout: 240s

  client:
    command: "npm run dev -- --host {{.BindHost}} --port {{.Port}} --strictPort"
    dir: "src/Client"
    env:
      VITE_API_BASE_URL: "http://{{.Host}}:{{.ServicePort \"api\"}}"
      BROWSER: "none"
    # This service's URL is the preview link.
    entry: true
    ready_timeout: 90s
```

**What is new compared to Example A**

- **`setup` / `teardown`** — one command each, run in the preview worktree
  through the same shell and process group as the services, with output
  appended to `~/.forge/logs/<beadID>/preview.log`. Each is bounded by a
  5-minute timeout. A failing `setup` aborts the start: no service is spawned
  and the preview is unwound completely. A failing `teardown` is reported but
  not obeyed — the worktree, the ports and the registry slot are released
  regardless, so a broken teardown script leaks databases, not previews.
- **`{{.Port}}` is not available in `setup`/`teardown`** — they run outside any
  one service. Name a service explicitly with `{{.ServicePort "api"}}` if a
  script needs a port. The scripts below need none: they address the database
  by `FORGE_PREVIEW_ID`.
- **The connection string lives on the command line, not in `env:`** — see
  [Secrets: `env:` values are literal](#secrets-env-values-are-literal).

### `scripts/preview-db-setup.sh`

Copy-paste runnable. Every value you must change is marked `<<LIKE THIS>>`;
`grep '<<' scripts/preview-db-*.sh` should come back empty when you are done.

```bash
#!/usr/bin/env bash
# Kiln preview setup: create and migrate this preview's database on the shared
# MSSQL server. Referenced from .forge/preview.yaml as `setup`.
#
# Runs with cwd = the preview worktree and inherits the Forge daemon's
# environment, so credentials belong in ~/.forge/env, never in this file.
set -euo pipefail

# --- placeholders -----------------------------------------------------------
API_PROJECT="<<src/Api/Api.csproj>>"       # project holding the EF Core DbContext
SEED_SCRIPT="<<scripts/seed-preview.sql>>" # optional; skipped when the file is absent
# ---------------------------------------------------------------------------

# Injected by Kiln into every preview command. Fail loudly if this script is
# ever run outside a preview, rather than touching a database named app_preview_.
: "${FORGE_PREVIEW_ID:?not running inside a Kiln preview (FORGE_PREVIEW_ID is unset)}"

# Set these in ~/.forge/env so the daemon exports them to every preview command.
MSSQL_HOST="${PREVIEW_MSSQL_HOST:-localhost}"
MSSQL_USER="${PREVIEW_MSSQL_USER:-sa}"
MSSQL_PASSWORD="${PREVIEW_MSSQL_PASSWORD:?set PREVIEW_MSSQL_PASSWORD in ~/.forge/env}"

DB="app_preview_${FORGE_PREVIEW_ID}"
CONN="Server=${MSSQL_HOST};Database=${DB};User Id=${MSSQL_USER};Password=${MSSQL_PASSWORD};TrustServerCertificate=True"

# -b makes sqlcmd exit non-zero on a SQL error; -C trusts the server certificate.
mssql() {
  command sqlcmd -S "$MSSQL_HOST" -U "$MSSQL_USER" -P "$MSSQL_PASSWORD" -C -b "$@"
}

# 1. Create the database. Idempotent: a retried start reuses what is there.
#    FORGE_PREVIEW_ID is sanitized by Kiln to [a-z][a-z0-9_]*, so it is safe to
#    interpolate into an identifier.
echo "kiln setup: creating database ${DB} on ${MSSQL_HOST}"
mssql -Q "IF DB_ID('${DB}') IS NULL CREATE DATABASE [${DB}];"

# 2. Apply EF Core migrations against this preview's database. --connection
#    keeps the app's own appsettings out of it entirely.
echo "kiln setup: applying EF Core migrations"
dotnet ef database update --project "$API_PROJECT" --connection "$CONN"

# 3. Optional seed data. Guarded so the manifest works in a repo that has none.
if [ -f "$SEED_SCRIPT" ]; then
  echo "kiln setup: seeding from ${SEED_SCRIPT}"
  mssql -d "$DB" -i "$SEED_SCRIPT"
else
  echo "kiln setup: no seed script at ${SEED_SCRIPT}, skipping"
fi

echo "kiln setup: ${DB} ready"
```

### `scripts/preview-db-teardown.sh`

```bash
#!/usr/bin/env bash
# Kiln preview teardown: drop this preview's database. Referenced from
# .forge/preview.yaml as `teardown`.
#
# Runs after every service has stopped — including when the preview failed to
# start, when it was reaped for idleness, and when its PR merged. It must
# therefore be idempotent and safe to re-run against a database that is already
# gone.
set -euo pipefail

: "${FORGE_PREVIEW_ID:?not running inside a Kiln preview (FORGE_PREVIEW_ID is unset)}"

MSSQL_HOST="${PREVIEW_MSSQL_HOST:-localhost}"
MSSQL_USER="${PREVIEW_MSSQL_USER:-sa}"
MSSQL_PASSWORD="${PREVIEW_MSSQL_PASSWORD:?set PREVIEW_MSSQL_PASSWORD in ~/.forge/env}"

DB="app_preview_${FORGE_PREVIEW_ID}"

mssql() {
  command sqlcmd -S "$MSSQL_HOST" -U "$MSSQL_USER" -P "$MSSQL_PASSWORD" -C -b "$@"
}

# SINGLE_USER WITH ROLLBACK IMMEDIATE evicts connections the API left behind —
# without it DROP DATABASE blocks until every pooled connection times out, and
# the 5-minute teardown budget is spent waiting.
echo "kiln teardown: dropping database ${DB} on ${MSSQL_HOST}"
mssql -Q "IF DB_ID('${DB}') IS NOT NULL
BEGIN
  ALTER DATABASE [${DB}] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
  DROP DATABASE [${DB}];
END"

echo "kiln teardown: ${DB} dropped"
```

**PostgreSQL instead of MSSQL?** The shape is identical; only the two SQL
statements change:

```bash
# setup
psql -h "$PGHOST" -U "$PGUSER" -d postgres -v ON_ERROR_STOP=1 \
  -c "SELECT 'CREATE DATABASE app_preview_${FORGE_PREVIEW_ID}'
      WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'app_preview_${FORGE_PREVIEW_ID}')\gexec"

# teardown — WITH (FORCE) is the Postgres 13+ equivalent of SINGLE_USER
psql -h "$PGHOST" -U "$PGUSER" -d postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS app_preview_${FORGE_PREVIEW_ID} WITH (FORCE)"
```

---

## Concepts these examples depend on

### One database server, one database per preview

A preview is a handful of local processes, not a container stack. Starting a
database server per preview would cost gigabytes and minutes; Kiln's model is
the opposite:

- **The server is shared and long-lived.** It is not declared in the manifest at
  all — Forge never starts or stops it. Install it once on the Forge box (or
  point at one nearby) and let it run.
- **The database is per preview**, named from `FORGE_PREVIEW_ID` —
  `app_preview_<sanitized bead id>`. `SanitizePreviewID` lowercases letters,
  folds everything else to `_`, collapses runs, and guarantees the result starts
  with a letter, precisely so it can be dropped into an identifier without
  quoting gymnastics.
- **`setup` creates it, `teardown` drops it.** Both run for every preview
  lifecycle event, including failed starts and idle reaping, so both must be
  idempotent.
- **The cleanup responsibility is yours.** Kiln releases the worktree, the ports
  and the registry row whether or not `teardown` succeeded. If your teardown
  script is broken you will find stale `app_preview_*` databases on the server;
  a periodic sweep (`SELECT name FROM sys.databases WHERE name LIKE
  'app_preview_%'`) is a reasonable safety net on a busy box.
- **Isolation is per database, not per server.** Two previews share a buffer
  pool, a `tempdb`, a login and a max-connections budget. That is fine for
  reviewing a UI change and wrong for a load test. Migrations that alter
  server-level objects (logins, linked servers, server-scoped configuration)
  will collide — keep them out of the preview path.
- **Credentials come from the daemon's environment.** Put them in `~/.forge/env`
  and reference them from the scripts. The manifest is committed source and the
  logs are readable in Hearth.

### Secrets: `env:` values are literal

Manifest `env:` values are Go templates, not shell strings. `{{.Port}}` expands;
`$PREVIEW_MSSQL_PASSWORD` does not — it reaches the process as the literal text
`$PREVIEW_MSSQL_PASSWORD`. A `command`, on the other hand, is handed to
`sh -c`, so shell expansion works there. Hence Example B's connection string:

```yaml
command: >-
  ConnectionStrings__Default="...Password=$PREVIEW_MSSQL_PASSWORD..."
  dotnet run --project src/Api --no-launch-profile
```

The `VAR=value command` prefix form is POSIX shell syntax. **On Windows**, where
commands run through `cmd /c`, it is not valid — use
`set VAR=value&& dotnet run ...` or, better, move the whole invocation into a
small script and call that.

Precedence, weakest to strongest: the daemon's own environment, then the
service's `env:`, then Kiln's `FORGE_*` context. A manifest can therefore
override an inherited variable but cannot override `FORGE_PREVIEW_ID` and
friends — those say which preview this is. Any `FORGE_*` variable in the
daemon's own environment is dropped for the same reason.

### `npm ci` must never run in a preview worktree

A preview checkout gets `node_modules` **linked** (a symlink, or an NTFS
junction on Windows) to the anvil's main checkout, which is what makes
`npm run dev` start in seconds instead of after a full install.

That link points *at* the main checkout. `npm ci` begins by deleting
`node_modules` — through the link — and would rewrite the dependencies of the
main checkout and of every worker worktree junctioned to it, mid-run. Never put
`npm ci`, `npm install` or `npm clean-install` in a preview `command` or in
`setup`. (Temper carries the same guard for worker worktrees, and blocks the
step outright.)

The consequence is that **a preview runs against main's `node_modules`**. If the
branch being previewed adds or bumps a dependency, install it once in the anvil's
main checkout before starting the preview:

```bash
cd <anvil> && npm install
```

If a project genuinely cannot share dependencies, the escape hatch is to have
the service command install into a directory that is not the junction (for
example `npm ci --prefix .preview-deps` plus a `NODE_PATH`), and to accept the
per-start cost — but treat that as a last resort.

The same invariant runs the other way: Forge itself must not `npm ci` the **main
checkout** while a preview is linked into it. Depcheck syncs `node_modules` with
`npm ci --ignore-scripts` before reading `npm outdated`, so it asks the daemon
whether the anvil has a live preview first and skips its whole npm half for that
cycle when it does, logging the bead holding the preview:

```
[depcheck] heimdall: skipping npm scan — Kiln preview for bead Forge-abc1 holds this checkout's node_modules
```

Go and .NET scanning are unaffected — neither deletes anything. The liveness
check is re-read immediately before npm is spawned, which leaves a window of a
few statements in which a preview could start; that race is accepted, and is no
wider than the pre-existing one against a worker's own npm build step.

### The manifest is loaded from main

Kiln reads `.forge/preview.yaml` from **the anvil's main checkout**, never from
the branch being previewed. The manifest decides which commands run on the Forge
host, so a pull request must not be able to change it — a PR that could edit the
manifest could run anything, as the daemon, on the box.

Two consequences worth internalising:

- **The preview code comes from the branch; the preview commands come from
  main.** A PR that changes how the app starts (new service, renamed script,
  different port flag) is not previewable until that manifest change has landed
  on main.
- **Iterate on a manifest by merging it.** The practical loop for a new manifest
  is: write it, merge it to main (it affects nothing until someone starts a
  preview), then start previews and fix forward. Editing it in a feature branch
  and wondering why nothing changed is the usual first mistake.

A manifest change is picked up on the next preview start — no daemon restart.

### Bind host vs. public host

`{{.Host}}` is `preview_public_host` falling back to `preview_bind_host`: the
name that belongs in a *URL*. It is not necessarily an address a server can
bind. `{{.BindHost}}` is the other half — `preview_bind_host` itself, the
address Kiln expects services to listen on and probes for health.

Kiln does not bind anything on a service's behalf: a third-party dev server
listens wherever its own command line says. So if you set
`preview_bind_host: 0.0.0.0` to reach previews from a LAN or VPN, the services
have to be told to bind wide too — but use `{{.BindHost}}` rather than writing
the address twice:

```yaml
services:
  client:
    command: "npm run dev -- --host {{.BindHost}} --port {{.Port}} --strictPort"
  api:
    command: "dotnet run --project src/Api --no-launch-profile"
    env:
      ASPNETCORE_URLS: "http://{{.BindHost}}:{{.Port}}"
```

Both examples above are written that way. A manifest that follows the setting
keeps working the day someone widens or narrows `preview_bind_host`, where one
that hardcodes `127.0.0.1` starts disagreeing with it. Remember that preview
URLs bypass the Hearth login; see the security note in
[configuration.md](configuration.md#preview-environments-kiln).

### Where the output goes

Everything a preview prints is under `~/.forge/logs/<beadID>/`, the same
directory the pipeline preserves worker logs in — so it shows up in Hearth's
bead-log browser and is covered by the retention sweep:

| File | Contents |
|------|----------|
| `preview.log` | `setup` and `teardown` output |
| `preview-<service>.log` | that service's stdout and stderr |

A failed `setup` also carries the tail of its output into the error surfaced in
Hearth, so the usual "why didn't my preview start" question is answerable
without opening a file.

## Checklist for a new manifest

1. Can every service be started by one command line, from a clean checkout,
   with only `node_modules` pre-linked? If not, that work belongs in `setup`.
2. Does exactly one service have `entry: true`?
3. Does every port come from `{{.Port}}` / `{{.ServicePort "..."}}` rather than
   a hardcoded number? Two previews run at once by default.
4. Is `ready_timeout` long enough for a **cold** first run (restore, compile,
   migrate)?
5. Does `setup` create only per-preview state, and does `teardown` remove
   exactly that state — twice in a row without failing?
6. Is there an `npm ci` anywhere? Remove it.
7. Is the manifest on main?
