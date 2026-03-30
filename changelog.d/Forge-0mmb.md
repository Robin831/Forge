category: Fixed
- **Worktree cleanup no longer deletes remote branch** - Removed `git push origin --delete` from `(*worktree.Manager).Remove` which was running after PR creation and deleting the remote branch the new PR depended on, causing all PRs to fail with "Head sha can't be blank". Remote branch cleanup is now delegated to GitHub's auto-delete-branch setting or Bellows after merge. (Forge-0mmb)

