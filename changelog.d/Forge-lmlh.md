category: Added
- **Data layer testability via bdExecFunc injection** - Introduced `bdExecFunc` type and `fetchAllBeadsWithExec` internal function to allow dependency injection of the bd CLI executor, enabling unit tests for `FetchAllBeads` without spawning real processes. (Forge-lmlh)
