category: Added
- **Multi-language dependency update scanner** - New `forge deps check` command and background depcheck monitor now scan Go, .NET/NuGet, and npm ecosystems for outdated dependencies. Patch/minor updates are grouped into single beads per ecosystem; major version bumps get separate beads. Per-anvil opt-out via `depcheck_enabled: false`. (Forge-m94)
