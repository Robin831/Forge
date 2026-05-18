category: Added
- **ResolveNeedsAttentionPanel component** - Hearth 2.0 frontend gains a panel that fetches `GET /api/forge/escalation/<bead>`, renders the full escalation message, commit/branch context, an audit-note textarea, and a draft-PR fallback form. Backed by a new `fetchEscalation`/`useEscalation` slice on the resolve store. Action buttons land in a sibling sub-task. (Forge-08lh)
