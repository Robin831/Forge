category: Added
- **Skip Warden review for small Copilot diffs** - New opt-in setting `copilot_skip_warden_small_diffs` auto-approves small, low-risk diffs when the primary provider is Copilot, saving one premium request per skipped review. Applies when all criteria are met: ≤100 lines changed, docs/tests-only or ≤2 files, no security-sensitive paths, and P3+ priority. (Forge-mvqd)
