category: Added
- **VCS provider abstraction layer** - Introduced `internal/vcs` package with a `Provider` interface that abstracts PR operations (create, merge, check status, list). GitHub implementation wraps existing `ghpr` package. Per-anvil `platform` config field added (default: `github`). Prepares Forge for GitLab, Gitea/Forgejo, Bitbucket, and Azure DevOps support. (Forge-m89a)
