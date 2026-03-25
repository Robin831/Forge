category: Fixed
- **Manual retry reset now clears warden-rejection needs_human flag** - The manual retry handler was calling `ResetDispatchFailures` (which only matches circuit-breaker rows) when `dispatch_failures > 0`, silently leaving `needs_human=1` for warden rejections. Replaced with `ResetRetry` so all needs_human states are cleared on manual reset. (Forge-5h0n)
