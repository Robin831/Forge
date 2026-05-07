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
#   * bd          - FHI beads fork, copied from ./build/bd
#   * gh          - GitHub CLI
#   * git         - worktree-aware
#   * node 24     - LTS, required for the npm-installed claude CLI
#   * .NET 10 SDK - used by .NET anvils for build/lint/test
#
# Build-context expectations:
#   - The full Forge source tree (cmd/, internal/, go.mod, go.sum, ...).
#   - A pre-built Linux bd binary at ./build/bd. The accompanying build
#     script (separate bead) compiles bd from the FHI fork and stages the
#     artifact there before invoking `docker build`.
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
# Stage 1: Build the forge binary from source
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
# Stage 2: Runtime image with all CLIs the daemon and Smith subprocesses need
# ============================================================================
FROM mcr.microsoft.com/dotnet/sdk:10.0 AS runtime

# Make apt fully non-interactive so package installs never block the build.
ENV DEBIAN_FRONTEND=noninteractive

# Install OS-level dependencies, Node.js 24 (NodeSource), and the GitHub CLI
# (cli.github.com). Both third-party apt repos are pinned via keyring files
# in /etc/apt/keyrings as recommended by current Debian best practice.
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
    # Node.js 24 LTS via NodeSource setup script
    curl -fsSL https://deb.nodesource.com/setup_24.x | bash -; \
    apt-get install -y --no-install-recommends nodejs; \
    \
    # GitHub CLI via the official stable apt repo
    install -d -m 0755 /etc/apt/keyrings; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        | tee /etc/apt/keyrings/githubcli-archive-keyring.gpg > /dev/null; \
    chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends gh; \
    \
    # Trim apt caches so the runtime layer stays small
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# Anthropic Claude Code CLI. Installed globally so it lands on the system
# PATH and is invokable from Smith subprocesses regardless of cwd.
RUN npm install -g @anthropic-ai/claude-code \
    && npm cache clean --force

# Copy the bd binary from the host-staged build directory. The build script
# is responsible for compiling bd (Linux target, matching this image's arch)
# from the FHI fork at C:\source\fhigit\beads and depositing the artifact
# at ./build/bd in the docker build context.
COPY --chmod=0755 build/bd /usr/local/bin/bd

# Copy the freshly-built forge binary from the Go builder stage.
COPY --from=forge-builder --chmod=0755 /out/forge /usr/local/bin/forge

# Create an unprivileged forge user so the daemon never runs as root inside
# the pod. The home directory persists Forge config and SQLite state when
# /home/forge is mounted from a PVC.
RUN groupadd --system --gid 1000 forge \
    && useradd --system --uid 1000 --gid forge \
        --create-home --home-dir /home/forge \
        --shell /bin/bash forge \
    && mkdir -p /home/forge/.forge \
    && chown -R forge:forge /home/forge

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
