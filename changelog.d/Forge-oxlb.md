category: Changed
- **All VCS consumers use the Provider interface** - Crucible, CI fix, review fix, bellows, and warden now use the `vcs.Provider` interface instead of direct `gh` CLI calls. GitLab `ResolveThread` is fully implemented. Unsafe GitHub-only fallbacks removed. (Forge-oxlb)
