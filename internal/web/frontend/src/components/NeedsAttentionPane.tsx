import { useState } from 'react'
import { AlertTriangle, ChevronDown, ChevronRight } from 'lucide-react'
import type { EscalationType, NeedsAttentionItem, NeedsAttentionResponse } from '../api/forge'
import { useApiPoll } from '../hooks/useApiPoll'
import { relativeTime } from '../lib/format'
import Pane, { EmptyState } from './Pane'
import ResolveNeedsAttentionPanel from './ResolveNeedsAttentionPanel'

const POLL_INTERVAL_MS = 5000

// TYPE_LABEL gives each escalation type a short human label for the row badge.
// Keyed by EscalationType so adding a type to the API forces a label here.
const TYPE_LABEL: Record<EscalationType, string> = {
  dispatch_failed: 'dispatch failed',
  smith_failed: 'smith failed',
  recovery_failed: 'recovery failed',
  dispatch_blocked_stranded_branch: 'stranded branch',
  clarification: 'clarification',
  pr_create_failed: 'PR create failed',
}

// TYPE_BADGE tints the escalation-type badge so the operator can scan the
// list by class. Stranded-branch and clarification get distinct hues because
// their resolution flows differ most from the generic failure set.
const TYPE_BADGE: Record<EscalationType, string> = {
  dispatch_failed: 'border-red-500/40 bg-red-500/10 text-red-300',
  smith_failed: 'border-red-500/40 bg-red-500/10 text-red-300',
  recovery_failed: 'border-orange-500/40 bg-orange-500/10 text-orange-300',
  dispatch_blocked_stranded_branch:
    'border-amber-500/40 bg-amber-500/10 text-amber-300',
  clarification: 'border-sky-500/40 bg-sky-500/10 text-sky-300',
  pr_create_failed: 'border-amber-500/40 bg-amber-500/10 text-amber-300',
}

// rowKey identifies a needs-attention row by bead+anvil, matching the
// resolve-store key shape so two same-id beads in different anvils stay
// distinct.
function rowKey(item: NeedsAttentionItem): string {
  return `${item.anvil}/${item.bead_id}`
}

// NeedsAttentionPane is the bead-centric needs-attention surface (Forge-iz6s).
// It polls GET /api/forge/needs-attention — driven by the retries table, not
// the workers table — so every needs_human / clarification bead is listed and
// resolvable regardless of whether a live worker row still exists. Each row
// expands to the shared ResolveNeedsAttentionPanel, passing the real
// escalation type (so the action set matches the failure mode) and the owning
// anvil (so the escalation fetch is unambiguous).
export default function NeedsAttentionPane() {
  const { data, loading, error } = useApiPoll<NeedsAttentionResponse>(
    '/api/forge/needs-attention',
    POLL_INTERVAL_MS,
  )
  const items = data?.items ?? []
  // expandedKey is the single row whose resolve panel is open. Only one at a
  // time keeps the pane compact; toggling the same row (or the panel's close
  // button) collapses it.
  const [expandedKey, setExpandedKey] = useState<string | null>(null)
  // collapsed hides the whole pane body. The pane is always present so the
  // operator can find escalations, but it starts collapsed-able for a clean
  // dashboard; default expanded so escalations are visible by default.
  const [collapsed, setCollapsed] = useState(false)

  return (
    <Pane
      title="Needs Attention"
      icon={<AlertTriangle size={16} className="text-amber-400" aria-hidden />}
      count={items.length}
      loading={loading}
      error={error}
      collapsible
      expanded={!collapsed}
      onToggle={() => setCollapsed((c) => !c)}
    >
      {items.length === 0 && !loading ? (
        <EmptyState message="No beads need attention." />
      ) : (
        <ul className="divide-y divide-slate-800">
          {items.map((item) => {
            const key = rowKey(item)
            const isExpanded = expandedKey === key
            const toggle = () =>
              setExpandedKey((prev) => (prev === key ? null : key))
            return (
              <li key={key}>
                <button
                  type="button"
                  onClick={toggle}
                  aria-expanded={isExpanded}
                  aria-controls={`needs-attention-resolve-${key}`}
                  data-testid={`needs-attention-row-${item.bead_id}`}
                  className="flex w-full items-start gap-2 px-4 py-3 text-left transition-colors hover:bg-slate-800/40 focus:bg-slate-800/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
                >
                  {isExpanded ? (
                    <ChevronDown
                      size={14}
                      className="mt-1 shrink-0 text-slate-400"
                      aria-hidden
                    />
                  ) : (
                    <ChevronRight
                      size={14}
                      className="mt-1 shrink-0 text-slate-400"
                      aria-hidden
                    />
                  )}
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span
                        className={`shrink-0 rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${TYPE_BADGE[item.escalation_type]}`}
                      >
                        {TYPE_LABEL[item.escalation_type] ??
                          item.escalation_type}
                      </span>
                      {!item.worker_row_exists && (
                        <span
                          className="rounded-md border border-slate-700 bg-slate-800/60 px-2 py-0.5 text-[10px] uppercase tracking-wide text-slate-400"
                          title="No live worker row — resolvable only from this panel"
                        >
                          no worker row
                        </span>
                      )}
                    </div>
                    <p className="mt-1.5 truncate text-sm font-medium text-slate-100">
                      {item.title || item.bead_id}
                    </p>
                    <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
                      <span className="font-mono text-slate-400">
                        {item.bead_id}
                      </span>
                      <span aria-hidden>·</span>
                      <span>{item.anvil}</span>
                      {item.updated_at && (
                        <>
                          <span aria-hidden>·</span>
                          <span title={item.updated_at}>
                            {relativeTime(item.updated_at)}
                          </span>
                        </>
                      )}
                    </p>
                    {item.last_error && (
                      <p className="mt-1 truncate text-xs text-amber-300/80">
                        {item.last_error}
                      </p>
                    )}
                  </div>
                </button>
                {isExpanded && (
                  <div
                    id={`needs-attention-resolve-${key}`}
                    className="border-t border-slate-800/60 bg-slate-950/30 px-4 py-3"
                  >
                    <ResolveNeedsAttentionPanel
                      escalationId={item.bead_id}
                      escalationType={item.escalation_type}
                      anvil={item.anvil}
                      onClose={() => setExpandedKey(null)}
                    />
                  </div>
                )}
              </li>
            )
          })}
        </ul>
      )}
    </Pane>
  )
}
