// Typed client for the Kiln preview endpoints. Kept in its own module (rather
// than in api.ts) because the whole preview surface — trigger button, status
// chip, bead-detail panel, previews overview, quest runs — reads from these
// calls, and the wire shapes mirror internal/web/preview_handlers.go and
// preview_quest_handlers.go field for field.

import { ApiError, type QueuedBody } from '../api'

// PreviewServiceHealth mirrors the per-service `health` column
// (state.PreviewService*). Left open (`| string`) so an unexpected backend
// value renders instead of failing to type-check.
//
// `exited` is deliberately not folded into `failed`: a service that never came
// up and one that served for seven minutes and then died are different
// problems, and only the second one has an answer waiting in its log.
export type PreviewServiceHealth =
  | 'starting'
  | 'healthy'
  | 'failed'
  | 'exited'
  | (string & {})

// PreviewRecordStatus mirrors the preview-level `status` column
// (state.Preview*). It is the *backend* vocabulary; the UI-facing vocabulary is
// PreviewStatus below, which adds the client-only transitions.
export type PreviewRecordStatus =
  | 'starting'
  | 'running'
  | 'degraded'
  | 'failed'
  | 'stopped'
  | (string & {})

// PreviewServiceStatus mirrors Go's web.PreviewServiceStatus.
export interface PreviewServiceStatus {
  name: string
  port: number
  health: PreviewServiceHealth
  entry: boolean
  /**
   * How long this service's own process has run. It stops at the exit for a
   * service that has died, so an `exited` row reports the lifetime it had
   * rather than a clock that keeps counting over a dead process.
   */
  uptime_seconds: number
  /** GET endpoint tailing this service's log. */
  log_url: string
  /** Explains a failed service (spawn error, health timeout, early exit). */
  error?: string
  /** When the process was reaped; null while it is still running. */
  exited_at?: string | null
  /** Its exit status; null while running and for a process killed by a signal. */
  exit_code?: number | null
}

// PreviewSummary mirrors Go's web.PreviewSummary — one live preview.
export interface PreviewSummary {
  bead_id: string
  anvil?: string
  branch?: string
  status: PreviewRecordStatus
  services: PreviewServiceStatus[]
  /**
   * The link an operator opens; '' when there is none — no service has a port
   * yet, or the entry service is not serving.
   */
  entry_url: string
  /**
   * Why the link was withheld (an entry service that failed or has exited).
   * Empty when there is nothing to explain, including a preview that is simply
   * still coming up. Rendered in place of the Open button, so a dead entry
   * service reads as a dead service rather than a link that vanished.
   */
  entry_note?: string
  created_at: string
  last_active_at: string
  /** When the idle reaper tears this down; null when the reaper is disabled. */
  idle_deadline: string | null
  /**
   * The same deadline as a countdown in seconds, straight from the daemon's
   * preview manager. Prefer it over `idle_deadline` for a live countdown: it is
   * relative, so it does not go wrong when the browser clock disagrees with the
   * daemon's. null when the reaper is disabled; 0 means the next reaper tick
   * takes this preview.
   */
  idle_remaining_seconds: number | null
  /** One line naming what the preview holds while up (services and ports). */
  resource_note?: string
}

// PreviewsListResponse is the body of GET /api/previews. `enabled` false means
// the daemon runs without a Kiln manager — "previews are disabled", not "none
// are running". `anvils` names the anvils a preview can be started for, and is
// what gates the Preview affordance per row.
export interface PreviewsListResponse {
  enabled: boolean
  anvils: string[]
  /**
   * Anvils that additionally opted into running their E2E quests against a
   * preview (`preview_quests`). Gates the "Run quests" action the same way
   * `anvils` gates the Preview one.
   */
  quest_anvils: string[]
  previews: PreviewSummary[]
}

// PreviewStatus is the UI-facing state of one bead's preview. It folds the
// backend's record statuses into what the chip renders, and adds the two
// client-only transitions (`starting` before the daemon has written a row,
// `stopping` while a teardown is in flight).
export type PreviewStatus =
  | 'idle'
  | 'starting'
  | 'healthy'
  | 'degraded'
  | 'failed'
  | 'stopping'

// mapPreviewStatus folds a backend record status into the UI vocabulary. A
// stopped record reads as `idle`: the row lingers only until the manager drops
// it, and nothing about it is actionable. An unrecognised value is treated as
// `starting` — the honest reading of "the daemon knows about this preview but
// this client does not know that state yet" is in-progress, never healthy.
export function mapPreviewStatus(status: PreviewRecordStatus | undefined): PreviewStatus {
  switch (status) {
    case 'running':
      return 'healthy'
    case 'degraded':
      return 'degraded'
    case 'failed':
      return 'failed'
    case 'stopped':
    case undefined:
      return 'idle'
    case 'starting':
      return 'starting'
    default:
      return 'starting'
  }
}

// previewServiceIsDown reports whether a service is not serving: it failed its
// readiness check, or became healthy and has since exited. Everything that asks
// "is this row a problem?" goes through it, so the two states cannot drift
// apart on one surface.
export function previewServiceIsDown(health: PreviewServiceHealth | undefined): boolean {
  return health === 'failed' || health === 'exited'
}

// previewErrorText returns the first down service's error for a preview, or
// null when nothing is down. Used to explain a failed or degraded chip without
// making the caller walk the service list.
export function previewErrorText(preview: PreviewSummary | null | undefined): string | null {
  if (!preview) return null
  for (const svc of preview.services ?? []) {
    if (previewServiceIsDown(svc.health) && svc.error) return svc.error
  }
  return null
}

// fetchPreviews loads every live preview plus the enablement/gating metadata.
export async function fetchPreviews(signal?: AbortSignal): Promise<PreviewsListResponse> {
  const res = await fetch('/api/previews', { credentials: 'include', signal })
  if (res.status === 401) {
    throw new ApiError(401, 'unauthorized')
  }
  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as { error?: string }
    throw new ApiError(res.status, body.error ?? `HTTP ${res.status}`)
  }
  const body = (await res.json()) as Partial<PreviewsListResponse>
  return {
    enabled: body.enabled === true,
    anvils: body.anvils ?? [],
    quest_anvils: body.quest_anvils ?? [],
    previews: body.previews ?? [],
  }
}

// postPreviewAction POSTs a preview start/stop and returns the daemon's 202
// body *without* resolving it.
//
// It deliberately does not go through apiPost: that helper polls the queued
// request to a terminal state before returning, on a 15s budget sized for
// sub-second bd shell-outs. A preview start runs a setup script, spawns every
// service and waits out their health checks — minutes, not seconds — so
// blocking on it would leave the button spinning and then report the outcome as
// unknown. The caller (usePreview) keeps the returned poll_url and resolves it
// on its own clock alongside the previews list.
async function postPreviewAction(path: string, body: unknown): Promise<QueuedBody> {
  const res = await fetch(path, {
    method: 'POST',
    credentials: 'include',
    headers: { 'X-Forge-Action': '1', 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (res.status === 401) {
    throw new ApiError(401, 'unauthorized')
  }
  const text = await res.text()
  let parsed: unknown = null
  if (text) {
    try {
      parsed = JSON.parse(text)
    } catch {
      parsed = null
    }
  }
  if (!res.ok) {
    throw new ApiError(res.status, (parsed as { error?: string })?.error ?? `HTTP ${res.status}`)
  }
  return (parsed ?? {}) as QueuedBody
}

// startPreview asks the daemon to bring a bead's preview up. The anvil is
// required: the daemon reads the manifest from that anvil's main checkout.
//
// `branch` is optional and is omitted from the body entirely when blank, so the
// daemon applies its own default (the bead's forge/<bead-id> branch) rather than
// receiving an empty string this client made up. Passing one is what makes an
// ad-hoc preview possible: the bead id is only a registry key, so any branch can
// be previewed under any id.
export function startPreview(
  beadID: string,
  anvil: string,
  branch?: string,
): Promise<QueuedBody> {
  const body: { anvil: string; branch?: string } = { anvil }
  const trimmed = branch?.trim()
  if (trimmed) body.branch = trimmed
  return postPreviewAction(`/api/bead/${encodeURIComponent(beadID)}/preview/start`, body)
}

// stopPreview tears a bead's preview down. The anvil is optional — the bead id
// alone identifies the preview in the manager's registry — but is forwarded
// when known to match the action-request convention.
export function stopPreview(beadID: string, anvil?: string): Promise<QueuedBody> {
  return postPreviewAction(`/api/bead/${encodeURIComponent(beadID)}/preview/stop`, {
    anvil: anvil ?? '',
  })
}

// pollURLFor resolves the request-status endpoint for a queued response: the
// daemon's own poll_url when it sent one, else the canonical path for the
// request id. Returns null when the daemon answered without a request id
// (a synchronous 200), which means there is nothing to poll.
export function pollURLFor(queued: QueuedBody | null | undefined): string | null {
  if (!queued) return null
  if (queued.poll_url) return queued.poll_url
  if (queued.request_id) return `/api/requests/${encodeURIComponent(queued.request_id)}`
  return null
}

// --- preview quest runs ----------------------------------------------------
//
// Running the anvil's E2E quests against a live preview. The wire shapes mirror
// internal/web/preview_quest_handlers.go.
//
// A run is informational: it reports what a browser found on a preview of one
// branch, and nothing on the backend reads the result back. A failed run never
// blocks a merge, a pipeline stage or a PR — which is why the UI styles it as a
// warning rather than an error.

// QuestRunStatus is the lifecycle of one run. `skipped` means a gate said no
// (the anvil never opted in, the preview went unhealthy mid-flight, the anvil
// declares no quests) and `error` means the run itself fell over, so neither is
// a statement about the application.
export type QuestRunStatus = 'running' | 'passed' | 'failed' | 'skipped' | 'error' | (string & {})

// QuestScreenshot mirrors Go's web.QuestScreenshot: an image the run captured,
// addressed by an endpoint on this server rather than by its path on disk.
export interface QuestScreenshot {
  name: string
  url: string
}

// QuestOutcomeSummary mirrors Go's web.QuestOutcomeSummary — one quest's verdict.
export interface QuestOutcomeSummary {
  name: string
  passed: boolean
  /** Index of the step that failed, or -1 when none did. */
  failed_step: number
  error_message?: string
  duration_seconds: number
  file_path?: string
  screenshots: QuestScreenshot[]
}

// QuestRunSummary mirrors Go's web.QuestRunSummary — one whole run.
export interface QuestRunSummary {
  run_id: string
  bead_id: string
  anvil?: string
  preview_id?: string
  base_url?: string
  status: QuestRunStatus
  /** Why a `skipped` run never ran. */
  skip_reason?: string
  /** Why an `error` run never got a verdict. */
  error?: string
  started_at: string
  finished_at: string | null
  duration_seconds: number
  quests: QuestOutcomeSummary[]
}

// QuestRunResponse is the body of the run GETs. `found` is false when the bead
// has never had a run, which renders as "no runs yet" rather than an error.
export interface QuestRunResponse {
  found: boolean
  run?: QuestRunSummary
}

// QuestRunStartResponse is the 202 body of a dispatched run.
export interface QuestRunStartResponse {
  started: boolean
  run_id: string
  message?: string
  run?: QuestRunSummary
}

// questRunPath is the endpoint for a bead's latest run.
function questRunPath(beadID: string): string {
  return `/api/bead/${encodeURIComponent(beadID)}/quests`
}

// fetchQuestRun loads a bead's most recent quest run.
export async function fetchQuestRun(
  beadID: string,
  signal?: AbortSignal,
): Promise<QuestRunResponse> {
  const res = await fetch(questRunPath(beadID), { credentials: 'include', signal })
  if (res.status === 401) {
    throw new ApiError(401, 'unauthorized')
  }
  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as { error?: string }
    throw new ApiError(res.status, body.error ?? `HTTP ${res.status}`)
  }
  const body = (await res.json()) as Partial<QuestRunResponse>
  return { found: body.found === true, run: body.run }
}

// startQuestRun dispatches a run against the bead's live preview and returns as
// soon as the daemon has accepted it — the browser work then proceeds
// asynchronously, and the caller polls fetchQuestRun for the outcome.
//
// A 403 here is the backend's answer to a button that should not have been
// offered: the anvil never opted in, or the preview is not healthy.
export async function startQuestRun(beadID: string): Promise<QuestRunStartResponse> {
  const res = await fetch(questRunPath(beadID), {
    method: 'POST',
    credentials: 'include',
    headers: { 'X-Forge-Action': '1', 'Content-Type': 'application/json' },
    body: '{}',
  })
  if (res.status === 401) {
    throw new ApiError(401, 'unauthorized')
  }
  const text = await res.text()
  let parsed: unknown = null
  if (text) {
    try {
      parsed = JSON.parse(text)
    } catch {
      parsed = null
    }
  }
  if (!res.ok) {
    throw new ApiError(res.status, (parsed as { error?: string })?.error ?? `HTTP ${res.status}`)
  }
  return (parsed ?? {}) as QuestRunStartResponse
}
