// Worker status vocabulary shared by every live-worker surface.
//
// It is one module rather than one copy per component because the copies had
// already drifted: WorkerPanelGrid, WorkersPane and WorkerPanel each carried
// their own "which statuses hold a Smith slot" list, and none of them included
// 'stalled'. The watchdog marks a worker stalled while its process is still
// running — the daemon keeps counting it against max_total_smiths
// (state.ActiveDispatchWorkers) and keeps returning it from ActiveWorkers — so
// a UI that drops it makes the panel vanish exactly when an operator wants to
// watch it, and reports one idle slot more than the daemon will actually
// dispatch into.

// SLOT_STATUSES are the worker statuses that occupy a dispatch slot and stream
// output worth a live panel. It mirrors state.ActiveDispatchWorkers' status
// list (minus 'monitoring', which is the bellows PR-monitor pseudo-worker and
// is filtered out separately — it produces no claude log):
//   - pending / running — a just-started or live Smith spawn,
//   - reviewing         — the Warden pass, which holds the slot too,
//   - paused            — a parked pipeline still holding its worktree,
//   - stalled           — the watchdog's "no output for a while" flag. The
//     process is alive and the slot is held; it flips back to running on
//     worker_recovered.
export const SLOT_STATUSES = new Set([
  'pending',
  'running',
  'reviewing',
  'paused',
  'stalled',
])

// isSlotStatus reports whether a status holds a dispatch slot. Callers that
// also need the bellows-monitor exclusion use WorkerPanelGrid's isSlotWorker.
export function isSlotStatus(status: string): boolean {
  return SLOT_STATUSES.has(status)
}

// WORKER_STATUS_CLASSES styles the per-worker status chip. Shared so the chip
// reads identically in the Workers pane and on a live worker panel — they held
// two copies that had already come apart ('reviewing' existed in one only).
//
// 'stalled' is orange rather than amber to reuse the colour HistoryPage and
// IngotsPage already give it, and because amber is 'paused' here: a worker the
// operator parked and a worker that stopped producing output are different
// enough that the chip must not read the same.
export const WORKER_STATUS_CLASSES: Record<string, string> = {
  pending: 'bg-slate-700/60 text-slate-200 border-slate-600/60',
  running: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  reviewing: 'bg-cyan-500/20 text-cyan-300 border-cyan-500/40',
  paused: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  stalled: 'bg-orange-500/20 text-orange-300 border-orange-500/40',
  done: 'bg-sky-500/20 text-sky-300 border-sky-500/40',
  // partial is its own chip, never the done or failed one: an Assay run whose
  // passes only half covered the head produced real findings but not a full
  // review, and either of the other two chips would say otherwise.
  partial: 'bg-amber-500/20 text-amber-200 border-amber-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
}

export function workerStatusClass(status: string): string {
  return WORKER_STATUS_CLASSES[status] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}
