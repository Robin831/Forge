category: Added
- **Confirmation modal for destructive resolve actions** - The resolve needs-attention panel now prompts for confirmation before invoking Retry or Stop worker, reducing the chance of accidentally re-dispatching or killing a worker. Other resolve verbs (clarify, unclarify, clear) dispatch immediately as before. (Forge-2pgb)
