category: Added
- **Copilot premium request tracking and daily limit** - Tracks weighted premium requests per Copilot model (e.g. opus 4.6 = 3x, haiku 4.5 = 0.33x) and enforces a configurable daily limit via `copilot_daily_request_limit`. When exceeded, the Copilot provider is skipped in the fallback chain while other providers remain available. (Forge-dq2)
