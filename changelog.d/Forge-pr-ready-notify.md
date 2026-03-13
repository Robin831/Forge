category: Fixed

- Fix PR ready-to-merge webhook notifications never firing — the event condition required `HasApproval` but Copilot only submits COMMENTED reviews (never APPROVED), so the condition was never satisfied. Removed `HasApproval` from the ready-to-merge check to match the Ready to Merge panel query (Forge-pr-ready-notify)
- Fix notification context cancellation race in handleBellowsNotifications — the goroutine used the bellows polling context which could be cancelled before webhook HTTP calls completed, now uses a detached context with 30s timeout (Forge-pr-ready-notify)
- Fix CI failure not detected after review fix — bellows snapshot cache was not reset after review fix completion, so CI failing (false→false) was never a transition and no quench worker spawned (Forge-pr-ready-notify)
- Fix CI failure never detected on daemon restart — when CI was already failing with no fix attempted, bellows seeded the snapshot with ci_passing=false from the DB, matching the polled state and producing no transition. Now seeds ci_passing=true in this case to force detection (Forge-pr-ready-notify)
