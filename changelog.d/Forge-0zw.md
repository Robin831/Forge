category: Fixed
- **Temper detects Node projects in subdirectories** - Temper now scans common subdirectories (web/, frontend/, client/, app/, ui/) for package.json in addition to the worktree root, enabling proper npm lint/build/test for hybrid projects like Go+Node. (Forge-0zw)
