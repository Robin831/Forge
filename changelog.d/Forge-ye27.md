category: Added
- **Queue API exposes bead timestamps** - The `/api/queue` endpoint and its IPC backing now include `created_at` and `updated_at` for each item, sourced from the in-memory poller snapshot so no SQLite migration is needed. Unblocks date-column work on the Hearth queue view. (Forge-ye27)
