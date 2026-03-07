category: Added

- **Changelog fragment system** - Each bead gets a `changelog.d/<bead-id>.md` fragment with category header and markdown bullets. `forge changelog assemble` collects fragments into CHANGELOG.md under [Unreleased]. Smith prompt now instructs agents to create fragments for user-visible changes. CI warns when fragments are missing on PRs. (Forge-ahj)
