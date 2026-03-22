category: Fixed
- **Smelter stale tracking ref** - Fixed `(stale info)` push failure after GitHub auto-deletes a merged batch warden-learn PR branch. The smelter now always creates batch worktrees from main (not the stale remote ref), and clears stale tracking refs before pushing. (Forge-owzy)
