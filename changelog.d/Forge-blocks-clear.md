category: Fixed
- Crucible no longer misidentifies children as parents when both are in the poll batch — raw `blocks` field (child→parent direction) is now cleared before reconstruction, preventing the inverted parent-child relationship
- Removed ResolveBlocks entirely — bd show's dependents array lists beads that depend on me (parents I block), not children, causing systematic crucible inversion
