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
  copilot_premium_requests?: number
  copilot_request_limit?: number
  copilot_limit_reached?: boolean
  max_total_smiths?: number
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

// apiPost dispatches a JSON-bodied POST to an action endpoint. Both 200
// (synchronous "ok") and 202 (async "queued") are treated as success since
// the daemon runs queued shellouts in the background. A 4xx/5xx response is
// surfaced as ApiError with the daemon's error message when available.
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
  return (parsed ?? {}) as T
}

export interface ActionRequest {
  anvil: string
  reason?: string
  note?: string
  label?: string
  force_run?: boolean
}

export const actions = {
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
