category: Fixed
- **Rate-limit backoff now visible in event log** - When all providers are rate limited, a `rate_limited` event is logged to the event store with the expected retry time (e.g. "Hytte-toa rate limited, will retry at 22:47"), making the backoff visible in `forge history events` and the hearth TUI. (Forge-sgzu)
