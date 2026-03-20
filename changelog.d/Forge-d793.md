category: Fixed
- **Bellows no longer flags CI as failed while checks are still in progress** - Added proper handling for GitHub StatusContext items (legacy commit status API) alongside CheckRun items in CI status evaluation. Fixed edge cases where COMPLETED checks with empty conclusions (transient state) and REQUESTED status were not detected as in-progress. (Forge-d793)
