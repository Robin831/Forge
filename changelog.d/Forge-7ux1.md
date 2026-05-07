category: Added
- **FORGE_FOREGROUND env var for container deployments** - Setting `FORGE_FOREGROUND=1` (or `true`/`yes`/`on`) on `forge up` skips the platform-specific detach and runs the daemon in the current process, required when running as PID 1 in a Kubernetes pod. Local behaviour is unchanged when the variable is unset. (Forge-7ux1)
