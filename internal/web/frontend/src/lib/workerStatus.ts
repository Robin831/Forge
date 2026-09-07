// The one home for worker-status vocabulary: the status sets the daemon
// defines, the predicates that read them, and the chip each status is drawn
// with.
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
//
// It lives here rather than in api.ts because api.ts is the transport layer —
// the fetch wrappers, the response DTOs and the action calls — and this is a
// reading of the daemon's own worker vocabulary that no request or response
// shape depends on. Everything that answers a question ABOUT a worker status
// belongs in this file: a reader adding a status has one place to look, which
// is the whole point of extracting it. The types below are structural
// subsets of api.ts's WorkerInfo rather than imports of it, so the direction
// of that dependency stays one-way.

// DispatchWorker is the minimal shape the slot predicates inspect, so any of
// api.ts's worker DTOs can be passed directly.
export interface DispatchWorker {
  status: string
  phase?: string
}

// Steerable is the minimal worker shape steerDisabledReason inspects — a subset
// of WorkerInfo / BeadDetailWorker so either can be passed directly.
export interface Steerable {
  status: string
  session_id?: string
  model?: string
}

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
//
// Kept in step with the Go side by TestFrontendSlotStatusesMatchDispatchQuery
// (internal/state/workerstatus_frontend_test.go), which reads this file and
// compares it against the query's own status list — a TS-only assertion could
// never catch the Go list moving, which is the drift that produced the omitted
// 'stalled' in the first place.
export const SLOT_STATUSES = new Set([
  'pending',
  'running',
  'reviewing',
  'paused',
  'stalled',
])

// BACKGROUND_PHASES is the second axis of the same query: state's
// backgroundPhases, the phases excluded from dispatch capacity because they
// are not Smith work. A quench/burnish/rebase/assay worker has a real id, a
// real log path and status 'running', so nothing but the phase tells it apart
// from a dispatched Smith — reading the status alone counted every lifecycle
// fix worker against max_total_smiths and under-reported the idle slots the
// daemon would happily dispatch into.
//
// Guarded against the Go constant by the same cross-language test as
// SLOT_STATUSES.
export const BACKGROUND_PHASES = new Set([
  'bellows',
  'quench',
  'cifix',
  'burnish',
  'reviewfix',
  'rebase',
  'assay',
  'crucible',
  'warden_rerun',
  'approve_as_is',
  'force_smith',
  'smelter',
  'depupdate',
])

// isSlotStatus reports whether a status is one the daemon counts. It is the
// status axis alone — use holdsDispatchSlot to ask the whole question.
export function isSlotStatus(status: string): boolean {
  return SLOT_STATUSES.has(status)
}

// isBackgroundPhase reports whether a phase is excluded from dispatch
// capacity. A worker with no phase recorded is not background: an unstamped
// row is a Smith row, and treating "unknown" as background would hide a real
// worker from the slot count.
export function isBackgroundPhase(phase: string | undefined): boolean {
  return !!phase && BACKGROUND_PHASES.has(phase)
}

// holdsDispatchSlot reports whether a worker occupies one of the
// max_total_smiths slots, on both of the axes state.ActiveDispatchWorkers
// filters on. It is what every idle-slot count is derived from, so the
// dashboard's "N idle" and the daemon's next dispatch decision agree.
export function holdsDispatchSlot(w: DispatchWorker): boolean {
  return isSlotStatus(w.status) && !isBackgroundPhase(w.phase)
}

// STEERABLE_STATUSES is the set of worker statuses for which the daemon accepts
// a steer. It mirrors the daemon acceptance matrix settled in the steering
// fixes (internal/daemon handleSteerBead): a steer is accepted whenever the bead
// has an active pipeline control handle, which spans the whole run —
//   - running / pending — a live or just-started Smith spawn (steer mode A:
//     interrupt the running spawn and resume the same session), and
//   - reviewing — the Warden (mode-B) queue: no spawn is live, so the message is
//     queued and consumed before the next Smith spawn.
//   - paused — the parked pipeline still holds its control handle, but a parked
//     spawn only consumes the message on resume, so the UI delivers a paused
//     steer as a resume-with-message via the resume endpoint (see
//     steerIsResumeDelivery / SteerComposer), not the steer endpoint.
const STEERABLE_STATUSES = new Set(['running', 'pending', 'reviewing', 'paused'])

// steerIsResumeDelivery reports whether a steerable worker's message must be
// delivered as a resume-with-message (via the resume endpoint) rather than a
// plain steer. Only a paused worker qualifies: its pipeline is parked awaiting a
// resume, so the message becomes the prompt the resumed Claude spawn continues
// with. Callers use this to route the submission and to phrase the affordance
// truthfully ("applies on resume") instead of implying an in-flight steer.
export function steerIsResumeDelivery(worker: Steerable | null | undefined): boolean {
  return worker?.status === 'paused'
}

// steerDisabledReason returns a human-readable reason why a worker cannot be
// steered, or null when steering is allowed. It mirrors the daemon's steer
// validation (internal/daemon workerSessionNonClaude + the active-handle check):
// steering needs an active pipeline (a running/pending Smith, a reviewing Warden,
// or a paused-but-parked pipeline) and a Claude session — only Claude reports a
// resumable session_id. A positively non-Claude session (a recorded non-claude
// model with no captured session_id) is rejected; an as-yet-unrecorded session
// (both fields empty, spawn still starting) is optimistically treated as
// steerable so a just-started Claude spawn is not falsely blocked. A paused
// worker is steerable but its message is delivered on resume — see
// steerIsResumeDelivery.
export function steerDisabledReason(worker: Steerable | null | undefined): string | null {
  const noPipeline = 'No active pipeline — steering requires an active Smith worker.'
  if (!worker) return noPipeline
  if (!STEERABLE_STATUSES.has(worker.status)) return noPipeline
  const sessionID = worker.session_id ?? ''
  const model = worker.model ?? ''
  if (sessionID === '' && model !== '' && !model.toLowerCase().includes('claude')) {
    return `Not a Claude session (model ${model}) — steering is only supported for Claude sessions.`
  }
  return null
}

// Pausable is the minimal worker shape the pause/resume gates inspect — a
// subset of WorkerInfo / BeadDetailWorker so either can be passed directly.
export interface Pausable {
  status: string
}

// pauseDisabledReason returns a human-readable reason why a worker cannot be
// paused, or null when pausing is allowed. It mirrors the daemon's paused-status
// transition table (state.CanTransitionPause): only a running worker may be
// paused. A missing worker or any non-running status is rejected.
export function pauseDisabledReason(worker: Pausable | null | undefined): string | null {
  if (!worker) return 'No active pipeline — pausing requires a running worker.'
  if (worker.status !== 'running') {
    return `Cannot pause a ${worker.status} worker — only a running worker can be paused.`
  }
  return null
}

// resumeDisabledReason returns a human-readable reason why a worker cannot be
// resumed, or null when resuming is allowed. It mirrors the daemon's
// paused-status transition table: only a paused worker may be resumed. A missing
// worker or any non-paused status is rejected.
export function resumeDisabledReason(worker: Pausable | null | undefined): string | null {
  if (!worker) return 'No paused pipeline — resuming requires a paused worker.'
  if (worker.status !== 'paused') {
    return `Cannot resume a ${worker.status} worker — only a paused worker can be resumed.`
  }
  return null
}

// FINISHED_WORKER_STATUSES are the terminal worker states whose panels linger
// as frozen transcripts for a few minutes (the /api/workers?recent= window)
// before aging out of the payload. 'partial' belongs here for the same reason
// the others do — it is terminal, and a half-covered Assay run is precisely the
// one an operator wants to read the log of, so its panel must linger rather
// than vanish on the poll that lands the status.
export const FINISHED_WORKER_STATUSES = new Set([
  'done',
  'failed',
  'timeout',
  'killed',
  'partial',
])

// isFinishedWorker reports whether a worker reached a terminal status and
// carries the completion timestamp the lingering panel's "Xm ago" caption and
// frozen elapsed time are derived from.
export function isFinishedWorker(w: { status: string; completed_at?: string }): boolean {
  return FINISHED_WORKER_STATUSES.has(w.status) && !!w.completed_at
}

// WORKER_STATUS_CLASSES styles the per-worker status chip. Shared so the chip
// reads identically wherever a worker status is rendered — the Workers pane, a
// live worker panel and the History page held three copies, and they had come
// apart in both directions ('reviewing' existed in one only; 'done' was
// emerald on History and sky on the live surfaces, so one status was two
// colours depending on the page an operator was looking at).
//
// 'stalled' is orange rather than amber to reuse the colour History and Ingots
// already give it, and because amber is 'paused' here: a worker the operator
// parked and a worker that stopped producing output are different enough that
// the chip must not read the same. 'timeout' keeps the amber History has
// always drawn it in.
//
// IngotsPage keeps its own map deliberately: its keys are ingot lifecycle
// stages (init/smith/temper/warden/approved/pr_open/pr_merged), a different
// vocabulary that merely overlaps on 'failed' and 'stalled'.
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
  timeout: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
}

export function workerStatusClass(status: string): string {
  return WORKER_STATUS_CLASSES[status] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}
