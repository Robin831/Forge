category: Added
- **Per-anvil VCS provider support** - Each anvil now uses its own VCS provider based on its `platform` config setting instead of hardcoding GitHub for all anvils. Supports GitHub, GitLab, and Gitea. Providers are rebuilt on config hot-reload when anvil platforms change. (Forge-cdp4)
