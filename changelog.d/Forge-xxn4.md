fix: close bead immediately when PR merged via IPC (Forge-xxn4)

When a PR was merged from the Hearth UI (IPC merge_pr command), the
associated bead was not closed — it remained in_progress until the
watchdog's orphan recovery fired ~14 minutes later.

Root cause: UpdatePRStatus(PRMerged) was called before
bellowsMonitor.Refresh(). Bellows uses a transition check
(newSnap.IsMerged && !lastSnap.IsMerged) to emit EventPRMerged; because
the DB was already updated, both snapshots showed IsMerged=true on the
Refresh() call, so the event was never emitted and handleBeadCloseOnMerge
never ran.

Fix: call closeBead directly in the IPC merge handler (mirroring
handleBeadCloseOnMerge) rather than relying on bellows transition
detection. The bellows Refresh() call is kept for other downstream
effects (worktree cleanup, etc.).
