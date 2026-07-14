category: Added
- **Event Bus toggle** - Added `settings.bus_enabled` / `settings.bus_buffer_size` config (plus `--enable-bus` and `--bus-buffer-size` daemon flags) to gate the in-process event Bus vs legacy polling. Disabled by default for safe rollout; when off, no Bus is constructed and event publishing no-ops. (Forge-e4f4)
