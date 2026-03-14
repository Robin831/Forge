category: Added
- **GitLab VCS provider** - Implements the VCS provider interface for GitLab using the `glab` CLI with factory wiring via `vcs.ForPlatform()`. Supports merge request creation, merging, status checks, approval fetching, unresolved thread counting, and open MR listing. Enables anvils with `platform: gitlab` in forge.yaml. (Forge-1363)
- **GitHub VCS provider** - Wraps the existing `ghpr` package as a `vcs.Provider` implementation, so `vcs.ForPlatform("")` and `vcs.ForPlatform("github")` now return a working provider. (Forge-1363)
