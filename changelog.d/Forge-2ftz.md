category: Added
- **Cross-anvil duplicate detection via Source URL** - Wicket triage now checks all configured anvil bead databases for a matching Source URL before calling the AI. If the incoming GitHub issue was already triaged in a different anvil, it is immediately returned as a duplicate without incurring an AI call. (Forge-2ftz)

