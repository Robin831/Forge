category: Added
- **Per-PR Bellows detach flag (state layer)** - New `prs.bellows_detached` column, `PR.BellowsDetached` field and `UpdatePRBellowsDetached` setter, kept strictly separate from `bellows_managed`/`bellows_manually_assigned` so a detach survives the reconcile loop's managed-flag rewrites. Foundation for `forge bellows stop/resume`. (Forge-u8ny)
