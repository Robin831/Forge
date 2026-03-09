category: Fixed
- **Track provider quota from all claude sessions** - Warden, cifix, reviewfix, and schematic now persist rate-limit quota data to state.db via UpsertProviderQuota, matching the existing smith behavior. Previously only smith sessions reported quota, causing the dashboard to undercount actual provider usage. (Forge-g5m)
