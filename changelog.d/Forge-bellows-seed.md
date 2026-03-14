category: Fixed
- Bellows now detects pre-existing issues (unresolved threads, merge conflicts) on newly assigned external PRs by seeding snapshot state to force transition detection on first poll
- Assigning bellows to an external PR no longer requires a daemon restart — the snapshot cache is cleared on managed transition so seeding runs immediately
