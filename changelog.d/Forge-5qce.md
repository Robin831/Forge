fix: depcheck aborts scan when git pull fails on anvil (Forge-5qce)

depcheck already attempted `git pull --ff-only` before scanning, but on
failure it only logged a warning and scanned anyway. Scanning stale
dependency files caused duplicate beads for updates that were already
merged — Forge would re-detect the old versions and create new beads for
work already done.

The pull failure is now fatal for that anvil's scan cycle: depcheck logs
an EventDepcheckFailed event to the event log and skips the scan
entirely. The next scheduled cycle will retry.
