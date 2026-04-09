# OpenCode Architecture Research

Research into [OpenCode](https://github.com/sst/opencode) (sst/opencode) patterns worth
adopting in Forge. OpenCode is a TypeScript-based AI coding agent with 140k+ GitHub stars,
offering a TUI and client-server architecture similar in spirit to Forge.

## 1. LSP Integration for Code Intelligence

### What OpenCode Does

OpenCode ships with **built-in LSP (Language Server Protocol) support** covering 60+ languages.
Key design decisions:

- **Auto-detection**: Identifies project languages via file extensions and root markers
  (`package.json`, `Cargo.toml`, `go.mod`, etc.), then spawns the appropriate LSP server.
- **Auto-download**: Missing language servers are downloaded automatically at startup
  (controllable via `OPENCODE_DISABLE_LSP_DOWNLOAD`).
- **Stdio transport**: All LSP servers communicate over stdin/stdout using the standard
  JSON-RPC protocol.
- **Diagnostics pipeline**: LSP diagnostics are collected, debounced (150ms), and published
  as events. The agent can call `waitForDiagnostics()` (3s timeout) to block until analysis
  completes before proceeding.
- **Agent-visible**: Diagnostics feed directly into agent context, so the AI knows about
  type errors, lint warnings, and other issues before deciding what to do next.

### Relevance to Forge

**Medium-high.** Forge's Temper phase runs build/lint/test as external processes and reports
pass/fail. LSP integration could provide richer, faster feedback:

- **Pre-Smith context**: Inject current diagnostics into the Smith prompt so it's aware of
  existing issues in the files it will modify.
- **Post-Smith validation**: After Smith edits files, get instant LSP diagnostics without
  running a full build cycle. This is faster than `go vet` or `dotnet build` for catching
  type errors.
- **Warden enrichment**: Give the Warden access to "did this change introduce new
  diagnostics?" as a review signal.

### Challenges

- Forge runs in headless worktrees — LSP servers need a project root and open file state.
- Managing LSP server lifecycles across multiple concurrent worktrees adds complexity.
- Go's `gopls` is heavy; spinning one up per worktree may not be practical.
- Current Temper approach (run `go build`, `go vet`, `go test`) already catches most issues.

### Recommendation

**Defer for now.** The complexity of managing LSP servers across concurrent worktrees
outweighs the benefit, given that Temper already runs the compiler and linter. Revisit if:
- Smith frequently produces type errors that waste Temper cycles.
- We add support for languages where build times are long but LSP is fast (large TypeScript
  projects).

A lighter alternative: parse `go vet` / `gopls` JSON output into structured diagnostics
that Forge can feed back to Smith more intelligently (file + line + message) rather than
raw build output.

---

## 2. Plugin System for Extensibility

### What OpenCode Does

OpenCode has a **hook-based plugin system** with two installation methods:

- **Local plugins**: JS/TS files in `.opencode/plugins/` (project) or
  `~/.config/opencode/plugins/` (global).
- **NPM packages**: Declared in `opencode.json`, auto-installed via Bun at startup.

**Plugin API surface** — plugins receive a context object with:
- `project`, `worktree`, `directory` — project context
- `client` — OpenCode SDK client for AI interaction
- `$` — shell execution capability

**Hook categories** (event-driven):
- `tool.execute.before` / `tool.execute.after` — intercept and modify tool I/O
- `file.edited`, `file.watcher.updated` — react to file changes
- `session.created`, `session.compacted`, `session.idle` — session lifecycle
- `command.executed` — command hooks
- `lsp.client.diagnostics` — LSP event hooks
- `shell.env` — inject environment variables

**Custom tools**: Plugins can define new tools with Zod schemas that become available to
the agent. Plugin tools override built-in tools with matching names.

### Relevance to Forge

**Medium.** Forge is an orchestrator, not an editor — the extensibility needs are different.
However, some patterns are worth considering:

- **Pipeline hooks**: `before_smith`, `after_smith`, `before_temper`, `after_temper`,
  `before_warden`, `after_warden` hooks would let users inject custom logic without
  modifying Forge internals. Example: run a custom linter, post to Slack, inject prompt
  context.
- **Custom Temper steps**: Currently Temper auto-detects Go/Node/.NET. A plugin could add
  support for Python, Rust, or custom build systems without core changes.
- **Anvil-level plugins**: Per-repo `.forge/plugins/` that customize behavior for that
  specific anvil.

### Challenges

- Forge is Go, not TypeScript — plugin loading is harder. Options: Go plugins (fragile),
  subprocess-based (like Hashicorp's go-plugin), or WASM.
- The current config-driven approach (per-anvil settings, prompt templates) already covers
  many customization needs.
- Plugin APIs need stability guarantees that add maintenance burden.

### Recommendation

**Adopt the hook concept, not the plugin system.** Instead of a full plugin framework:

1. **Add pipeline hooks** as shell commands in `forge.yaml`:
   ```yaml
   anvils:
     myrepo:
       hooks:
         after_smith: "./scripts/post-smith.sh"
         before_temper: "./scripts/pre-temper.sh"
   ```
   This gives extensibility without a plugin runtime. The hook script receives context
   (bead ID, worktree path, branch) as environment variables.

2. **Add custom Temper commands** per anvil:
   ```yaml
   anvils:
     myrepo:
       temper:
         build: "make build"
         test: "make test"
         lint: "make lint"
   ```

Both are simpler than a plugin system and solve the most common extensibility needs.

---

## 3. Mid-Session Model Switching

### What OpenCode Does

OpenCode's provider system supports:

- **Multiple providers**: Anthropic, OpenAI, Azure, Google, Copilot, and custom providers
  via plugins.
- **Fuzzy model matching**: `getModel()` does fuzzy lookup so users can type partial model
  names.
- **Per-agent model override**: Each agent can specify its preferred model independently.
- **Runtime switching**: Users can switch models mid-conversation via the TUI.
- **Provider fallback**: If one provider fails, the system can try the next in the chain.

The architecture separates provider registration (which APIs are available) from model
selection (which model to use for a given task).

### Relevance to Forge

**High.** Forge already has `internal/provider` with a fallback chain (Claude, Gemini,
Copilot). Several OpenCode patterns would improve this:

- **Per-stage model selection**: Use a powerful model (Opus) for Smith, a faster model
  (Sonnet/Haiku) for Warden reviews and Schematic analysis. This is partially supported
  via `smith_providers` but could be more granular.
- **Dynamic fallback on rate limits**: Forge already does this, but OpenCode's fuzzy model
  matching and auto-discovery of available models is more sophisticated.
- **Cost-aware routing**: Choose cheaper models for simpler beads (typo fixes, dep bumps)
  and expensive models for complex features.

### What Forge Already Has

- `smith_providers` config — a single provider chain shared by Smith, Warden, and Schematic (falls back to `providers` when empty)
- Provider fallback on rate limits
- Cost tracking per bead and per day

### Recommendation

**Implement cost-aware model routing.** Two concrete improvements:

1. **Per-stage provider config** — extend config to allow separate provider chains for
   each pipeline stage:
   ```yaml
   settings:
     providers:
       smith: [claude/claude-opus-4-6]
       warden: [claude/claude-sonnet-4-6]
       schematic: [claude/claude-sonnet-4-6]
       cifix: [claude/claude-sonnet-4-6]
   ```

2. **Priority-based model selection** — automatically use cheaper models for low-complexity
   beads (priority 3-4, or beads tagged as simple).

---

## 4. ACP (Agent Client Protocol)

### What OpenCode Does

ACP is **not** "Agent Communication Protocol" as hypothesized — it stands for **Agent
Client Protocol**, an open standard for communication between code editors and AI coding
agents.

- **Purpose**: Lets OpenCode run as a subprocess inside editors (Zed, JetBrains, Neovim)
  via JSON-RPC over stdio.
- **Scope**: Standardizes how an editor sends prompts, receives responses, and manages
  tool calls with an AI agent.
- **Supported editors**: Zed, JetBrains IDEs, Neovim (via avante.nvim and
  codecompanion.nvim).

Separately, OpenCode exposes an **HTTP server** (`opencode serve`) with an OpenAPI 3.1
spec, providing:
- Session management (create, list, send messages)
- File operations and search
- LSP server management
- Tool and MCP server interaction
- Server-sent events for real-time updates

### Relevance to Forge

**Low for ACP, Medium for the HTTP server pattern.**

ACP is about editor integration — Forge doesn't need to be embedded in an editor. However,
the **HTTP server with OpenAPI spec** pattern is interesting:

- Forge already has IPC via Unix socket with JSON commands. The protocol is bespoke.
- An HTTP API would enable web dashboards, mobile monitoring, CI integration, and
  third-party tooling without custom IPC clients.
- OpenAPI spec generation would auto-document the API and enable SDK generation.

### What Forge Already Has

- Unix socket / named pipe IPC with JSON protocol
- `subscribe` command for event streaming
- Hearth TUI as the primary interface

### Recommendation

**Consider HTTP API as a future enhancement.** The current Unix socket IPC works well for
local CLI and TUI communication. An HTTP API would add value when:
- Remote monitoring is needed (it partially is — see `docs/remote-access.md`)
- A web dashboard is desired
- External systems need to trigger or monitor Forge

This is a significant effort and should be a separate initiative, not a quick adoption.

---

## 5. Permission / Sandbox Model

### What OpenCode Does

OpenCode implements a **granular, rule-based permission system**:

- **Three actions**: `allow` (auto-execute), `ask` (prompt user), `deny` (block).
- **Pattern matching**: Glob-style patterns for fine-grained control:
  ```json
  {
    "bash": {
      "*": "ask",
      "git *": "allow",
      "rm *": "deny"
    }
  }
  ```
- **Per-tool permissions**: Separate rules for `read`, `edit`, `glob`, `grep`, `bash`,
  `webfetch`, `external_directory`, etc.
- **Agent-level overrides**: Each agent can have its own permission set (e.g., the Plan
  agent denies edits by default).
- **Last-match-wins**: Rules are evaluated sequentially; the last matching rule determines
  the action.
- **Deferred trust**: Users can approve "once" or "always" when prompted, with persistent
  storage of approved rules.
- **Default safety**: `.env` files are denied by default; `external_directory` access
  requires approval.

### Relevance to Forge

**Medium.** Forge operates differently — it's headless and autonomous, not interactive.
The "ask" permission level doesn't apply since there's no user to ask. However:

- **Deny rules are valuable**: Preventing Smith from modifying certain files (configs,
  secrets, CI pipelines) or running dangerous commands (`rm -rf`, `git push --force`).
- **Per-anvil restrictions**: Different repos may need different safety rules.
- **Warden integration**: The Warden could check if Smith's changes violate permission
  rules as part of review.

### What Forge Already Has

- Smith runs in isolated worktrees (natural sandbox)
- `--dangerously-skip-permissions` flag for Claude CLI (current approach)
- Warden review catches inappropriate changes
- Temper validates build/test pass

### Recommendation

**Adopt deny-list patterns for Smith.** Add per-anvil file/command restrictions:

```yaml
anvils:
  myrepo:
    smith:
      deny_patterns:
        files: ["*.env", ".forge/*", "*.key", "*.pem"]
        commands: ["rm -rf /", "git push --force*", "curl *"]
```

This would be enforced by:
1. Passing deny rules to Smith's Claude CLI session via the prompt (soft enforcement).
2. Post-Smith validation in the pipeline — check the diff doesn't touch denied file
   patterns (hard enforcement).

The diff-based check in the pipeline is more reliable than prompt-based enforcement and
integrates naturally with the existing Warden review step.

---

## Summary of Recommendations

| Area | Priority | Action |
|------|----------|--------|
| LSP Integration | Low | Defer. Temper covers this. Revisit for slow-build languages. |
| Plugin System | Medium | Add pipeline hooks (shell commands) and custom Temper commands in config. |
| Model Switching | High | Per-stage provider config and priority-based model selection. |
| ACP / HTTP API | Low | Future initiative for remote access. Current IPC is sufficient. |
| Permission Model | Medium | Add per-anvil deny patterns for files and commands. |

## Follow-Up Beads

The following beads were created for the recommended work items:

- **Forge-4nto** — Per-stage provider configuration: Allow separate model/provider chains for Smith, Warden, Schematic, and CI fix stages.
- **Forge-e179** — Pipeline hooks: Add configurable shell command hooks at each pipeline stage (before/after Smith, Temper, Warden).
- **Forge-81rq** — Custom Temper commands: Allow per-anvil override of build/test/lint commands in forge.yaml.
- **Forge-do9k** — Smith deny patterns: Per-anvil file and command deny lists enforced in the pipeline.
