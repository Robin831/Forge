category: Added
- **Per-anvil conventions injection in Smith prompt** - Smith now reads `.forge/conventions.md` from each anvil and injects it as a `## Project Rules (Non-Negotiable)` section between Instructions and Escalation, giving project-specific rules a proactive upfront slot instead of relying on reactive lookups into appended AGENTS.md/CLAUDE.md context. (Forge-lr5j)
