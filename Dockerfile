# syntax=docker/dockerfile:1.7
#
# Forge container image for Linux (skybert deployment).
#
# This image bundles the Forge daemon together with every CLI that the daemon
# and its Smith subprocesses need to operate inside a Kubernetes pod, where
# the pod filesystem is the only available source of tooling:
#
#   * forge       - built from source in the builder stage
#   * claude      - Anthropic Claude Code CLI (npm)
#   * bd          - FHI beads fork, built from source in its own stage
#                   (CGO_ENABLED=1 because bd's embedded Dolt requires CGO)
#   * gh          - GitHub CLI
#   * git         - worktree-aware
#   * node 24     - LTS, required for the npm-installed claude CLI
#   * .NET 10 SDK - used by .NET anvils for build/lint/test
#
# Build-context expectations:
#   - The full Forge source tree (cmd/, internal/, go.mod, go.sum, ...).
#   - The bd binary is no longer staged from outside; bd-builder clones
#     and compiles it inside the image build using the BEADS_REPO and
#     BEADS_REF build args.
#
# Runtime behaviour:
#   - The daemon runs in foreground mode (FORGE_FOREGROUND=1) so it survives
#     as PID 1 inside a container instead of self-detaching into the void.
#   - tini is used as PID 1 to reap zombie subprocesses (Smith spawns
#     claude, claude spawns helpers) and to forward SIGTERM cleanly so that
#     graceful shutdown runs.
#   - The daemon already performs an orphan-worktree sweep on startup
#     (internal/shutdown.CleanupOrphans), so a SIGKILL'd pod restarting from
#     the same persistent volume cleans up stale worktrees automatically.

# ============================================================================
# Stage 1a: Build the forge binary from source
# ============================================================================
FROM golang:1.26-bookworm AS forge-builder

WORKDIR /src

# Cache go module downloads on go.mod/go.sum changes
COPY go.mod go.sum ./
RUN go mod download

# Compile a static binary so it runs on the Debian runtime image without
# fragile glibc dynamic-linker dependencies.
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X github.com/Robin831/Forge/internal/forge.Version=${VERSION} -X github.com/Robin831/Forge/internal/forge.Build=${COMMIT}" \
        -o /out/forge ./cmd/forge

# ============================================================================
# Stage 1b: Build the bd (beads) binary from the FHI fork.
# ============================================================================
# Built with CGO_ENABLED=1 because bd's embedded Dolt store uses a SQLite
# (and now go-mysql-server's storage path) C binding — `bd init --remote`
# fails with "failed to open Dolt store: requires a CGO build" otherwise.
# The gms_pure_go tag is still applied (it pure-Go's the SQL parser, which
# stays useful) but CGO is what actually unblocks embedded Dolt.
#
# Override BEADS_REPO/BEADS_REF to test a different fork or branch:
#   --build-arg BEADS_REPO=https://github.com/<fork>/beads
#   --build-arg BEADS_REF=<branch-or-sha>
FROM golang:1.26-bookworm AS bd-builder

ARG BEADS_REPO=https://github.com/Robin831/beads
ARG BEADS_REF=main

RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
# Use init + fetch (not `git clone --branch`) so BEADS_REF works with EITHER
# a branch name (e.g. `main`) OR a concrete commit SHA. `git clone --branch`
# only accepts refnames (branches/tags) — passing a SHA fails with "Remote
# branch <sha> not found in upstream origin". The build pipeline pins
# BEADS_REF to a resolved SHA so podman's layer cache invalidates whenever
# the fork moves; without the init+fetch form, that pinning breaks the build.
RUN git init -b main . && \
    git remote add origin "${BEADS_REPO}" && \
    git fetch --depth=1 origin "${BEADS_REF}" && \
    git reset --hard FETCH_HEAD

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    CGO_ENABLED=1 GOOS=linux go build \
        -tags gms_pure_go \
        -trimpath -ldflags='-s -w' \
        -o /out/bd ./cmd/bd

# ============================================================================
# Stage 2: Runtime image with all CLIs the daemon and Smith subprocesses need
# ============================================================================
FROM mcr.microsoft.com/dotnet/sdk:10.0 AS runtime

# Make apt fully non-interactive so package installs never block the build.
ENV DEBIAN_FRONTEND=noninteractive

# Install OS-level dependencies, Node.js 24 (NodeSource), and the GitHub CLI
# (cli.github.com). Both third-party apt repos are added via signed-by=
# keyring entries in /etc/apt/keyrings as recommended by current Debian best
# practice — no remote scripts are piped into bash.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        gnupg \
        openssh-client \
        tini; \
    \
    # Node.js 24 LTS via NodeSource keyring + sources.list entry (no curl|bash)
    install -d -m 0755 /etc/apt/keyrings; \
    curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
        | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_24.x nodistro main" \
        > /etc/apt/sources.list.d/nodesource.list; \
    \
    # GitHub CLI via the official stable apt repo
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        | tee /etc/apt/keyrings/githubcli-archive-keyring.gpg > /dev/null; \
    chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends nodejs gh; \
    \
    # Trim apt caches so the runtime layer stays small
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# Anthropic Claude Code CLI. Installed globally so it lands on the system
# PATH and is invokable from Smith subprocesses regardless of cwd.
# Pin the version via build ARG for reproducible builds; override at build
# time with --build-arg CLAUDE_CODE_VERSION=x.y.z to upgrade intentionally.
ARG CLAUDE_CODE_VERSION=@latest
RUN npm install -g @anthropic-ai/claude-code${CLAUDE_CODE_VERSION} \
    && npm cache clean --force

# Copy the bd binary built in stage 1b. Pure-Go embedded Dolt courtesy of
# the gms_pure_go build tag — no CGO, statically linked.
COPY --from=bd-builder --chmod=0755 /out/bd /usr/local/bin/bd

# Install the standalone dolt CLI. The bootstrap initContainer needs it to
# `dolt checkout -b beads-sync` after `bd init --remote`, since the local
# clone defaults to main and pushes are rejected on main (the server has
# main checked out). Dolt is distributed as a single static Linux binary.
ARG DOLT_VERSION=1.88.1
RUN set -eux; \
    arch=$(dpkg --print-architecture); \
    case "$arch" in \
        amd64) dolt_arch=amd64 ;; \
        arm64) dolt_arch=arm64 ;; \
        *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/dolthub/dolt/releases/download/v${DOLT_VERSION}/dolt-linux-${dolt_arch}.tar.gz" \
        -o /tmp/dolt.tar.gz; \
    tar -xzf /tmp/dolt.tar.gz -C /tmp; \
    install -m 0755 "/tmp/dolt-linux-${dolt_arch}/bin/dolt" /usr/local/bin/dolt; \
    rm -rf /tmp/dolt.tar.gz /tmp/dolt-linux-${dolt_arch}; \
    dolt version

# Copy the freshly-built forge binary from the Go builder stage.
COPY --from=forge-builder --chmod=0755 /out/forge /usr/local/bin/forge

# Create an unprivileged forge user so the daemon never runs as root inside
# the pod. The home directory persists Forge config and SQLite state when
# /home/forge is mounted from a PVC.
#
# The dotnet/sdk base image ships with an existing 'app' user at UID/GID
# 1000. We delete it first so forge can claim the same numeric IDs — they
# match the K8s securityContext (runAsUser/fsGroup: 1000) and any files on
# the PVC owned by the previous forge user.
RUN set -eux; \
    if existing_user=$(getent passwd 1000 | cut -d: -f1) && [ -n "$existing_user" ]; then \
        userdel --remove "$existing_user" 2>/dev/null || userdel "$existing_user" || true; \
    fi; \
    if existing_group=$(getent group 1000 | cut -d: -f1) && [ -n "$existing_group" ]; then \
        groupdel "$existing_group" 2>/dev/null || true; \
    fi; \
    groupadd --system --gid 1000 forge; \
    useradd --system --uid 1000 --gid forge \
        --create-home --home-dir /home/forge \
        --shell /bin/bash forge; \
    mkdir -p /home/forge/.forge; \
    chown -R forge:forge /home/forge

USER forge
WORKDIR /home/forge

# FORGE_FOREGROUND=1 makes `forge up` skip its self-spawn detach so the
# daemon stays attached to PID 1 — the only mode that survives in a
# container. The CMD also passes --foreground explicitly as a belt-and-
# suspenders measure for users who override CMD.
ENV FORGE_FOREGROUND=1

# tini provides a proper PID 1: it reaps zombies (Smith → claude → helpers)
# and forwards SIGTERM cleanly so graceful shutdown runs.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/forge"]
CMD ["up", "--foreground"]
