import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import {
  AlertTriangle,
  ChevronDown,
  ChevronRight,
  MonitorOff,
  Skull,
  Users,
} from 'lucide-react'
import { actions, type WorkerInfo } from '../api'
import { useAction } from '../hooks/useAction'
import { useUIState } from '../hooks/useUIState'
import { relativeTime } from '../lib/format'
import { isBellowsMonitor } from './PipelineBar'
import ConfirmModal from './ConfirmModal'
import Pane, { EmptyState } from './Pane'
import ResolveNeedsAttentionPanel from './ResolveNeedsAttentionPanel'

interface WorkersPaneProps {
  workers: WorkerInfo[]
  loading: boolean
  error: string | null
  // maxTotalSmiths is the global concurrent-Smith cap reported by /api/status.
  // The pane renders (maxTotalSmiths - active workers) dimmed Idle slots so
  // the user can see remaining capacity at a glance. Zero or negative values
  // disable the placeholders entirely (e.g. when the daemon has not yet
  // reported a value).
  maxTotalSmiths?: number
  onSelectWorker?: (worker: WorkerInfo) => void
  onActionSuccess?: () => void
}

const STATUS_CLASSES: Record<string, string> = {
  pending: 'bg-slate-700/60 text-slate-200 border-slate-600/60',
  running: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  done: 'bg-sky-500/20 text-sky-300 border-sky-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
}

function statusClass(status: string): string {
  return STATUS_CLASSES[status] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}

// phaseBadgeClass styles the per-worker phase chip. Most phases share the
// neutral slate chip, but the Assay verification pass gets a distinct cyan
// accent — echoing the Assay findings panel's cyan theme — so the review/
// verify worker row stands out from in-flight Smith phases at a glance.
function phaseBadgeClass(phase: string): string {
  if (phase === 'assay') {
    return 'border-cyan-500/40 bg-cyan-500/10 text-cyan-300'
  }
  return 'border-slate-700 bg-slate-800/60 text-slate-300'
}

// Encode an arbitrary anvil name into a valid HTML id token using the same
// percent-encoding scheme as QueuePane (replace % with _ for readability).
// The `workers-` prefix keeps IDs unique when both panes appear on the same
// page, since HTML ids must be document-scoped.
function anvilDomId(name: string): string {
  return `workers-anvil-body-${encodeURIComponent(name).replace(/%/g, '_')}`
}

interface AnvilGroup {
  anvil: string
  workers: WorkerInfo[]
}

// groupWorkersByAnvil partitions the (already filtered + sorted) worker list
// into per-anvil sections. Groups are ordered by their newest worker's
// started_at descending so the most recently active anvil appears first,
// preserving the "newest at top" intent across the whole pane. Workers
// within each group keep their incoming order (newest started_at first).
function groupWorkersByAnvil(workers: WorkerInfo[]): AnvilGroup[] {
  const byAnvil = new Map<string, AnvilGroup>()
  for (const w of workers) {
    let group = byAnvil.get(w.anvil)
    if (!group) {
      group = { anvil: w.anvil, workers: [] }
      byAnvil.set(w.anvil, group)
    }
    group.workers.push(w)
  }
  return Array.from(byAnvil.values()).sort((a, b) => {
    const aT = Date.parse(a.workers[0]?.started_at) || 0
    const bT = Date.parse(b.workers[0]?.started_at) || 0
    return bT - aT || a.anvil.localeCompare(b.anvil)
  })
}

export default function WorkersPane({
  workers,
  loading,
  error,
  maxTotalSmiths = 0,
  onSelectWorker,
  onActionSuccess,
}: WorkersPaneProps) {
  const { run, busy } = useAction()
  const [killTarget, setKillTarget] = useState<WorkerInfo | null>(null)
  // expandedRowId is the worker id whose ResolveNeedsAttentionPanel is
  // currently expanded inline beneath its row. Only one row's panel is
  // visible at a time so the pane keeps a compact footprint; clicking the
  // toggle a second time (or the panel's close button) collapses it back.
  const [expandedRowId, setExpandedRowId] = useState<string | null>(null)

  // collapsed is keyed by anvil name. Missing keys (and explicit `false`)
  // render the group expanded — matching the pre-grouping behaviour where
  // every worker was visible by default. localStorage so per-anvil
  // preferences survive a browser restart, mirroring QueuePane's split.
  const [collapsed, setCollapsed] = useUIState<Record<string, boolean>>(
    'workers-pane.group-collapsed',
    {},
    { storage: 'local' },
  )
  // Scroll position is transient navigation state — sessionStorage so it
  // survives a back-nav round-trip but resets when the tab closes.
  const [scrollTop, setScrollTop] = useUIState<number>(
    'workers-pane.scroll',
    0,
    { storage: 'session' },
  )
  const bodyRef = useRef<HTMLDivElement | null>(null)

  // The Bellows PR-monitor row is intentionally filtered out — it produces no
  // smith log, so a row with no log modal would look broken. Its state is now
  // surfaced inside the Pipeline bar's PR stage. Bellows-spawned sub-workers
  // (quench/burnish/rebase) keep their own phase and remain clickable here.
  const visibleWorkers = useMemo(
    () => workers.filter((w) => !isBellowsMonitor(w)),
    [workers],
  )

  // Sort by started_at descending so workers within each anvil group appear
  // newest first. groupWorkersByAnvil then orders groups by their newest
  // member, so the overall list is newest-group-first, newest-within-group.
  const sorted = useMemo(
    () =>
      [...visibleWorkers].sort((a, b) => {
        const aT = Date.parse(a.started_at) || 0
        const bT = Date.parse(b.started_at) || 0
        return bT - aT
      }),
    [visibleWorkers],
  )

  const groups = useMemo(() => groupWorkersByAnvil(sorted), [sorted])

  // Idle slot count = (configured cap) - (active Smith-like workers). We count
  // workers that occupy a Smith slot (pending/running/reviewing) and that are
  // not bellows monitors (already filtered above). "reviewing" covers Warden
  // phase workers (state.WorkerReviewing) which also hold a slot. When the
  // daemon reports a cap of 0 we omit the placeholders entirely.
  const activeSlotWorkers = sorted.filter(
    (w) => w.status === 'pending' || w.status === 'running' || w.status === 'reviewing',
  )
  const idleCount = Math.max(0, maxTotalSmiths - activeSlotWorkers.length)

  // Restore scroll position before the browser paints so users see no jump
  // from 0 → saved on back-navigation. The dep on `loading` re-fires once
  // the polled data arrives so the list has actual height to scroll into.
  useLayoutEffect(() => {
    const el = bodyRef.current
    if (!el) return
    if (scrollTop > 0 && el.scrollTop !== scrollTop) {
      el.scrollTop = scrollTop
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [loading])

  // Throttle scroll capture so we don't write to storage on every pixel.
  // The hook itself debounces writes by 150ms, but updating React state on
  // every scroll event still causes wasteful renders.
  useEffect(() => {
    const el = bodyRef.current
    if (!el) return
    let rafId = 0
    const onScroll = () => {
      if (rafId) return
      rafId = window.requestAnimationFrame(() => {
        rafId = 0
        setScrollTop(el.scrollTop)
      })
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => {
      el.removeEventListener('scroll', onScroll)
      if (rafId) window.cancelAnimationFrame(rafId)
    }
  }, [setScrollTop])

  const toggleGroup = (anvil: string) =>
    setCollapsed((prev) => ({ ...prev, [anvil]: !prev[anvil] }))

  const handleKill = async () => {
    if (!killTarget) return
    const ok = await run(() => actions.killWorker(killTarget.id), {
      successMessage: `Kill signal sent to worker ${killTarget.id.slice(0, 8)}`,
      onSuccess: onActionSuccess,
    })
    if (ok) setKillTarget(null)
  }

  return (
    <>
      <Pane
        title="Workers"
        icon={<Users size={16} className="text-sky-400" aria-hidden />}
        count={visibleWorkers.length}
        loading={loading}
        error={error}
        bodyRef={bodyRef}
      >
        {sorted.length === 0 && idleCount === 0 && !loading ? (
          <EmptyState message="No active workers." />
        ) : (
          <ul className="divide-y divide-slate-800">
            {groups.map((group) => {
              const isCollapsed = !!collapsed[group.anvil]
              return (
                <li key={group.anvil}>
                  <button
                    type="button"
                    data-testid={`workers-group-${group.anvil}`}
                    onClick={() => toggleGroup(group.anvil)}
                    aria-expanded={!isCollapsed}
                    aria-controls={anvilDomId(group.anvil)}
                    className="flex w-full items-center gap-2 px-4 py-2.5 text-left text-sm font-semibold text-slate-100 hover:bg-slate-800/40 focus:bg-slate-800/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
                  >
                    {isCollapsed ? (
                      <ChevronRight size={14} className="text-slate-400" aria-hidden />
                    ) : (
                      <ChevronDown size={14} className="text-slate-400" aria-hidden />
                    )}
                    <span className="truncate">{group.anvil}</span>
                    <span className="ml-auto rounded-full bg-slate-800 px-2 py-0.5 text-xs font-normal text-slate-300">
                      {group.workers.length}
                    </span>
                  </button>
                  {!isCollapsed && (
                    <ul
                      id={anvilDomId(group.anvil)}
                      className="divide-y divide-slate-800/60 border-t border-slate-800/60"
                    >
                      {group.workers.map((w) => {
                        // Bellows pseudo-workers monitor a PR; they have no claude log
                        // so the modal would just render an error. Render their cards
                        // as static info rather than clickable buttons. The phase
                        // fallback is for older API clients that don't yet send `kind`.
                        const isBellows = w.kind === 'bellows' || w.phase === 'bellows'
                        const hasLog = !!w.log_path && !isBellows
                        const clickable = hasLog && !!onSelectWorker
                        const canKill = w.status === 'pending' || w.status === 'running'
                        // Only failed workers have a smith_failed escalation the
                        // daemon can resolve. Healthy / in-flight workers have
                        // nothing for the panel to fetch, so we hide the toggle.
                        const canResolve = w.status === 'failed' && !isBellows
                        const isExpanded = expandedRowId === w.id
                        const toggleExpand = () =>
                          setExpandedRowId((prev) => (prev === w.id ? null : w.id))
                        return (
                          <li key={w.id}>
                            <div className="flex items-stretch">
                              <div className="min-w-0 flex-1">
                                <button
                                  type="button"
                                  disabled={!clickable}
                                  onClick={() => {
                                    if (clickable) onSelectWorker?.(w)
                                  }}
                                  className={`block w-full px-4 py-3 text-left ${
                                    clickable
                                      ? 'cursor-pointer transition-colors hover:bg-slate-800/40 focus:bg-slate-800/40 focus:outline-none focus:ring-1 focus:ring-amber-400/40'
                                      : 'opacity-80'
                                  }`}
                                  aria-label={
                                    clickable ? `Open log for ${w.title || w.bead_id}` : undefined
                                  }
                                >
                                  <div className="flex flex-wrap items-start gap-2">
                                    <span
                                      className={`shrink-0 rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${statusClass(w.status)}`}
                                    >
                                      {w.status}
                                    </span>
                                    {w.phase && (
                                      <span
                                        className={`rounded-md border px-2 py-0.5 text-[10px] uppercase tracking-wide ${phaseBadgeClass(w.phase)}`}
                                      >
                                        {w.phase}
                                      </span>
                                    )}
                                    {w.pr_number ? (
                                      w.pr_url ? (
                                        <a
                                          href={w.pr_url}
                                          target="_blank"
                                          rel="noreferrer"
                                          onClick={(e) => e.stopPropagation()}
                                          title={w.pr_url}
                                          className="rounded-md border border-purple-500/40 bg-purple-500/10 px-2 py-0.5 text-[10px] text-purple-300 hover:bg-purple-500/20 hover:underline"
                                        >
                                          PR #{w.pr_number}
                                        </a>
                                      ) : (
                                        <span className="rounded-md border border-purple-500/40 bg-purple-500/10 px-2 py-0.5 text-[10px] text-purple-300">
                                          PR #{w.pr_number}
                                        </span>
                                      )
                                    ) : null}
                                    {clickable && (
                                      <span className="ml-auto text-[10px] uppercase tracking-wide text-slate-500">
                                        view log
                                      </span>
                                    )}
                                  </div>
                                  <p className="mt-1.5 truncate text-sm font-medium text-slate-100">
                                    {w.title || w.bead_id}
                                  </p>
                                  <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
                                    <span className="font-mono text-slate-400">{w.bead_id}</span>
                                    <span aria-hidden>·</span>
                                    <span>{w.anvil}</span>
                                    <span aria-hidden>·</span>
                                    <span title={w.started_at}>{relativeTime(w.started_at)}</span>
                                  </p>
                                </button>
                              </div>
                              {canResolve && (
                                <div className="flex items-start p-2">
                                  <button
                                    type="button"
                                    onClick={toggleExpand}
                                    aria-expanded={isExpanded}
                                    aria-controls={`workers-resolve-${w.id}`}
                                    data-testid={`workers-resolve-toggle-${w.id}`}
                                    className="rounded-md border border-amber-500/40 bg-amber-500/10 p-1.5 text-amber-300 hover:bg-amber-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-400"
                                    aria-label={
                                      isExpanded
                                        ? `Hide resolution panel for ${w.bead_id}`
                                        : `Show resolution panel for ${w.bead_id}`
                                    }
                                    title={
                                      isExpanded
                                        ? 'Hide resolution panel'
                                        : 'Resolve needs attention'
                                    }
                                  >
                                    <AlertTriangle size={14} />
                                  </button>
                                </div>
                              )}
                              {canKill && (
                                <div className="flex items-start p-2">
                                  <button
                                    type="button"
                                    onClick={() => setKillTarget(w)}
                                    disabled={busy}
                                    className="rounded-md border border-red-500/40 bg-red-500/10 p-1.5 text-red-300 hover:bg-red-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-red-400 disabled:opacity-50"
                                    aria-label={`Kill worker ${w.id}`}
                                    title="Kill worker"
                                  >
                                    <Skull size={14} />
                                  </button>
                                </div>
                              )}
                            </div>
                            {canResolve && isExpanded && (
                              <div
                                id={`workers-resolve-${w.id}`}
                                className="border-t border-slate-800/60 bg-slate-950/30 px-4 py-3"
                              >
                                <ResolveNeedsAttentionPanel
                                  escalationId={w.bead_id}
                                  escalationType="smith_failed"
                                  onClose={() => setExpandedRowId(null)}
                                />
                              </div>
                            )}
                          </li>
                        )
                      })}
                    </ul>
                  )}
                </li>
              )
            })}
            {idleCount > 0 &&
              Array.from({ length: idleCount }).map((_, i) => (
                <li
                  key={`idle-${i}`}
                  data-testid="workers-idle-slot"
                  className="flex items-center gap-3 border-t border-dashed border-slate-800/80 px-4 py-3 text-slate-600"
                  aria-label={`Idle slot ${i + 1}`}
                >
                  <MonitorOff size={16} aria-hidden />
                  <span className="text-xs uppercase tracking-wide">Idle</span>
                </li>
              ))}
          </ul>
        )}
      </Pane>

      <ConfirmModal
        open={killTarget !== null}
        title="Kill worker?"
        message={
          killTarget
            ? `This will SIGTERM the Smith process for ${killTarget.bead_id} (${killTarget.anvil}). Any in-progress changes will be lost.`
            : ''
        }
        confirmLabel="Kill worker"
        tone="danger"
        busy={busy}
        onConfirm={handleKill}
        onCancel={() => setKillTarget(null)}
      />
    </>
  )
}
