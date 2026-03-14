category: Changed
- **Refactored GitHub PR operations behind VCS provider interface** - The `ghpr` package functionality is now available as a GitHub implementation of the `vcs.Provider` interface (`internal/vcs/github`). All callers (daemon, bellows, crucible, reviewfix) now use the `vcs.Provider` abstraction, enabling future support for GitLab, Forgejo, Bitbucket, and Azure DevOps. (Forge-co9n)
