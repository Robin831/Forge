category: Added
- **Wicket GitHubClient interface and implementation** - Add `GitHubClient` interface with `ListIssues`, `GetIssue`, `CommentOnIssue`, `AddLabels`, `RemoveLabel`, and `CloseIssue` methods, backed by the `gh` CLI. Includes `MockGitHubClient` for use in tests and `ghclient_test.go` with JSON parse tests. (Forge-plcz)
