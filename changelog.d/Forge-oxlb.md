category: Changed
- **All VCS consumers use the Provider interface** - Crucible, CI fix, and review fix workers now use the `vcs.Provider` interface instead of direct `gh` CLI calls, enabling multi-platform VCS support (GitHub, GitLab, etc.). (Forge-oxlb)
