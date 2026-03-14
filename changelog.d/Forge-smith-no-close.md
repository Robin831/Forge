category: Fixed
- Smith no longer closes beads — the orchestrator now explicitly instructs agents not to call `bd close`, preventing dependent beads from being unblocked before the PR merges
