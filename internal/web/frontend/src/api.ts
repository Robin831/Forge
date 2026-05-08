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
