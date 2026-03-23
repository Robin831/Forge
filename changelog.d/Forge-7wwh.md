category: Added
- **Wicket triage logic and AI prompt** - Implement `RunTriage` in the wicket package to call the AI provider with a formatted issue prompt, parse the JSON response into a `TriageDecision`, retry once on parse failure, and fall back to `ActionFlagHuman` on persistent failure. (Forge-7wwh)

