category: Added
- **Dependency update changelog generation** - Implement `GenerateChangelog` in `internal/depupdate/changelog.go` to produce changelog fragments for dependency batch updates, supporting both monolingual (`.md`) and bilingual (`.en.md`/`.nb.md`) projects. Fragments are git-added and committed automatically after group updates. (Forge-cvh8)
