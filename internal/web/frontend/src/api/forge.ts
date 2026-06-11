// Typed client for the Forge resolve / escalation endpoints. Kept in a
// separate module from the general api.ts so the resolve-needs-attention
// panel can import a tight surface area, and so the verb table stays
// co-located with the request shape that callers build.

import { apiGet, apiPost } from '../api'

// ResolveVerb mirrors the actions accepted by POST /api/forge/resolve
// (see internal/web/forge_resolve.go). The first five route to the
// daemon's queue_* IPC handlers; `approve-as-is` and `warden-rerun` are
// Forge-level dispatch overrides — bypass-warden and re-review-only,
// respectively — added so pod-hosted forges can ship a worker's existing
// branch without a laptop git checkout.
export type ResolveVerb =
  | 'clear'
  | 'retry'
  | 'clarify'
  | 'unclarify'
  | 'stop'
  | 'approve-as-is'
  | 'warden-rerun'
  | 'create-pr'

// RESOLVE_VERBS is the canonical ordered list of verbs the backend
// accepts. Exporting it lets the panel render the action buttons without
// hard-coding the list a second time.
export const RESOLVE_VERBS: readonly ResolveVerb[] = [
  'clear',
  'retry',
  'clarify',
  'unclarify',
  'stop',
  'approve-as-is',
  'warden-rerun',
  'create-pr',
] as const

// EscalationType discriminates the needs-attention class of a bead and drives
// which resolve actions the panel offers. Mirrors the escalation_type values
// returned by GET /api/forge/needs-attention (see internal/web/
// forge_needs_attention.go). 'dispatch_failed' / 'smith_failed' are the two
// original worker/bead modes; the rest were added by Forge-iz6s so the
// bead-centric surface can classify graceful and stranded-branch escalations.
export type EscalationType =
  | 'dispatch_failed'
  | 'smith_failed'
  | 'recovery_failed'
  | 'dispatch_blocked_stranded_branch'
  | 'clarification'
  | 'pr_create_failed'

// NeedsAttentionItem is one row of GET /api/forge/needs-attention. It mirrors
// `needsAttentionItem` in internal/web/forge_needs_attention.go and is driven
// by the retries table rather than the workers table, so it surfaces every
// needs_human / clarification bead regardless of worker-row state.
export interface NeedsAttentionItem {
  bead_id: string
  anvil: string
  title?: string
  escalation_type: EscalationType
  needs_human: boolean
  clarification_needed: boolean
  dispatch_failures: number
  recovery_failures: number
  last_error?: string
  updated_at?: string
  worker_row_exists: boolean
}

export interface NeedsAttentionResponse {
  items: NeedsAttentionItem[]
}

// fetchNeedsAttention loads the bead-centric needs-attention list. The panel
// polls this on the dashboard's interval so resolved beads drop off the list
// on the next tick.
export function fetchNeedsAttention(
  signal?: AbortSignal,
): Promise<NeedsAttentionResponse> {
  return apiGet<NeedsAttentionResponse>('/api/forge/needs-attention', signal)
}

// RetryDetail mirrors the `retry` block of the escalation response. Fields
// are best-effort: the daemon returns `undefined` for beads that never
// reached the retry table (e.g. workers killed before their first retry).
export interface RetryDetail {
  needs_human: boolean
  clarification_needed: boolean
  dispatch_failures: number
  recovery_failures: number
  updated_at?: string
}

// EscalationGitContext bundles the git context the backend gathers from
// the worker's worktree. Fields stay optional so partial responses (e.g.
// the worktree exists but `origin/<branch>` was never pushed) still
// deserialise cleanly.
export interface EscalationGitContext {
  parent_base?: string
  diff_range?: string
  origin_branch_ref?: string
  origin_branch_exists: boolean
  origin_commits?: string[]
  local_commits?: string[]
  diff_stat?: string
}

// EscalationDetail is the JSON shape served by
// GET /api/forge/escalation/{bead_id}. Mirrors `escalationResponse` in
// internal/web/forge_resolve.go.
export interface EscalationDetail {
  bead_id: string
  anvil: string
  branch?: string
  worktree_path?: string
  worktree_exists: boolean
  escalation_message: string
  retry?: RetryDetail
  context?: EscalationGitContext
  errors?: string[]
}

// ResolveRequest is the body of POST /api/forge/resolve. anvil is required
// by the backend even though the JSON field is optional; the helper
// surfaces it as required so callers cannot forget to thread it through.
// forgeId is passthrough for multi-forge safety checks (see daemon IPC).
export interface ResolveRequest {
  verb: ResolveVerb
  anvil: string
  note?: string
  forgeId?: string
}

// ResolveResponse is the daemon's response to a resolve action.
//
// For synchronous verbs (HTTP 200) the server forwards the IPC payload
// verbatim — e.g. create-pr returns `{message, pr_number, pr_url}`.
//
// For async verbs (HTTP 202) the server returns
// `{queued: true, request_id, message?}`.
//
// Both shapes are merged here so callers can destructure whichever
// fields the verb provides.
export interface ResolveResponse {
  queued?: boolean
  request_id?: string
  message?: string
  pr_number?: number
  pr_url?: string
}

// fetchEscalation calls GET /api/forge/escalation/{bead_id}. When the bead
// exists in more than one anvil the backend returns an entry in
// `errors[]` and asks the caller to retry with `anvil`; the helper exposes
// the optional anvil hint as the second argument so callers can supply it
// up front.
export function fetchEscalation(
  beadID: string,
  anvil?: string,
  signal?: AbortSignal,
): Promise<EscalationDetail> {
  const params = new URLSearchParams()
  if (anvil) params.set('anvil', anvil)
  const qs = params.toString()
  const suffix = qs ? `?${qs}` : ''
  return apiGet<EscalationDetail>(
    `/api/forge/escalation/${encodeURIComponent(beadID)}${suffix}`,
    signal,
  )
}

// postResolve dispatches a resolve verb to POST /api/forge/resolve. The
// request body field names match the backend (snake_case); the helper
// translates the camelCase request shape so callers don't have to know
// the wire format.
export function postResolve(
  beadID: string,
  req: ResolveRequest,
): Promise<ResolveResponse> {
  return apiPost<ResolveResponse>('/api/forge/resolve', {
    bead_id: beadID,
    action: req.verb,
    anvil_name: req.anvil,
    note: req.note,
    forge_id: req.forgeId,
  })
}
