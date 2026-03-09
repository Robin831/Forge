category: Fixed
- **Orphan recovery no longer resets non-Forge beads** - Removed fallback in `RecoverOrphanedBeads()` that treated beads without a Forge worker record as orphan candidates after 15 minutes. Beads set to in_progress by humans or external tools are now always left untouched. (Forge-10c)
