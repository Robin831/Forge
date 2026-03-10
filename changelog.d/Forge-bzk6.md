category: Fixed
- **Bellows reliably retries CI fixes after failed attempts** - Bellows now directly detects when CI is still failing after a completed cifix attempt and re-emits EventCIFailed to trigger retries, rather than relying solely on snapshot cache resets which had timing issues with pending CI checks. (Forge-bzk6)
