# GitHub Copilot PR Review Instructions for Forge

This repository orchestrates autonomous AI agents (Claude Code) across git repositories. When reviewing Pull Requests in this repository, please adhere to the following guidelines.

## Canonical Sources
Always reference `AGENTS.md` and `CLAUDE.md` in the root of the repository as the canonical sources for project guidelines, workflows, and AI instructions.

## Architecture Metaphor
Forge uses a **blacksmith metaphor** for its architecture. Please understand and use this terminology:
- **Hearth**: The daemon process and TUI dashboard.
- **Smith**: The implementation worker (a Claude Code session).
- **Temper**: Build, lint, and test verification.
- **Warden**: The review agent that validates Smith's output.
- **Bellows**: Monitors open PRs for CI failures and review comments.
- **Poller**: Fetches available work (beads).

## What to Look For (Focus Areas)
When reviewing code, pay special attention to:
- **Concurrency issues**: Data races or unsynchronized access to shared resources.
- **Goroutine leaks**: Ensure all spawned goroutines have a clear exit path.
- **state.db transaction safety**: Verify that SQLite operations handle transactions correctly and safely.
- **Provider fallback chain correctness**: Check that provider fallbacks behave correctly and exhaustively.
- **Worktree cleanup on error paths**: Ensure `git worktree` instances are always cleaned up, even when errors occur or processes panic/fail.

## Patterns to Avoid (Flag These)
Please flag any code that introduces the following:
- **Breaking IPC protocol backward compatibility**: The named pipe/Unix socket JSON protocol must remain compatible.
- **Adding interactive prompts**: Shell commands and child processes must run non-interactively (e.g., no `y/n` prompts).
- **Skipping graceful shutdown hooks**: All background processes and worktrees must participate in graceful shutdown on `SIGINT`.

## Intentional Patterns (DO NOT Flag)
The following are intentional design choices in this project. **Do not flag them as issues or suggest alternatives:**
- **Viper config with env var binding**: We use Viper for configuration management and environment variable binding.
- **Manual Go error wrapping**: We use manual error wrapping (e.g., `fmt.Errorf("...: %w", err)`) instead of third-party libraries.
- **SQLite WAL mode**: Our `state.db` uses Write-Ahead Logging (WAL) mode by design.
