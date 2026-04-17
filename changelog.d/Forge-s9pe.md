category: Fixed
- **Immediate bead release on dispatch failure** - Pipeline dispatch failures (temper exhaustion, warden rejection, etc.) now release the bead claim immediately via bd update instead of deferring to orphan recovery, reducing latency before the bead becomes available again. (Forge-s9pe)
