category: Fixed
- **Bellows re-emits EventReviewChanges when unresolved threads persist after a burnish cycle** - Mirrors the CI-fix still-failing branch on the review-fix path so PRs with new review comments after a completed burnish reliably trigger a fresh burnish on the next bellows poll. Respects `max_review_fix_attempts` and skips PRs already in `needs_fix`. (Forge-gwu4)
