# Kiln — Preview Environments for Workers

Status: draft for bead breakdown
Related: docs/plans/questgiver-adventurer.md (Adventurer integration in Phase 3)

## Problem

When a worker's PR is ready to merge, the only ways to evaluate it are reading
the diff, trusting Temper/Warden/Assay, or checking out the branch by hand and
starting the app manually. For UI-heavy changes (Hytte dashboard widgets, a
.NET API + React client app) the diff alone is a poor signal — you want to
*see* the branch running before merging.

Everything a preview needs already exists at some point in the pipeline: the
worker's branch (`forge/<beadID>`), a worktree mechanism, and an authenticated
web dashboard (Hearth 2.0). What's missing is a way to describe how a project
runs, and something to supervise it.

## Proposal

**Kiln** (`internal/kiln`): on-demand preview environments for worker branches.
A "Preview" button in Hearth 2.0 on a PR / ready-to-merge bead:

1. Materializes a detached worktree of the PR branch under `<anvil>/.previews/<beadID>`.
2. Reads a per-project manifest (`<anvil>/.forge/preview.yaml`) describing the
   services (api, client, …) and optional db setup/teardown commands.
3. Allocates ports, runs setup, starts and supervises the services, health-checks
   them, and surfaces per-service status + an "Open preview" link in Hearth 2.0.
4. Tears the whole thing down on request, on idle timeout, or when the PR
   merges/closes.

Design principles:

- **On-demand, not automatic.** Previews cost real memory (SQL Server + dotnet
  + vite per preview); most PRs merge without one. Auto-preview-on-ready is a
  later opt-in flag.
- **No containers.** Forge supervises local subprocesses everywhere else
  (Smith, Temper, hooks); Kiln does the same. This keeps Windows and the
  single-pod k8s deployment story intact. Databases are handled by
  setup/teardown commands against an always-running server (e.g. one shared
  MSSQL instance, one database per preview), not per-preview containers.
- **Forge stays agnostic.** Like hooks and temper commands, Kiln only runs the
  commands the manifest declares and injects context via `FORGE_*` env vars.
  No framework detection, no docker-compose parsing.
- **Previews never touch the pipeline.** The worker worktree lifecycle
  (`pipeline.go` create → … → `worktree.Remove`) is unchanged. Kiln uses its
  own directory and a detached checkout, so it can't collide with a live
  worker worktree on the same branch, and works fine after the worker worktree
  is long gone (the branch survives on origin; `git fetch` + detached checkout
  at its tip).

## Manifest Format

Implemented in `internal/kiln`; the user-facing reference lives in
[docs/preview-manifest.md](../preview-manifest.md).

`<anvil>/.forge/preview.yaml`, next to `quests/` and `plans/`. **Loaded from
the anvil's main checkout, not from the PR branch** — a PR must not be able to
change what commands Kiln executes. (Consequence: a PR that changes how the
app starts isn't previewable until its manifest change lands on main.
Acceptable; call it out in docs.)

```yaml
# .forge/preview.yaml
version: 1

# Optional, run once before services start / after they stop.
# Typical use: create + migrate a per-preview database, drop it on teardown.
setup: "./scripts/preview-db-setup.sh"
teardown: "./scripts/preview-db-teardown.sh"

services:
  api:
    command: "dotnet run --project src/Api --no-launch-profile"
    dir: "."                        # relative to the preview worktree
    env:
      ASPNETCORE_URLS: "http://127.0.0.1:{{.Port}}"
      ConnectionStrings__Default: "Server=localhost;Database=app_preview_{{.PreviewID}};..."
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

- Every service gets an allocated port, exposed to its command/env as
  `{{.Port}}` and to all services as `{{.ServicePort "name"}}` /
  `FORGE_PREVIEW_PORT_<NAME>`.
- All commands (setup, teardown, services) run with cwd = the service `dir`
  inside the preview worktree and receive: `FORGE_PREVIEW_ID` (sanitized bead
  id, safe for db names), `FORGE_BEAD_ID`, `FORGE_BRANCH`,
  `FORGE_WORKTREE_PATH`, `FORGE_ANVIL_NAME`, `FORGE_ANVIL_PATH` — the same
  vocabulary as pipeline hooks.
- `entry: true` marks the service whose URL is shown in Hearth and used as
  `{{.BaseURL}}` for quests (Phase 3). Default: the sole service, else required.
- Health: `starting → healthy` when the check passes within `ready_timeout`,
  else `failed` (other services keep running; the panel shows per-service state).

## Architecture

```
Hearth 2.0 (React SPA)
  → POST /api/bead/{id}/preview/start        → 202 {request_id, poll_url}
  → GET  /api/previews                        → list + per-service status/ports
  → GET  /api/preview/{id}/log/{service}      → log tail
  → POST /api/bead/{id}/preview/stop
      ↓ in-process (same pattern as other Hearth write actions)
kiln.Manager (started by daemon, like questgiver.Monitor)
  ├─ registry: beadID → *kiln.Preview  (also persisted to state.db `previews`)
  ├─ caps: preview_max_concurrent (default 2); LRU-stop or reject when full
  ├─ idle reaper: preview_idle_timeout (default 30m) since last start/URL open
  ├─ startup reconciliation: kill orphan PIDs from state.db, prune
  │    <anvil>/.previews/* (mirrors shutdown.go orphan worktree cleanup)
  └─ per preview:
       worktree: git fetch origin <branch> && git worktree add --detach
                   <anvil>/.previews/<beadID> <tip>
       ports:    allocate from preview_port_range (default 42000–42999)
       setup → start services (executil, process groups) → health checks
       teardown: stop process groups → run teardown cmd → remove worktree
bellows.OnEvent(PR merged/closed) → kiln.Stop(beadID)   (auto-teardown)
```

Key implementation notes:

- **Detached worktree, new helper.** `worktree.CreateFromBranch` exists but is
  resume-specific (exact-path requirement for `claude --resume`, refuses when
  branch is checked out elsewhere). Kiln needs a small new
  `worktree.CreateDetached(ctx, anvilPath, beadID, branch)` → fetch, resolve
  tip, `git worktree add --detach`, reusing `validateBranchName`,
  `CleanStaleCoreWorktree`, `assertOnMainBranch`. Detached HEAD sidesteps
  git's "branch already checked out" error when the worker worktree still
  exists.
- **node_modules junction:** reuse the existing junction logic
  (`worktree/nodemodules.go`) so `npm run dev` works instantly; document that
  a manifest must not run `npm ci` in a junctioned dir (same guard Temper has).
  If the PR changes package.json, the manifest's service command is the place
  to handle it (or skip junction via manifest flag).
- **Process supervision:** `exec.CommandContext` + platform process groups via
  `internal/executil` (kill the whole tree, Windows included). Stdout/stderr
  → `~/.forge/logs/<beadID>/preview-<service>.log` (picked up by the existing
  bead-logs browser and logsweep retention).
- **State:** new `previews` table in state.db: bead_id, anvil, branch, status,
  worktree_path, ports/services JSON, pids JSON, created_at, last_active_at.
  Survives daemon restart for reconciliation; feeds `GET /api/previews`.
- **URL surfacing (v1):** services bind `127.0.0.1` by default;
  `preview_bind_host` config lets a LAN/VPN setup use `0.0.0.0`, and
  `preview_public_host` sets the hostname used in displayed links (e.g. the
  box's WireGuard/LAN name). This bypasses Hearth's login — fine for a
  private box, and the honest v1. A built-in authenticated reverse proxy is
  deliberately deferred (path-prefix proxying fights SPAs; subdomain proxying
  needs DNS/TLS anyway — on a Caddy box, a static wildcard site block is
  simpler than anything Forge could ship).

## Configuration

Global (`~/.forge/config.yaml` settings):

```yaml
preview_enabled: true          # master gate, default false
preview_max_concurrent: 2
preview_idle_timeout: 30m
preview_port_range: "42000-42999"
preview_bind_host: "127.0.0.1"
preview_public_host: ""        # hostname for links; default = request Host
```

Per-anvil (`AnvilConfig`): `preview_enabled *bool` (tri-state like
`questgiver_enabled`). An anvil without `.forge/preview.yaml` simply shows no
Preview button.

## Hearth 2.0 UI

- **Preview button** on ready-to-merge cards, PR rows, and the bead detail
  page — visible when the anvil has a manifest and the bead has a surviving
  branch/PR. Click → 202/poll pattern → button becomes a status chip
  (`starting… / healthy / failed`).
- **Preview panel** (bead detail): per-service rows — name, port, health,
  uptime, log-tail link — plus "Open preview" (entry URL, new tab) and "Stop".
- **Previews overview**: small section (Workers pane or status bar) listing
  active previews with idle countdown, so nothing runs forgotten.
- TUI (Hearth 1.0) parity is out of scope; add IPC commands only if wanted
  later (`preview_start`, `preview_stop`, `previews`).

## Phases & Bead Breakdown

Each item below is intended to be one bead; Phase 1 items are sequenced by
dependency (1 → 2/3 in parallel → 4 → 5).

**Phase 1 — MVP (manifest → running preview → link in Hearth)**

1. **kiln: manifest + config plumbing.** `internal/kiln`: `preview.yaml`
   schema, loader + validation (from anvil main checkout), template expansion
   (`{{.Port}}`, `{{.ServicePort}}`, `{{.PreviewID}}`, `{{.Host}}`); global
   settings + per-anvil `preview_enabled` in `internal/config`; docs in
   docs/configuration.md.
2. **worktree: detached preview checkouts.** `CreateDetached` +
   `RemoveDetached` under `<anvil>/.previews/`, junction reuse, tests
   covering "worker worktree still exists on same branch" and "branch only on
   origin".
3. **kiln: runtime.** Port allocator (range, collision-safe), process
   supervision via executil process groups, health checks, per-service logs
   under `~/.forge/logs/<beadID>/`, `previews` state.db table + migrations.
4. **daemon: kiln.Manager.** Registry, concurrency cap, idle reaper,
   setup/teardown execution, startup reconciliation (orphan PIDs + stale
   `.previews` dirs), bellows OnEvent hook for teardown on PR merge/close,
   daemon wiring behind `preview_enabled` (mirror questgiver startup).
5. **web + frontend: preview UX.** Routes (`preview/start`, `preview/stop`,
   `GET /previews`, log tail) using the 202+request_id pattern; Preview
   button, status chip, preview panel, previews overview in the SPA.

**Phase 2 — polish & safety**

6. **DB lifecycle recipes + reference manifests.** Documented, tested example
   manifests: Hytte (Go api + Vite client) and a .NET + React + MSSQL app
   (setup script creating `app_preview_<id>` + running EF migrations,
   teardown dropping it). Ships as docs + Hytte's real `.forge/preview.yaml`.
7. **Resource hardening.** Idle-timeout tuning, LRU eviction when the cap is
   hit, memory note surfaced in the panel, `forge preview list|stop` CLI for
   recovery without the web UI.

**Phase 3 — leverage**

8. **Adventurer vs. previews.** When a preview is healthy, allow QuestGiver
   to run the anvil's quests against the preview's entry URL
   (`{{.BaseURL}}` already exists in quest templating) and attach results +
   screenshots to the PR (via the existing ingot/assay comment machinery).
   This is per-PR browser E2E — arguably the biggest payoff of the feature.
9. **Auto-preview on ready-to-merge.** Per-anvil opt-in
   (`preview_auto: ready_to_merge`), still subject to cap + idle reaper.

## Out of Scope (deliberately)

- Containers / docker-compose / devcontainers — contradicts Forge's
  no-container model and the k8s single-pod deployment.
- Built-in reverse proxy & auth for preview URLs — revisit only if Forge
  grows genuinely multi-user; a Caddy wildcard block covers the current need.
- Per-preview database *servers* — one shared server + per-preview databases
  via setup/teardown commands.
- Previewing uncommitted worker state mid-run — previews always build from
  the branch tip.
