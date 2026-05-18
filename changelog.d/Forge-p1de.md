category: Added
- **Resolve action buttons in needs-attention panel** - Render escalation-type-specific resolve verb buttons (retry/stop/clarify/unclarify/clear) wired to `POST /api/forge/resolve` via the resolve store, with pending/error state surfaced inline and a hook for the upcoming confirmation modal to gate destructive actions. (Forge-p1de)
