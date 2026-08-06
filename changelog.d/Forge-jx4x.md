category: Added
- **`preview_list` IPC command** - The preview listing is now served under the CLI's `preview_list` name as well as the dashboard's `previews`, and each preview carries its entry URL, entry port, idle countdown and a resource note summarising the services and ports it holds. (Forge-jx4x)
- **`preview_stop` reports an unknown bead** - Stopping a bead that has no live preview now fails with `no preview running for bead <id>` instead of silently reporting success, and the completed request carries a structured `{stopped, bead_id}` payload. (Forge-jx4x)
