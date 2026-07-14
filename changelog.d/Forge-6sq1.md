category: Added
- **Activity SSE poll fallback flag** - Added `settings.sse_poll_fallback` (env `FORGE_SETTINGS_SSE_POLL_FALLBACK`) to force `/api/activity/stream` back onto the legacy 2s polling loop even when the event Bus is enabled. A one-release safety valve, hot-reloadable and disabled by default; scheduled for removal next release once the bus-based stream is proven stable. (Forge-6sq1)
