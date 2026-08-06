# Preview Manifest (`.forge/preview.yaml`)

**Kiln** starts on-demand preview environments for a worker's branch. It has no
framework detection and no container support: it runs exactly the commands a
project declares in its preview manifest. This page documents that manifest.
The feature as a whole is described in
[docs/plans/preview-environments.md](plans/preview-environments.md); the global
and per-anvil settings that gate it are in
[configuration.md](configuration.md#preview-environments-kiln).

An anvil without a manifest simply offers no preview.

## Location — the main checkout, never the PR branch

The manifest is read from **the anvil's main checkout** (`<anvil>/.forge/preview.yaml`),
not from the branch being previewed. The manifest decides which commands Kiln
executes on the host, so a pull request must not be able to change it.

The consequence is worth stating plainly: **a PR that changes how the app starts
is not previewable until its manifest change has landed on the main branch.**
The preview *code* always comes from the branch; the preview *commands* always
come from main.

## Example

```yaml
# .forge/preview.yaml
version: 1

# Optional. Run once before services start / after they stop. Typical use:
# create + migrate a per-preview database, drop it again on teardown.
setup: "./scripts/preview-db-setup.sh"
teardown: "./scripts/preview-db-teardown.sh"

services:
  api:
    command: "dotnet run --project src/Api --no-launch-profile"
    dir: "."                        # relative to the preview worktree
    env:
      ASPNETCORE_URLS: "http://127.0.0.1:{{.Port}}"
      ConnectionStrings__Default: "Server=localhost;Database=app_preview_{{.PreviewID}}"
    health: "/healthz"              # GET on the allocated port; omit = port-open check
    ready_timeout: 120s

  client:
    command: "npm run dev -- --port {{.Port}} --strictPort"
    dir: "client"
    env:
      VITE_API_URL: "http://{{.Host}}:{{.ServicePort \"api\"}}"
    entry: true                     # this service's URL is THE preview link
    ready_timeout: 60s
```

## Top-level fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `version` | int | `1` | Manifest schema version. Only `1` is supported; omitting it means `1`. |
| `setup` | string | | Command run once before any service starts. Optional. |
| `teardown` | string | | Command run once after every service has stopped. Optional; runs even when a service failed. |
| `services` | map | **required** | Service name → service definition. At least one entry. Manifest order is the start order. |

Unknown top-level fields are rejected rather than ignored, so a typo fails at
load time instead of silently doing nothing.

## Service fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `command` | string | **required** | The command line to run. Templated (see below). |
| `dir` | string | worktree root | Working directory, relative to the preview worktree. Must not be absolute or escape the worktree. |
| `env` | map[string]string | `{}` | Extra environment variables, layered on top of the `FORGE_*` context variables. Values are templated. |
| `health` | string | | HTTP path probed on the service's allocated port (e.g. `/healthz`). Must start with `/`. Omitted means "port is open" counts as ready. |
| `ready_timeout` | duration | `60s` | How long the readiness check may take before the service is marked failed. Must be a duration **string** (`120s`, `2m`) — a bare number is rejected. |
| `entry` | bool | `false` | Marks the service whose URL is *the* preview link. Required when the manifest declares more than one service; implicit when it declares exactly one. |

Service names must match `[a-zA-Z0-9][a-zA-Z0-9_-]*` — they become an
environment variable suffix (`FORGE_PREVIEW_PORT_<NAME>`), a log file name and
part of a URL. Duplicate names and unknown service fields are rejected.

Readiness is per-service: a service becomes `healthy` when its check passes
within `ready_timeout`, otherwise `failed`. One failed service does not stop the
others; the preview panel shows the state of each.

## Template variables

`command` and every `env` value — plus `setup` and `teardown` — are Go
templates expanded when a preview starts.

| Variable | Available in | Expands to |
|----------|--------------|------------|
| `{{.Port}}` | service `command` / `env` | The port allocated to *this* service. |
| `{{.ServicePort "name"}}` | anywhere | The port allocated to any service in the manifest — how one service is told where another listens. |
| `{{.PreviewID}}` | anywhere | The sanitized bead id for this preview, safe to use in database names. |
| `{{.Host}}` | anywhere | The hostname preview URLs are built from (`preview_public_host`, falling back to `preview_bind_host`). |

`{{.Port}}` is meaningless outside a service, so `setup` and `teardown` must
name a service explicitly via `{{.ServicePort "name"}}`.

Template errors are caught when the manifest is loaded, not when a preview is
started: an unknown variable, a syntax error, or a `{{.ServicePort "typo"}}`
that names no declared service all fail with an error naming the service and
field involved.

## Environment injected into every command

`setup`, `teardown` and every service command run with cwd = the service `dir`
inside the preview worktree, and receive the same vocabulary as pipeline hooks:

| Variable | Value |
|----------|-------|
| `FORGE_PREVIEW_ID` | Sanitized bead id (safe for database names) |
| `FORGE_BEAD_ID` | The bead being previewed |
| `FORGE_BRANCH` | The previewed branch |
| `FORGE_WORKTREE_PATH` | The preview worktree |
| `FORGE_ANVIL_NAME` / `FORGE_ANVIL_PATH` | The anvil the preview belongs to |
| `FORGE_PREVIEW_PORT_<NAME>` | The allocated port of each service, uppercased name |

Every service sees the port of *every* service, so a client can be told where
its api listens without either side knowing the allocation in advance. The name
is uppercased and `-` becomes `_` (`api-gateway` → `FORGE_PREVIEW_PORT_API_GATEWAY`).

Precedence, from weakest to strongest: the daemon's own environment, the
service's `env`, then the `FORGE_*` context. A manifest can therefore override
an inherited variable (that is how a service is pointed at its preview
database) but cannot override the context variables — those describe which
preview this is. Any `FORGE_*` variable in the daemon's own environment is
dropped for the same reason.

## Runtime behaviour

- **Ports** are allocated per service from `preview_port_range` before any
  command runs, so templates can reference every service's port. Each candidate
  is bind-tested, so a port another process already holds is skipped.
- **Logs**: each service's stdout and stderr are appended to
  `~/.forge/logs/<beadID>/preview-<service>.log` — the same per-bead directory
  the pipeline preserves worker logs in, so preview logs appear in the Hearth
  bead-log browser and are covered by the usual retention sweep.
- **Supervision**: each service runs through the platform shell in its own
  process group, so stopping a preview reaps the whole tree (a `npm run dev`
  that forks node and esbuild included). Stop asks politely first, then kills
  the group.
- **`setup` / `teardown`** run through the same shell and process group, with
  output appended to `~/.forge/logs/<beadID>/preview.log` (beside the service
  logs). Each is bounded by a 5-minute timeout, after which its process group is
  killed. A failing `setup` aborts the start — no service is spawned and the
  preview is unwound completely. A failing `teardown` is reported but not
  obeyed: the worktree, the ports and the registry slot are released anyway.
- **Concurrency**: at most `preview_max_concurrent` previews run at once
  (default 2). A start over that limit is rejected outright rather than queued,
  so an operator who asked for a preview learns immediately that they need to
  stop one first.
- **Unwinding**: any failure after the preview worktree is created — `setup`,
  the runtime, or every service failing its health check — kills whatever
  started, runs `teardown`, removes the checkout and deletes the state row. The
  per-service logs survive under `~/.forge/logs/<beadID>/` for the post-mortem.
- **Status**: the preview is `running` when every service is healthy,
  `degraded` when some are healthy and some failed, and `failed` when none came
  up. A failed service is never allowed to stop its siblings.

## Validation errors

Every validation error names the offending service and field. The rules:

- `version`, when set, must be `1`
- at least one service must be declared
- service names must be unique and match `[a-zA-Z0-9][a-zA-Z0-9_-]*`
- no two service names may fold to the same `FORGE_PREVIEW_PORT_<NAME>`
  variable (i.e. they must differ by more than case or `-` vs `_`)
- `command` is required and must be non-blank
- exactly one service must set `entry: true` when the manifest declares more
  than one service (a lone service is the entry point implicitly)
- `dir` must be relative and must not escape the preview worktree
- `health`, when set, must start with `/`
- `ready_timeout` must be a non-negative duration string of at least `1s`
- unknown top-level fields and unknown service fields are rejected
- templates must parse, use known variables, and only reference declared services
