category: Added
- **VCS provider interface** - Added `internal/vcs` package defining a platform-agnostic `Provider` interface for PR operations (create, merge, status, list). This enables future support for GitLab, Gitea, Bitbucket, and Azure DevOps alongside the existing GitHub integration. (Forge-9lor)
- **Anvil platform configuration** - Added `platform` field to anvil config (`github|gitlab|gitea|bitbucket|azuredevops`, default: `github`) to specify which VCS provider each repository uses. (Forge-9lor)
