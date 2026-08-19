// Lightweight typed wrapper around the Hearth JSON API. We deliberately avoid
// pulling in SWR / react-query for the MVP — a single useApiPoll hook covers
// every screen and keeps the bundle small.

export interface QueueItem {
  bead_id: string
  anvil: string
  title: string
  description?: string
  priority: number
  status: string
  labels: string[]
  section: string
  assignee?: string
  created_at: string
  updated_at: string
  // created_by is the bd identity that filed the bead. Unlike `assignee` it is
  // set on essentially every bead, and it answers a different question: who
  // raised the work, not who is doing it. Empty until the daemon's first poll
  // populates it, the same way the timestamps above degrade.
  created_by?: string
  // auto_dispatch_tag is the anvil's configured dispatch label (forge.yaml
  // `auto_dispatch_tag`). Surfaced per-row so the queue UI can render a
  // one-click apply-tag button on Unlabeled beads without an extra fetch.
  // Empty when the owning anvil has no tag configured.
  auto_dispatch_tag?: string
}

export interface QueueResponse {
  items: QueueItem[]
}

// WorkerKind is the coarse worker-class label served by the daemon. Bellows
// PR-monitor workers expose `kind: 'bellows'` so the UI can render them as
// non-clickable info cards (they have no claude log to display); pipeline
// Smiths and the lifecycle sub-workers (quench/burnish/rebase) are tagged
// as `'smith'`.
export type WorkerKind = 'smith' | 'bellows'

export interface WorkerInfo {
  id: string
  bead_id: string
  anvil: string
  branch?: string
  title?: string
  status: string
  phase?: string
  kind?: WorkerKind
  pid?: number
  started_at: string
  completed_at?: string
  log_path?: string
  pr_number?: number
  /** Full GitHub PR URL, when the worker's bead has an open PR. Server-enriched
   *  from the bead's ingot in /api/status (the IPC WorkerInfo carries only the
   *  number). Used to render the PR number as a clickable link. */
  pr_url?: string
  /** Provider session identifier captured from the smith stream (Claude only). */
  session_id?: string
  /** Model actually used for this worker spawn. */
  model?: string
}

export interface WorkersResponse {
  workers: WorkerInfo[]
}

export interface EventInfo {
  id: number
  timestamp: string
  type: string
  message: string
  bead_id?: string
  anvil?: string
}

export interface EventsResponse {
  events: EventInfo[]
}

// LogLine matches the SSE payload emitted by /api/worker/{id}/stream.
export interface LogLine {
  line: string
  timestamp: string
}

// LogTailResponse matches GET /api/worker/{id}/log?tail=N.
export interface LogTailResponse {
  lines: string[]
}

// WedgedAnvil mirrors `ipc.WedgedAnvilItem`: an anvil whose beads (Dolt)
// working set is mid-merge with unresolved conflicts. While an anvil is listed
// here every bd write against it is rolled back, so nothing dispatched to it
// can succeed until an operator resolves the conflict by hand.
export interface WedgedAnvil {
  anvil: string
  conflict_tables?: string
  conflict_count?: number
  branch?: string
  ahead?: number
  behind?: number
  divergence_known?: boolean
  detail?: string
  detected_at?: string
}

export interface StatusResponse {
  running: boolean
  pid: number
  uptime?: string
  workers?: number
  queue_size?: number
  open_prs?: number
  daily_cost?: number
  daily_cost_limit?: number
  cost_limit_paused?: boolean
  dispatch_paused?: boolean
  // Why dispatch is paused: 'manual' (operator) or 'self-deploy' (the drain a
  // self-deploy takes before rebuilding). Absent/empty on a paused daemon means
  // an older daemon — treat as manual. Unknown values are rendered verbatim.
  dispatch_pause_reason?: string
  // Reason-specific context, e.g. 'waiting on 2 workers, max 30m'.
  dispatch_pause_detail?: string
  paused_since?: string
  copilot_premium_requests?: number
  copilot_request_limit?: number
  copilot_limit_reached?: boolean
  max_total_smiths?: number
  wedged_anvils?: WedgedAnvil[]
}

export interface CrucibleStatus {
  parent_id: string
  parent_title: string
  anvil: string
  branch: string
  phase: string
  total_children: number
  completed_children: number
  current_child: string
  started_at: string
}

export interface CruciblesResponse {
  crucibles: CrucibleStatus[]
}

export interface TestResult {
  id: number
  step_index: number
  step_name: string
  command: string
  exit_code: number
  duration_ms: number
  passed: boolean
  optional: boolean
  skipped?: boolean
  output_summary?: string
  recorded_at: string
}

export interface Ingot {
  id: number
  bead_id: string
  anvil: string
  pr_id?: number
  worker_id: string
  status: string
  temper_passed: boolean
  temper_failed_step?: string
  temper_duration_ms: number
  pr_number?: number
  pr_url?: string
  title: string
  branch: string
  test_results?: TestResult[]
  created_at: string
  updated_at: string
}

export interface HistoryWorker {
  id: string
  bead_id: string
  anvil: string
  branch?: string
  title?: string
  status: string
  phase?: string
  kind?: WorkerKind
  pid?: number
  started_at: string
  completed_at?: string
  duration_sec?: number
  log_path?: string
  pr_number?: number
  session_id?: string
  model?: string
}

export interface HistoryWorkersResponse {
  workers: HistoryWorker[]
}

export interface CostRow {
  date: string
  input_tokens: number
  output_tokens: number
  estimated_cost: number
}

export interface ProviderCostRow {
  provider: string
  input_tokens: number
  output_tokens: number
  cache_read: number
  cache_write: number
  estimated_cost: number
}

export interface CostsResponse {
  today: CostRow
  today_limit: number
  recent: CostRow[]
  today_providers: ProviderCostRow[]
}

export interface BeadDetailQueue {
  bead_id: string
  anvil: string
  title: string
  description?: string
  priority: number
  status: string
  section: string
  labels: string[]
  assignee?: string
  // created_by is the bd identity that filed the bead, mirroring the queue
  // pane's byline so the two surfaces do not disagree about who raised it.
  created_by?: string
}

export interface BeadDetailRetry {
  retry_count: number
  next_retry?: string
  needs_human: boolean
  clarification_needed: boolean
  dispatch_failures: number
  recovery_failures: number
  last_error?: string
  updated_at?: string
}

export interface BeadDetailCost {
  input_tokens: number
  output_tokens: number
  cache_read: number
  cache_write: number
  estimated_cost_usd: number
  updated_at?: string
}

export interface BeadDetailEvent {
  id: number
  timestamp: string
  type: string
  message: string
}

export interface BeadDetailPR {
  id: number
  number: number
  anvil: string
  branch?: string
  base_branch?: string
  status: string
  title?: string
  created_at?: string
  last_checked?: string
}

export interface BeadDetailWorker {
  id: string
  status: string
  phase?: string
  branch?: string
  started_at: string
  completed_at?: string
  duration_sec?: number
  log_path?: string
  pr_number?: number
  session_id?: string
  model?: string
}

// BeadDetailComment is one entry from the `comments` array on the bead detail
// response. The backend renames bd's `text` field to `body` so the SPA can
// render it uniformly with other markdown-ish text blocks.
export interface BeadDetailComment {
  id?: string
  author: string
  body: string
  created_at: string
}

// BeadBrief is the lightweight reference shape used by the dep graph
// (matches Go's beadDetailDepRef). The nested blocks / blocked_by fields
// are populated only when the deps endpoint is asked to recurse past
// depth 1; the immediate lists on BeadDetailResponse leave them undefined.
export interface BeadBrief {
  bead_id: string
  anvil?: string
  title: string
  status: string
  priority: number
  blocks?: BeadBrief[]
  blocked_by?: BeadBrief[]
}

export interface BeadDetailResponse {
  bead_id: string
  anvil?: string
  queue?: BeadDetailQueue
  ingot?: Ingot
  retry?: BeadDetailRetry
  cost?: BeadDetailCost
  workers: BeadDetailWorker[]
  events: BeadDetailEvent[]
  prs: BeadDetailPR[]
  blocks: BeadBrief[]
  blocked_by: BeadBrief[]
  notes?: string
  design?: string
  acceptance_criteria?: string
  comments: BeadDetailComment[]
}

export interface BeadDepsResponse {
  bead_id: string
  depth: number
  blocks: BeadBrief[]
  blocked_by: BeadBrief[]
}

export function fetchBeadDeps(
  beadID: string,
  depth = 1,
  signal?: AbortSignal,
): Promise<BeadDepsResponse> {
  const qs = new URLSearchParams({ depth: String(depth) }).toString()
  return apiGet<BeadDepsResponse>(
    `/api/bead/${encodeURIComponent(beadID)}/deps?${qs}`,
    signal,
  )
}

// BeadLogFile is one stage log file from GET /api/bead/{id}/logs. `stage` is
// parsed from the filename prefix (smith/warden/temper/…, or "other"). `live`
// is true for the file an active worker is currently writing; `worker_id` is
// set only in that case so the client can stream /api/worker/{id}/stream.
export interface BeadLogFile {
  filename: string
  stage: string
  size_bytes: number
  mtime: string
  live: boolean
  worker_id?: string
}

export interface BeadLogsResponse {
  bead_id: string
  files: BeadLogFile[]
}

// beadLogs wraps the per-bead transcript endpoints: list the preserved + live
// stage log files for a bead, and tail one file. Live files should be streamed
// via /api/worker/{worker_id}/stream instead of tailed.
export const beadLogs = {
  list: (beadID: string, signal?: AbortSignal) =>
    apiGet<BeadLogsResponse>(`/api/bead/${encodeURIComponent(beadID)}/logs`, signal),
  tail: (beadID: string, filename: string, tail = 500, signal?: AbortSignal) =>
    apiGet<LogTailResponse>(
      `/api/bead/${encodeURIComponent(beadID)}/logs/${encodeURIComponent(filename)}?tail=${tail}`,
      signal,
    ),
}

export interface PRItem {
  id?: number
  number: number
  anvil: string
  repo?: string
  branch?: string
  base_branch?: string
  title?: string
  url?: string
  author?: string
  status: string
  is_external: boolean
  is_conflicting?: boolean
  ci_passing?: boolean
  ci_failing?: boolean
  reviews_approved?: boolean
  bellows_assigned?: boolean
  ci_fix_count?: number
  review_fix_count?: number
  rebase_count?: number
  bead_id?: string
  created_at?: string
  updated_at?: string
  merged_at?: string
  closed_at?: string
}

// PRsResponse is the shape served by GET /api/prs/all. Keys align with
// PRSectionKind so callers can index directly without a mapping layer.
export interface PRsResponse {
  forge_prs: PRItem[]
  external_prs: PRItem[]
  recently_merged: PRItem[]
}

// FindingSeverity mirrors the `severity` column the daemon writes to
// pr_findings. The three canonical Assay buckets are surfaced as a union for
// ergonomic switch/badge logic, but the type stays open (`| string`) so an
// unexpected backend value renders rather than failing to type-check.
export type FindingSeverity = 'Important' | 'PreExisting' | 'Nit' | (string & {})

// FindingStatus mirrors web's findingStatus(): a finding is `open` once
// detected, `posted` after the comment lands on the PR, and `resolved` once
// the review thread is closed.
export type FindingStatus = 'open' | 'posted' | 'resolved' | (string & {})

// AssayFinding mirrors internal/web's findingJSON. The first seven fields are
// the stable contract (id, pr, anvil, status, severity, message, timestamp);
// the rest are optional context the findings panel renders when present.
export interface AssayFinding {
  id: number
  // pr is the GitHub PR number the finding was raised against (not the
  // state.db row id used in the endpoint path).
  pr: number
  anvil: string
  status: FindingStatus
  severity: FindingSeverity
  message: string
  timestamp?: string
  head_sha?: string
  file?: string
  anchor?: string
  category?: string
  body?: string
}

// AssayRunStatus mirrors web's runStatus(): a run is `running` until it
// finishes, then resolves to `error`, `skipped`, `partial`, or `complete`.
// `partial` means some review passes covered the head and others never did —
// the findings are real but they are not a review of the whole diff.
export type AssayRunStatus =
  | 'running'
  | 'error'
  | 'skipped'
  | 'partial'
  | 'complete'
  | (string & {})

// AssayPassFailure names one Assay pass that did not review the head, and why
// ("error_max_turns", "rate_limited", …).
export interface AssayPassFailure {
  name: string
  reason?: string
}

// AssayRun mirrors internal/web's assayRunJSON — a summary of the most recent
// Assay review pass over a PR so the panel can show rerun progress.
export interface AssayRun {
  status: AssayRunStatus
  head_sha?: string
  started_at?: string
  finished_at?: string
  duration_ms?: number
  cost_usd?: number
  findings_count: number
  posted_count: number
  shadow_mode?: boolean
  skipped_reason?: string
  error?: string
  // Coverage of a `partial` run: how many passes completed out of how many,
  // which ones did not, and the server-rendered one-line status text.
  completed_passes?: number
  total_passes?: number
  failed_passes?: AssayPassFailure[]
  status_text?: string
}

// PRFindingsResponse mirrors internal/web's prFindingsResponse. It is the body
// of GET /api/prs/{id}/findings and the payload of each `findings` SSE event
// on /api/prs/{id}/findings/stream. `run` is null until Assay has recorded at
// least one pass; `findings` is always an array.
export interface PRFindingsResponse {
  pr: number
  anvil: string
  run: AssayRun | null
  findings: AssayFinding[]
}

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

export async function apiGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(path, { credentials: 'include', signal })
  if (res.status === 401) {
    throw new ApiError(401, 'unauthorized')
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    const msg = (body as { error?: string }).error ?? `HTTP ${res.status}`
    throw new ApiError(res.status, msg)
  }
  return (await res.json()) as T
}

// RequestState mirrors internal/ipc's request outcome states. `pending` means
// the daemon is still running the queued command; `ok`/`error` are terminal;
// `unknown` means the daemon no longer holds a record for the id (it aged out
// of the bounded store) — which is explicitly not a success.
export type RequestState = 'pending' | 'ok' | 'error' | 'unknown'

// RequestStatus is the body of GET /api/requests/{request_id}.
export interface RequestStatus {
  request_id: string
  state: RequestState
  message?: string
  updated_at?: string
}

// QueuedBody is the 202 Accepted body every async action endpoint returns.
// `poll_url` points at the request-status endpoint for `request_id`.
export interface QueuedBody {
  queued?: boolean
  request_id?: string
  poll_url?: string
  message?: string
}

// queued_unresolved marks a response whose queued command never reached a
// terminal state before we stopped polling (or whose record had already been
// evicted). Callers must not present it as success — see useAction.
export interface UnresolvedQueued {
  queued_unresolved: true
  queued_state: RequestState
}

// Polling budget for resolving a queued command. bd shell-outs are typically
// sub-second; 15s of polling covers a loaded daemon without leaving a button
// spinning indefinitely.
const QUEUED_POLL_INTERVAL_MS = 400
const QUEUED_POLL_BUDGET_MS = 15_000

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// fetchRequestStatus resolves a single request_id to its current state. A
// failed or unparseable lookup reads as `unknown` rather than throwing: we
// must never turn a bookkeeping hiccup into a phantom action failure.
export async function fetchRequestStatus(pollUrl: string): Promise<RequestStatus> {
  try {
    const res = await fetch(pollUrl, { credentials: 'include' })
    if (!res.ok) {
      return { request_id: '', state: 'unknown' }
    }
    const body = (await res.json()) as RequestStatus
    if (!body || typeof body.state !== 'string') {
      return { request_id: '', state: 'unknown' }
    }
    return body
  } catch {
    return { request_id: '', state: 'unknown' }
  }
}

// resolveQueuedRequest polls the request-status endpoint until the queued
// command reaches a terminal state or the budget expires. A timeout returns
// `pending` so the caller can report "queued, outcome unknown" — never
// success.
export async function resolveQueuedRequest(
  pollUrl: string,
  budgetMs = QUEUED_POLL_BUDGET_MS,
): Promise<RequestStatus> {
  const deadline = Date.now() + budgetMs
  let last: RequestStatus = { request_id: '', state: 'pending' }
  for (;;) {
    last = await fetchRequestStatus(pollUrl)
    if (last.state !== 'pending') return last
    if (Date.now() >= deadline) return last
    await sleep(QUEUED_POLL_INTERVAL_MS)
  }
}

// apiPost dispatches a JSON-bodied POST to an action endpoint.
//
// A 200 ("ok") is a synchronous success. A 202 ("queued") means the daemon
// only accepted the command — the bd/gh shell-out that does the actual work
// runs in the background and can still fail (a wedged anvil fails every write).
// So a 202 carrying a request_id is not treated as success: we poll the
// request-status endpoint and either return normally (state ok), throw an
// ApiError carrying the daemon's failure message (state error), or return the
// body tagged `queued_unresolved` when the outcome could not be determined.
// A 4xx/5xx response is surfaced as ApiError with the daemon's message.
export async function apiPost<T = unknown>(path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { 'X-Forge-Action': '1' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const res = await fetch(path, {
    method: 'POST',
    credentials: 'include',
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
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
    const msg =
      (parsed as { error?: string })?.error ?? `HTTP ${res.status}`
    throw new ApiError(res.status, msg)
  }
  if (res.status === 202) {
    const queued = (parsed ?? {}) as QueuedBody
    const requestID = queued.request_id
    if (requestID) {
      const pollUrl = queued.poll_url ?? `/api/requests/${encodeURIComponent(requestID)}`
      const outcome = await resolveQueuedRequest(pollUrl)
      if (outcome.state === 'error') {
        throw new ApiError(500, outcome.message || 'the queued command failed')
      }
      if (outcome.state !== 'ok') {
        return {
          ...(queued as object),
          queued_unresolved: true,
          queued_state: outcome.state,
        } as T
      }
    }
  }
  return (parsed ?? {}) as T
}

// isUnresolvedQueued reports whether an action result came back from apiPost
// without a confirmed outcome. Such a result must be reported neutrally
// ("queued, outcome unknown"), not as success.
export function isUnresolvedQueued(result: unknown): result is UnresolvedQueued {
  return (
    typeof result === 'object' &&
    result !== null &&
    (result as UnresolvedQueued).queued_unresolved === true
  )
}

export interface ActionRequest {
  anvil: string
  reason?: string
  note?: string
  label?: string
  force_run?: boolean
}

export const actions = {
  // pauseDispatch / resumeDispatch toggle the daemon-wide auto-dispatch
  // switch. Pausing stops new workers from being dispatched while leaving
  // running workers untouched; resuming re-enables dispatch immediately.
  pauseDispatch: () => apiPost<{ message?: string }>(`/api/dispatch/pause`),
  resumeDispatch: () => apiPost<{ message?: string }>(`/api/dispatch/resume`),
  killWorker: (workerID: string) => apiPost(`/api/worker/${encodeURIComponent(workerID)}/kill`),
  retry: (beadID: string, anvil: string) =>
    apiPost(`/api/queue/${encodeURIComponent(beadID)}/retry`, { anvil }),
  dispatch: (beadID: string, anvil: string, forceRun = false) =>
    apiPost(`/api/queue/${encodeURIComponent(beadID)}/dispatch`, {
      anvil,
      force_run: forceRun,
    }),
  clarify: (beadID: string, anvil: string, reason: string) =>
    apiPost(`/api/queue/${encodeURIComponent(beadID)}/clarify`, { anvil, reason }),
  unclarify: (beadID: string, anvil: string) =>
    apiPost(`/api/queue/${encodeURIComponent(beadID)}/unclarify`, { anvil }),
  stop: (beadID: string, anvil: string, reason?: string) =>
    apiPost(`/api/queue/${encodeURIComponent(beadID)}/stop`, { anvil, reason }),
  closeBead: (beadID: string, anvil: string) =>
    apiPost(`/api/bead/${encodeURIComponent(beadID)}/close`, { anvil }),
  addLabel: (beadID: string, anvil: string, label: string) =>
    apiPost(`/api/bead/${encodeURIComponent(beadID)}/label/add`, { anvil, label }),
  // applyDispatchTag asks the backend to add the anvil's configured
  // auto_dispatch_tag to the bead. The tag itself is resolved server-side
  // from forge.yaml; the response includes the resolved tag so the SPA can
  // render it in the success toast.
  applyDispatchTag: (beadID: string, anvil: string) =>
    apiPost<{ tag?: string }>(
      `/api/queue/${encodeURIComponent(beadID)}/apply-dispatch-tag`,
      { anvil },
    ),
  removeLabel: (beadID: string, anvil: string, label: string) =>
    apiPost(`/api/bead/${encodeURIComponent(beadID)}/label/remove`, { anvil, label }),
  addNote: (beadID: string, anvil: string, note: string) =>
    apiPost(`/api/bead/${encodeURIComponent(beadID)}/note`, { anvil, note }),
  // addComment POSTs to the bd-backed comment endpoint. The backend returns
  // the created comment in `{comment}` on 201; a 204 means the write
  // succeeded but bd did not echo a parseable comment back (older bd
  // versions) — callers should fall back to the next poll in that case.
  addComment: (beadID: string, anvil: string, body: string) =>
    apiPost<{ comment?: BeadDetailComment }>(
      `/api/bead/${encodeURIComponent(beadID)}/comment`,
      { anvil, body },
    ),
  // steer delivers a human steering message to a bead's in-flight pipeline.
  // Unlike the other bead actions it is keyed purely by bead id — the daemon
  // resolves the active pipeline from its in-memory control registry, so no
  // anvil is sent. The daemon returns an actionable error (surfaced by apiPost)
  // when the bead has no active pipeline, the message is empty, or the session
  // is not a Claude session.
  steer: (beadID: string, message: string) =>
    apiPost<{ message?: string }>(
      `/api/bead/${encodeURIComponent(beadID)}/steer`,
      { message },
    ),
  // pause parks a bead's in-flight Smith spawn. Like steer it is keyed purely
  // by bead id — the daemon resolves the active pipeline and validates the
  // worker is running before parking it. The daemon returns an actionable
  // error (surfaced by apiPost) when the bead has no active pipeline or its
  // worker is not running.
  pause: (beadID: string) =>
    apiPost<{ status?: string; message?: string }>(
      `/api/bead/${encodeURIComponent(beadID)}/pause`,
    ),
  // resume continues a paused bead's pipeline. The optional message becomes the
  // prompt the resumed Claude spawn continues with; the daemon defaults it when
  // omitted. It transparently handles both a warm resume (live parked goroutine)
  // and a cold resume (paused worker surviving a daemon restart). The daemon
  // returns an actionable error when the bead is not paused.
  resume: (beadID: string, message?: string) =>
    apiPost<{ status?: string; message?: string }>(
      `/api/bead/${encodeURIComponent(beadID)}/resume`,
      message ? { message } : undefined,
    ),
  // resumeWithMessage resumes a needs-attention bead whose worktree was torn
  // down but whose forge/<bead> branch survives, seeding the resumed (or
  // fresh-fallback) Claude session with an operator message. Unlike `resume`
  // (which continues a paused, still-parked pipeline), this recreates the
  // worktree from the surviving branch before resuming. Like steer it is keyed
  // purely by bead id — no anvil is sent. The daemon returns an actionable error
  // (surfaced by apiPost) when the bead has a live pipeline, has no resumable
  // worker row, or its resume preconditions are unmet.
  resumeWithMessage: (beadID: string, message?: string) =>
    apiPost<{ worker_id?: string; message?: string }>(
      `/api/bead/${encodeURIComponent(beadID)}/resume-with-message`,
      message ? { message } : undefined,
    ),
}

// Steerable is the minimal worker shape steerDisabledReason inspects — a subset
// of WorkerInfo / BeadDetailWorker so either can be passed directly.
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
export function isFinishedWorker(w: Pick<WorkerInfo, 'status' | 'completed_at'>): boolean {
  return FINISHED_WORKER_STATUSES.has(w.status) && !!w.completed_at
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

// ForgeSession is one design conversation persisted on the server.
// stage tracks the AI loop's current phase (drafting | grilling | ready);
// plan holds the latest implementation plan emitted by claude.
export interface ForgeSession {
  id: number
  title: string
  status: string
  anvil?: string
  created_by?: string
  created_at: string
  updated_at: string
  message_count: number
  stage: 'drafting' | 'grilling' | 'ready'
  plan?: string
}

// ForgeMessage is one entry in a forge session conversation.
// kind extends role with a structured payload type used by the AI loop:
//   - text:           plain conversational turn (user or assistant).
//   - plan:           markdown plan emitted by claude in drafting stage.
//   - question:       structured grilling-stage question; metadata holds options.
//   - answer:         user's answer to a question; metadata pins question_id+option_id.
//   - status:         system-emitted status note (stage transitions, "grilling done").
//   - beads_created:  receipt for a successful bead-emission turn; metadata holds
//                     a JSON {summary, beads:[{bead_id, anvil, title}]} payload.
export interface ForgeMessage {
  id: number
  session_id: number
  role: 'user' | 'assistant' | 'system'
  content: string
  created_at: string
  kind?: 'text' | 'plan' | 'question' | 'answer' | 'status' | 'beads_created'
  metadata?: string
}

// ForgeCreatedBead mirrors one entry in the JSON metadata of a
// kind=beads_created message and the response of POST /create-beads.
export interface ForgeCreatedBead {
  bead_id: string
  anvil: string
  title: string
}

// ForgeBeadsCreatedPayload is the JSON-decoded shape stored in
// ForgeMessage.metadata when kind === "beads_created".
export interface ForgeBeadsCreatedPayload {
  summary?: string
  beads: ForgeCreatedBead[]
}

// ForgeCreateBeadsResponse is the JSON returned by
// POST /api/forge/sessions/{id}/create-beads.
export interface ForgeCreateBeadsResponse {
  session: ForgeSession
  messages: ForgeMessage[]
  beads: ForgeCreatedBead[]
  summary?: string
}

// ForgeQuestionPayload is the JSON-decoded shape stored in
// ForgeMessage.metadata when kind === "question".
export interface ForgeQuestionPayload {
  options: Array<{ id: string; label: string; description?: string }>
  recommendation?: string
  rationale?: string
}

export interface ForgeSessionsListResponse {
  sessions: ForgeSession[]
}

export interface ForgeSessionDetailResponse {
  session: ForgeSession
  messages: ForgeMessage[]
}

// ForgeTurnRequest is the body shape for POST /api/forge/sessions/{id}/turn.
// All fields are optional and combinable, mirroring the server.
export interface ForgeTurnRequest {
  content?: string
  answer_option_id?: string
  answer_question_id?: number
  request_plan?: boolean
  start_grilling?: boolean
  mark_ready?: boolean
}

// ForgeTurnResponse is the unified response for /turn. messages includes
// any newly-appended user message followed by the assistant emissions.
export interface ForgeTurnResponse {
  session: ForgeSession
  messages: ForgeMessage[]
}

// ForgeAnvil is one entry in the GET /api/forge/anvils response. The
// backend only exposes the name today; the path is held server-side so
// the browser cannot leak filesystem layout.
export interface ForgeAnvil {
  name: string
}

export interface ForgeAnvilsListResponse {
  anvils: ForgeAnvil[]
}

// forgeAnvils wraps the read-only anvil-listing endpoint used by the
// Beads-Forge new-session form to populate its anvil-select dropdown.
export const forgeAnvils = {
  list: (signal?: AbortSignal) =>
    apiGet<ForgeAnvilsListResponse>('/api/forge/anvils', signal),
}

// forgeSessions wraps the /api/forge/sessions endpoints. All mutating calls
// route through apiPost / apiSend which set the X-Forge-Action header that
// the daemon's CSRF middleware requires.
export const forgeSessions = {
  list: (signal?: AbortSignal) =>
    apiGet<ForgeSessionsListResponse>('/api/forge/sessions', signal),
  get: (id: number, signal?: AbortSignal) =>
    apiGet<ForgeSessionDetailResponse>(
      `/api/forge/sessions/${id}`,
      signal,
    ),
  create: (input: { title?: string; anvil?: string; initial_message?: string }) =>
    apiPost<{ session: ForgeSession; message?: ForgeMessage }>(
      '/api/forge/sessions',
      input,
    ),
  appendMessage: (id: number, content: string) =>
    apiPost<{ session: ForgeSession; message: ForgeMessage }>(
      `/api/forge/sessions/${id}/messages`,
      { content },
    ),
  rename: (id: number, title: string) =>
    apiSend<{ session: ForgeSession }>(
      'PATCH',
      `/api/forge/sessions/${id}`,
      { title },
    ),
  setStatus: (id: number, status: string) =>
    apiSend<{ session: ForgeSession }>(
      'PATCH',
      `/api/forge/sessions/${id}`,
      { status },
    ),
  delete: (id: number) =>
    apiSend<{ status: string }>('DELETE', `/api/forge/sessions/${id}`),
  turn: (id: number, req: ForgeTurnRequest) =>
    apiPost<ForgeTurnResponse>(`/api/forge/sessions/${id}/turn`, req),
  createBeads: (id: number) =>
    apiPost<ForgeCreateBeadsResponse>(`/api/forge/sessions/${id}/create-beads`),
}

// apiSend is a generic helper for non-POST mutating verbs (PATCH, DELETE,
// PUT). The CSRF middleware accepts any non-safe method as long as the
// X-Forge-Action header is present. apiPost remains the friendlier name for
// the common case.
export async function apiSend<T = unknown>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = { 'X-Forge-Action': '1' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const res = await fetch(path, {
    method,
    credentials: 'include',
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
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
    const msg =
      (parsed as { error?: string })?.error ?? `HTTP ${res.status}`
    throw new ApiError(res.status, msg)
  }
  return (parsed ?? {}) as T
}

// ConfigValueType is the value type of a managed setting; it selects which
// editor control the SettingsPage renders for each key. The type strings are a
// contract shared verbatim with the backend (internal/web/forge_config.go).
export type ConfigValueType =
  | 'bool'
  | 'int'
  | 'float'
  | 'enum'
  | 'string'
  | 'string_list'
  | 'provider_map'
  | 'duration'

// ConfigValue is the typed value of a managed setting as it appears in JSON.
// Scalars cover bool/int/float/enum/string/duration; a string array is a
// string_list value; a stage→provider-chain map is a provider_map value.
export type ConfigValue =
  | boolean
  | number
  | string
  | string[]
  | Record<string, string[]>

// ConfigKeyInfo mirrors Go's web.ConfigKeyInfo — one managed setting with the
// metadata the SettingsPage needs to render and group it. `value` is typed per
// `type`; `options` is the allowed set for enums; `min`/`max` bound numbers;
// `unit` is an optional display suffix. `hotReloadable` is true only for keys
// the running daemon applies without a restart, which drives the "applies on
// next run" hint for the rest.
export interface ConfigKeyInfo {
  key: string
  type: ConfigValueType
  value: ConfigValue
  isDefault: boolean
  hotReloadable: boolean
  area: string
  label: string
  description: string
  options?: string[]
  min?: number
  max?: number
  unit?: string
}

// AnvilKeyInfo mirrors Go's web.AnvilKeyInfo — the per-anvil key schema served
// in `anvilKeys` so the UI renders per-anvil controls from metadata rather than
// a hardcoded list. `triState` marks *bool keys where `null` clears the override
// (anvil inherits the global/default).
export interface AnvilKeyInfo {
  key: string
  type: ConfigValueType
  triState: boolean
  hotReloadable: boolean
  label: string
  description: string
  options?: string[]
  min?: number
  max?: number
}

// AnvilSettings mirrors Go's config.AnvilSettings — the per-anvil projection
// served under `anvils.<name>` by GET /api/forge/config. The *bool keys are
// tri-state: `null` means "inherit / unset" (the anvil follows the global
// setting or built-in default), while `true`/`false` is an explicit override.
// This nullable distinction must survive the round-trip, so callers must NOT
// collapse `null` to `false`. `auto_merge`, `preview_quests` and
// `wicket_auto_dispatch` are plain booleans with no inherit semantics. The
// remaining fields are non-boolean per-anvil scalars (Forge-85wn).
export interface AnvilSettings {
  auto_merge: boolean
  schematic_enabled: boolean | null
  golangci_lint: boolean | null
  go_race_detection: boolean | null
  depcheck_enabled: boolean | null
  questgiver_enabled: boolean | null
  // Kiln preview keys. `preview_enabled` is tri-state (null = inherit the
  // global `settings.preview_enabled`); `preview_auto` is '' (on-demand only)
  // or 'ready_to_merge'; `preview_quests` is a plain opt-in bool.
  preview_enabled: boolean | null
  preview_auto: string
  preview_quests: boolean
  wicket_enabled: boolean | null
  wicket_auto_dispatch: boolean
  max_smiths: number
  auto_dispatch: string
  auto_dispatch_tag: string
  auto_dispatch_min_priority: number
  platform: string
  // Composite per-anvil overrides (Forge-vo5a). Nil slices/maps serialize to
  // JSON `null` and the empty triage prompt to `""`; all mean "inherit" — the
  // anvil has no explicit override and the global/default applies. The null
  // distinction must survive the round-trip (callers must NOT collapse it), so
  // the list/map fields are nullable rather than defaulted to empty.
  stage_providers: Record<string, string[]> | null
  wicket_trusted_users: string[] | null
  wicket_ignore_users: string[] | null
  wicket_repos: string[] | null
  wicket_issue_labels: string[] | null
  wicket_triage_prompt: string
}

// ConfigResponse is the body of GET /api/forge/config (and the echo returned by
// PATCH). `keys` is server-ordered so the UI renders deterministically.
// `anvilKeys` is the per-anvil key schema. `anvils` maps each configured anvil
// name to its per-anvil overrides; it is always present (an empty object when no
// anvils are configured).
export interface ConfigResponse {
  keys: ConfigKeyInfo[]
  anvilKeys: AnvilKeyInfo[]
  anvils: Record<string, AnvilSettings>
}

// AnvilKeyApplied mirrors Go's web.AnvilKeyApplied — per edited key, whether the
// change takes effect instantly (hot-reloaded by the daemon) or on the next
// dispatch/run, and whether the edit cleared the override (tri-state reset to
// inherit) rather than setting an explicit value.
export interface AnvilKeyApplied {
  key: string
  applies: 'instant' | 'next_run'
  cleared: boolean
}

// AnvilConfigPatchResponse is the body of PATCH /api/forge/config/anvils/{name}.
// `settings` is the re-read projection of the anvil after the edit (same shape
// as the per-anvil entries in GET), so callers see the persisted result without
// a second request. `applied` lists per-key hot-reload coverage for exactly the
// keys touched by the request.
export interface AnvilConfigPatchResponse {
  anvil: string
  settings: AnvilSettings
  applied: AnvilKeyApplied[]
}

// forgeConfig wraps the managed-settings endpoints. `patch` sends a map of
// key->boolean to PATCH /api/forge/config (validated all-or-nothing by the
// backend) and returns the freshly re-read config, same shape as `get`.
// `patchAnvil` writes per-anvil overrides: tri-state keys accept `null` to
// clear the override (inherit the global/default), so the change map is
// key->(boolean|null).
export const forgeConfig = {
  get: (signal?: AbortSignal) => apiGet<ConfigResponse>('/api/forge/config', signal),
  patch: (changes: Record<string, ConfigValue>) =>
    apiSend<ConfigResponse>('PATCH', '/api/forge/config', changes),
  patchAnvil: (anvil: string, changes: Record<string, ConfigValue | null>) =>
    apiSend<AnvilConfigPatchResponse>(
      'PATCH',
      `/api/forge/config/anvils/${encodeURIComponent(anvil)}`,
      changes,
    ),
}

// PRActionKind enumerates the per-row actions exposed on the /prs tab. The
// backend resolves the PR row from state.db using the numeric id, so the
// frontend only sends the path — no body is required.
export type PRActionKind =
  | 'merge'
  | 'close'
  | 'approve'
  | 'bellows'
  | 'fix-ci'
  | 'fix-comments'
  | 'fix-conflicts'
  | 'reset-counters'

// prActions wraps the /api/prs/{id}/<action> endpoints. The daemon dispatches
// these via in-process IPC. For external PRs (ext-* bead IDs), the backend
// re-uses the same handlers — the daemon's pr_action falls through to gh CLI
// for merge/close/approve/bellows. The branch-required actions (fix-ci,
// fix-comments, fix-conflicts) are 400'd by the backend when no branch is on
// record, and the UI hides them for external PRs anyway.
export const prActions = {
  run: (prID: number, action: PRActionKind) =>
    apiPost(`/api/prs/${prID}/${action}`),
}

// --- Assay findings client -------------------------------------------------
//
// The findings endpoints are all keyed by the PR's numeric state.db id (the
// `{id}` path parameter), the same id prActions.run uses. `getFindings` and
// the SSE channel consume the web-backend sub-task's endpoints directly;
// `rerunAssay` POSTs to the rerun endpoint, which the daemon turns into an
// `assay_rerun` action (anvil is forwarded in the body, mirroring the
// `{anvil}` convention the queue/PR actions already use).

// RerunAssayParams is the argument shape for assay.rerunAssay. `pr` is the
// state.db PR id (the path parameter); `anvil` is forwarded to the daemon so
// it can resolve the worktree/config for the re-review.
export interface RerunAssayParams {
  anvil: string
  pr: number
}

// findingsStreamURL builds the SSE endpoint for a PR's live findings channel.
// Exposed so callers (and tests) construct the URL through one place.
export function findingsStreamURL(pr: number): string {
  return `/api/prs/${pr}/findings/stream`
}

// FindingsStreamHandlers is the callback set passed to subscribeFindings.
export interface FindingsStreamHandlers {
  // onSnapshot fires for every `findings` event with the full, current
  // findings/run snapshot — the backend re-emits the whole payload on each
  // change, so consumers replace (not merge) their state.
  onSnapshot: (snapshot: PRFindingsResponse) => void
  // onOpen fires once the EventSource connection is established.
  onOpen?: () => void
  // onError reports a transport problem. The browser auto-reconnects (the
  // backend emits `retry: 3000`), so callers typically show a quiet banner
  // rather than tearing the panel down.
  onError?: (message: string) => void
}

// FindingsStreamOptions lets tests inject a fake EventSource constructor.
export interface FindingsStreamOptions {
  eventSourceImpl?: typeof EventSource
}

// FindingsSubscription is returned from subscribeFindings. close() releases
// the EventSource and prevents any further handler invocations. Safe to call
// more than once.
export interface FindingsSubscription {
  close: () => void
}

// subscribeFindings opens the named-event findings SSE stream for a PR and
// forwards each `findings` event to onSnapshot. The backend emits a named
// event (`event: findings`) rather than the default message event, so this
// registers an explicit listener instead of using the generic useEventSource
// hook. Returns a no-op subscription when EventSource is unavailable (jsdom /
// non-streaming environments); callers should fall back to getFindings polling
// or the initial fetch in that case.
export function subscribeFindings(
  pr: number,
  handlers: FindingsStreamHandlers,
  opts: FindingsStreamOptions = {},
): FindingsSubscription {
  const EventSourceCtor =
    opts.eventSourceImpl ?? (typeof EventSource !== 'undefined' ? EventSource : undefined)
  if (!EventSourceCtor) {
    return { close: () => {} }
  }

  const es = new EventSourceCtor(findingsStreamURL(pr), { withCredentials: true })
  let closed = false
  const close = () => {
    if (closed) return
    closed = true
    es.close()
  }

  es.addEventListener('open', () => {
    if (!closed) handlers.onOpen?.()
  })
  es.addEventListener('findings', (ev: MessageEvent) => {
    if (closed) return
    try {
      handlers.onSnapshot(JSON.parse(ev.data) as PRFindingsResponse)
    } catch {
      // Drop malformed frames silently — a single bad payload must not kill
      // the stream.
    }
  })
  es.onerror = () => {
    if (!closed) handlers.onError?.('connection lost')
  }

  return { close }
}

// assay groups the typed findings client: a one-shot snapshot fetch, the
// re-run trigger, and the SSE subscription. Mutating calls route through
// apiPost (which sets the X-Forge-Action CSRF header).
export const assay = {
  // getFindings loads the current findings + latest run for a PR by its
  // state.db id.
  getFindings: (pr: number, signal?: AbortSignal) =>
    apiGet<PRFindingsResponse>(`/api/prs/${pr}/findings`, signal),
  // rerunAssay asks the daemon to re-run Assay over the PR's current head.
  rerunAssay: ({ anvil, pr }: RerunAssayParams) =>
    apiPost<{ message?: string }>(`/api/prs/${pr}/rerun-assay`, { anvil }),
  findingsStreamURL,
  subscribeFindings,
}
