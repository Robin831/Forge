category: Fixed
- **Bellows orphan-worker sweep** - Every poll cycle now transitions any bellows worker stuck in `monitoring` whose underlying PR is missing or in a terminal status (`merged`/`closed`) to `done`. This clears stranded Hearth Workers panel entries left behind when an external PR is closed on GitHub before being assigned to bellows. (Forge-ck69)
