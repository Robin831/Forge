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
// is the whole point of extracting it.
//
// Convention for the worker shapes below: each predicate group declares the
// minimal structural subset of api.ts's WorkerInfo it inspects, immediately
// above the predicates that take it. Structural subsets rather than imports of
// WorkerInfo, so the direction of the dependency stays one-way (api.ts may
// grow a field without this module noticing); declared beside their users
// rather than collected at the top, so a reader adding a predicate has one
// precedent to copy instead of three.

// DispatchWorker is the minimal shape the slot predicates inspect, so any of
// api.ts's worker DTOs can be passed directly.
export interface DispatchWorker {
  status: string
  phase?: string
}

// DISPATCH_STATUSES mirrors state.dispatchStatuses exactly — the status axis of
// ActiveDispatchWorkers, the query that decides whether the daemon has a free
// Smith slot. Exactly, including 'monitoring': the daemon counts a monitoring
// row whose phase is not a background one, and a UI set that quietly left the
// status out could not be checked against the daemon's list at all — the one
// status where the two are documented to be able to disagree would have been
// the one the guard structurally could not see.
//
// Kept in step with the Go side by TestFrontendDispatchStatusesMatchDispatchQuery
// (internal/state/workerstatus_frontend_test.go), which reads this file and
// compares it against the query's own status list — a TS-only assertion could
// never catch the Go list moving, which is the drift that produced the omitted
// 'stalled' in the first place.
export const DISPATCH_STATUSES = new Set([
  'pending',
  'running',
  'reviewing',
  'monitoring',
  'stalled',
  'paused',
])

// MONITORING_STATUS is the one member of DISPATCH_STATUSES that gets no live
// panel. A row reaches it from two directions and neither streams a claude
// transcript worth a panel: bellows upserts a pseudo-worker per open PR that
// holds no PID and no log at all, and a pipeline flips its own row here the
// moment Warden approves — before the push, before the PR is created, before
// the worktree goes (state.WorkerStatus.IsMonitorOnly documents that second
// case, which is why "monitoring" is not on its own a licence to call a row
// idle). Slot accounting reads DISPATCH_STATUSES and so keeps both.
const MONITORING_STATUS = 'monitoring'

// SLOT_STATUSES is the panelling axis: the dispatch statuses whose workers also
// stream output worth a live panel. It is DERIVED from DISPATCH_STATUSES rather
// than written out again, so the two can only ever differ by the one status
// named above.
export const SLOT_STATUSES = new Set(
  [...DISPATCH_STATUSES].filter((s) => s !== MONITORING_STATUS),
)

// BACKGROUND_PHASES is the second axis of the same query: state's
// backgroundPhases, the phases excluded from dispatch capacity because they
// are not Smith work. A quench/burnish/rebase/assay worker has a real id, a
// real log path and status 'running', so nothing but the phase tells it apart
// from a dispatched Smith — reading the status alone counted every lifecycle
// fix worker against max_total_smiths and under-reported the idle slots the
// daemon would happily dispatch into.
//
// 'ready_to_merge' is in the set but in no SQL list, because it is in no
// database row either: the workers IPC handler substitutes it for a monitor's
// stored 'bellows' once its PR meets every merge condition
// (state.PhaseDisplayRewrite), so it exists only in the payload — which is the
// vocabulary this file reads. Guarded against the Go side by the same
// cross-language test as DISPATCH_STATUSES, which compares this set against the
// SQL list plus those rewrite targets.
export const BACKGROUND_PHASES = new Set([
  'bellows',
  'ready_to_merge',
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

// isSlotStatus reports whether a status gets a live panel. It is the panelling
// axis, deliberately wider than the capacity one — use holdsDispatchSlot to ask
// whether a worker occupies a Smith slot.
export function isSlotStatus(status: string): boolean {
  return SLOT_STATUSES.has(status)
}

// isBackgroundPhase reports whether a phase is excluded from dispatch
// capacity. A worker with no phase recorded is not background: an unstamped
// row is a Smith row, and treating "unknown" as background would hide a real
// worker from the slot count.
//
// That agrees with the daemon rather than merely resembling it. Its SQL is
// `phase NOT IN (...)`, which in SQLite yields NULL — and so excludes the row —
// for a NULL phase, the opposite of what is written here. It never fires:
// workers.phase is `TEXT NOT NULL DEFAULT ''` in both the CREATE TABLE and the
// add-column migration (pinned by TestWorkersPhaseColumnIsNotNull), so the
// column holds '' where a row was never stamped, '' compares normally and the
// daemon counts it exactly as this does. An absent `phase` in the payload is
// that empty string arriving over JSON.
export function isBackgroundPhase(phase: string | undefined): boolean {
  return !!phase && BACKGROUND_PHASES.has(phase)
}

// holdsDispatchSlot reports whether a worker occupies one of the
// max_total_smiths slots, on both of the axes state.ActiveDispatchWorkers
// filters on. It is what every idle-slot count is derived from, so the
// dashboard's "N idle" and the daemon's next dispatch decision agree.
export function holdsDispatchSlot(w: DispatchWorker): boolean {
  return DISPATCH_STATUSES.has(w.status) && !isBackgroundPhase(w.phase)
}

// Killable is the minimal worker shape the kill gate inspects — a subset of
// WorkerInfo / BeadDetailWorker so either can be passed directly.
export interface Killable {
  status: string
}

// KILLABLE_STATUSES is the set of worker statuses for which the dashboard
// offers a kill control. The daemon applies NO status gate of its own —
// killWorkerProcess reads the PID off the worker row and signals it whatever
// the row says — so this set is a judgement about which statuses it is useful
// to offer the button on, not a mirror of a server-side check.
//
// 'stalled' is on it because that is the status an operator most often wants
// to kill: the watchdog sets it when a worker's log goes quiet while its
// PROCESS is still running, which is exactly the condition an operator watches
// a panel to end. It was omitted while a stalled worker had no panel at all;
// once the panel stayed visible (Forge-wl5s), a live row was left with its kill
// button hidden and the daemon perfectly willing to accept the kill.
//
// A terminal row has nothing to signal, and 'paused' has resume as its verb, so
// neither is offered here.
const KILLABLE_STATUSES = new Set(['pending', 'running', 'stalled'])

// canKillWorker reports whether the kill control should be offered for a
// worker. It is one function rather than an expression per surface because it
// was two: WorkerPanel and WorkersPane each inlined
// `status === 'pending' || status === 'running'`, so 'stalled' had to be missed
// twice and would have to be added twice.
export function canKillWorker(worker: Killable | null | undefined): boolean {
  return !!worker && KILLABLE_STATUSES.has(worker.status)
}

// Steerable is the minimal worker shape the steer gates inspect — a subset of
// WorkerInfo / BeadDetailWorker so either can be passed directly.
export interface Steerable {
  status: string
  session_id?: string
  model?: string
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
//   - stalled — the watchdog's status is a MASK the daemon's steer path never
//     reads: MarkWorkerStalled writes it over whatever the row held while the
//     pipeline goroutine runs on untouched, still selecting on its steer
//     mailbox. Delivery does not depend on the spawn being responsive either —
//     smith.Process.Interrupt signals the process group (SIGINT, then SIGKILL
//     past the grace period) and the captured session_id is resumed with the
//     steer message — so a spawn that has gone quiet is interrupted exactly as
//     a chatty one is. Excluding it disabled the composer with a reason
//     ('No active pipeline') that the daemon does not act on and that a stalled
//     worker, whose process is still running, contradicts.
//
// None of these promise acceptance: the daemon's gate is a live control handle,
// which no status can prove (a 'running' lifecycle fix worker holds none
// either). A status the daemon would never accept must not be enabled, and one
// it may accept is enabled and left to answer for itself.
const STEERABLE_STATUSES = new Set([
  'running',
  'pending',
  'reviewing',
  'paused',
  'stalled',
])

// isSteerTargetStatus reports whether a worker in this status is the one a
// bead's steer composer should be aimed at. It is the same set the reason above
// reads, exported because BeadDetailPage asks the question one step earlier —
// it picks the bead's steerable worker out of a list before any reason can be
// derived — and it had the set written out again inline. Two copies is how
// 'stalled' came to be missing from the steer matrix in three places at once.
export function isSteerTargetStatus(status: string): boolean {
  return STEERABLE_STATUSES.has(status)
}

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
// a paused-but-parked pipeline, or one of those masked 'stalled' by the
// watchdog) and a Claude session — only Claude reports a
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

// Finishable is the minimal worker shape the terminal-panel gate inspects — a
// subset of WorkerInfo / BeadDetailWorker so either can be passed directly.
export interface Finishable {
  status: string
  completed_at?: string
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
export function isFinishedWorker(w: Finishable): boolean {
  return FINISHED_WORKER_STATUSES.has(w.status) && !!w.completed_at
}

// WORKER_STATUS_CLASSES styles the per-worker status chip. Shared so the chip
// reads identically wherever a worker status is rendered — the Workers pane, a
// live worker panel and the History page held three copies, and they had come
// apart in both directions ('reviewing' existed in one only; 'done' was
// emerald on History and sky on the live surfaces, so one status was two
// colours depending on the page an operator was looking at).
//
// Every terminal status has a key here — 'killed' included, which none of the
// three copies ever styled even though `kill_worker` / `forge queue stop` is a
// routine operator action, so a killed worker read as an unrecognised status on
// every surface. Folding three maps into one is only a consolidation if the
// result covers each of their vocabularies, and 'timeout' was added to this one
// for exactly that reason; the vitest suite beside this file now pins
// FINISHED_WORKER_STATUSES against the keys so the next terminal status cannot
// be missed the same way. 'monitoring' is deliberately absent: it is not
// terminal, and the neutral fallback is what all three copies drew it with.
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
  killed: 'bg-red-500/20 text-red-300 border-red-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
}

export function workerStatusClass(status: string): string {
  return WORKER_STATUS_CLASSES[status] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}
