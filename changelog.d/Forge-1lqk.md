category: Fixed
- **Force Smith no longer races with normal pipeline** - Force smith now claims the `activeBeads` slot before launching, preventing the poller from dispatching a normal pipeline run on the same bead concurrently. If the bead is already in flight when force smith is triggered, the IPC call returns an error instead of spawning a duplicate worker. (Forge-1lqk)
