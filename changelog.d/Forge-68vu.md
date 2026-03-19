category: Fixed
- **Bellows no longer flags CI as failed while checks are still running** - Previously, bellows would emit `ci_failed` events when any CI check had a non-success conclusion, including checks that were still in progress. Now bellows waits until all checks have completed before evaluating CI status, preventing false failure events and unnecessary cifix attempts. (Forge-68vu)
