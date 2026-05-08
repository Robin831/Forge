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
}

export interface QueueResponse {
  items: QueueItem[]
}

export interface WorkerInfo {
  id: string
  bead_id: string
  anvil: string
  branch?: string
  title?: string
  status: string
  phase?: string
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
