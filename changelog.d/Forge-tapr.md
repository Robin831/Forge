category: Fixed
- **Schematic no longer exhausts turns without emitting verdict** - Strengthened prompt to require JSON output in the first response without tool use, and reduced default MaxTurns from 10 to 5 to prevent runaway investigation sessions for providers that support --max-turns (e.g. Claude CLI). (Forge-tapr)
