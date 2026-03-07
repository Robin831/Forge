category: Fixed
- **Fix bead and PR retry not re-triggering workers immediately** - Fixed an issue where retrying an exhausted PR (e.g., Rebase exhausted) or a circuit-broken bead would not trigger a new worker cycle immediately because no immediate poll or refresh was triggered. All retry actions now trigger an immediate poll or Bellows refresh. (Forge-u55)
