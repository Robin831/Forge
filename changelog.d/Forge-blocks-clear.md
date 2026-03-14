category: Fixed
- Crucible no longer misidentifies children as parents when both are in the poll batch — raw `blocks` field (child→parent direction) is now cleared before reconstruction, preventing the inverted parent-child relationship
- ResolveBlocks now filters out closed dependents, preventing beads with closed parents from being treated as crucible candidates
