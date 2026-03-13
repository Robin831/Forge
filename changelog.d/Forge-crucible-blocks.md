category: Fixed

- Fix poller Blocks reconstruction treating depends_on relationships as parent-child edges — only blocks and parent-child dependency types should be used, preventing the crucible from incorrectly adopting downstream beads as children (Forge-crucible-blocks)
- Defer bead close when downstream beads depend on it (depends_on) — previously only blocks-type children triggered deferred close, so depends_on dependents could start before the PR was merged (Forge-crucible-blocks)
