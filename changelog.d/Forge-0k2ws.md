category: Fixed
- **gitguard tests pass inside a Forge worker** - The `internal/gitguard` test fixtures now resolve the real `git` by skipping any copy of the installed guard on `PATH`, so `TestTheFaultTheGuardExistsFor` no longer has its deliberately unguarded setup step refused by the guard it is testing when `go test` runs in a worker worktree. (Forge-0k2ws)
