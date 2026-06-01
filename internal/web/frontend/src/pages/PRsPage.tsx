import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  AlertTriangle,
  Bell,
  GitMerge,
  GitPullRequest,
  MessageSquare,
  RefreshCw,
  RotateCcw,
  ScanSearch,
  ShieldCheck,
  Wrench,
  X,
  XCircle,
} from 'lucide-react'
import { useApiPoll } from '../hooks/useApiPoll'
import { useAction } from '../hooks/useAction'
import { useToast } from '../hooks/useToast'
import { useUIState } from '../hooks/useUIState'
import { prActions, type PRActionKind, type PRItem, type StatusResponse } from '../api'
import AppHeader from '../components/AppHeader'
import ConfirmModal from '../components/ConfirmModal'
import Pane, { EmptyState } from '../components/Pane'
import PRFindingsPanel from '../components/PRFindingsPanel'
import {
  PR_SECTION_DESCRIPTIONS,
  PR_SECTION_EMPTY_MESSAGES,
  PR_SECTION_TITLES,
  type PRSectionKind,
} from './prsTypes'
import { PRS_CACHE_TTL_MS, usePRsData } from './usePRsData'

const STATUS_POLL_INTERVAL_MS = 10_000

const SECTION_ORDER: PRSectionKind[] = ['forge_prs', 'external_prs', 'recently_merged']

export type PRSortKey =
  | 'updated-desc'
  | 'updated-asc'
  | 'created-desc'
  | 'created-asc'
  | 'number-desc'
  | 'number-asc'
  | 'title-asc'

const SORT_OPTIONS: ReadonlyArray<{ value: PRSortKey; label: string }> = [
  { value: 'updated-desc', label: 'Updated (newest first)' },
  { value: 'updated-asc', label: 'Updated (oldest first)' },
  { value: 'created-desc', label: 'Created (newest first)' },
  { value: 'created-asc', label: 'Created (oldest first)' },
  { value: 'number-desc', label: 'PR # (high to low)' },
  { value: 'number-asc', label: 'PR # (low to high)' },
  { value: 'title-asc', label: 'Title (A→Z)' },
]

// parseTimestamp / compareTimestamps mirror the helpers in QueuePane so missing
// or unparseable created_at / updated_at values degrade gracefully: items
// without a timestamp sort to the "oldest" end regardless of direction.
function parseTimestamp(value: string | undefined): number {
  if (!value) return NaN
  const t = Date.parse(value)
  return Number.isNaN(t) ? NaN : t
}

function compareTimestamps(a: number, b: number, direction: 'asc' | 'desc'): number {
  const aMissing = Number.isNaN(a)
  const bMissing = Number.isNaN(b)
  if (aMissing && bMissing) return 0
  if (aMissing) return direction === 'asc' ? -1 : 1
  if (bMissing) return direction === 'asc' ? 1 : -1
  return direction === 'asc' ? a - b : b - a
}

export function sortPRs(items: PRItem[], sortKey: PRSortKey): PRItem[] {
  const copy = items.slice()
  switch (sortKey) {
    case 'updated-desc':
    case 'updated-asc': {
      const dir = sortKey === 'updated-desc' ? 'desc' : 'asc'
      const ts = new Map(copy.map((item) => [item, parseTimestamp(item.updated_at)]))
      copy.sort((a, b) => compareTimestamps(ts.get(a)!, ts.get(b)!, dir))
      return copy
    }
    case 'created-desc':
    case 'created-asc': {
      const dir = sortKey === 'created-desc' ? 'desc' : 'asc'
      const ts = new Map(copy.map((item) => [item, parseTimestamp(item.created_at)]))
      copy.sort((a, b) => compareTimestamps(ts.get(a)!, ts.get(b)!, dir))
      return copy
    }
    case 'number-desc':
      copy.sort((a, b) => b.number - a.number)
      return copy
    case 'number-asc':
      copy.sort((a, b) => a.number - b.number)
      return copy
    case 'title-asc':
      copy.sort((a, b) =>
        (a.title || '').localeCompare(b.title || '', undefined, { sensitivity: 'base' }),
      )
      return copy
    default:
      // Stale or corrupted storage value — return unsorted as a safe fallback.
      return copy
  }
}

export function filterPRs(items: PRItem[], filter: string): PRItem[] {
  const q = filter.trim().toLowerCase()
  if (!q) return items
  return items.filter((pr) => {
    if (pr.title && pr.title.toLowerCase().includes(q)) return true
    if (pr.bead_id && pr.bead_id.toLowerCase().includes(q)) return true
    if (pr.anvil.toLowerCase().includes(q)) return true
    if (pr.branch && pr.branch.toLowerCase().includes(q)) return true
    if (pr.author && pr.author.toLowerCase().includes(q)) return true
    if (String(pr.number).includes(q) || `#${pr.number}`.includes(q)) return true
    return false
  })
}

const SECTION_ICON_CLASSES: Record<PRSectionKind, string> = {
  forge_prs: 'text-amber-400',
  external_prs: 'text-sky-400',
  recently_merged: 'text-emerald-400',
}

// PendingAction couples an action with the row it targets so the confirm
// modal has all the context it needs (PR title/number for the message,
// `kind` so the success/failure copy is action-specific).
interface PendingAction {
  kind: PRActionKind
  pr: PRItem
}

// PRsPage is the shell for the Hearth 2.0 /prs tab. It owns:
//   - the polled status pill in the AppHeader
//   - the cached usePRsData fetch + manual refresh button
//   - per-PR action buttons that route through /api/prs/{id}/<action>
//
// Per-row action buttons dispatch via the useAction hook (toast + busy
// flag). Forge PRs and recently-merged PRs see the full set of actions;
// external PRs only see merge / approve / close / bellows since the
// branch-bound lifecycle workers (fix-ci, fix-comments, fix-conflicts)
// require a Forge-managed branch.
export default function PRsPage() {
  const status = useApiPoll<StatusResponse>('/api/status', STATUS_POLL_INTERVAL_MS)
  const { forge_prs, external_prs, recently_merged, loading, error, refresh, fetchedAt } =
    usePRsData()

  // filter / scroll are transient navigation state — sessionStorage so they
  // survive a back-nav round-trip but reset when the tab closes. sort and
  // per-section expanded state are true preferences — localStorage so they
  // survive a browser restart. Mirrors the mixed split documented in
  // QueuePane; keys are namespaced by route via useUIState's default scope.
  const [filter, setFilter] = useUIState<string>('pr.filter', '', { storage: 'session' })
  const filterInputRef = useRef<HTMLInputElement>(null)
  const [sortKey, setSortKey] = useUIState<PRSortKey>('pr.sort', 'updated-desc', {
    storage: 'local',
  })
  // Each section has its own expanded key so the storage layout matches the
  // `pr.section.<id>.expanded` namespace from the bead spec. Sections default
  // to expanded so the first-time experience preserves the prior layout.
  const [forgeExpanded, setForgeExpanded] = useUIState<boolean>(
    'pr.section.forge_prs.expanded',
    true,
    { storage: 'local' },
  )
  const [externalExpanded, setExternalExpanded] = useUIState<boolean>(
    'pr.section.external_prs.expanded',
    true,
    { storage: 'local' },
  )
  const [mergedExpanded, setMergedExpanded] = useUIState<boolean>(
    'pr.section.recently_merged.expanded',
    true,
    { storage: 'local' },
  )
  const expandedBySection: Record<PRSectionKind, boolean> = {
    forge_prs: forgeExpanded,
    external_prs: externalExpanded,
    recently_merged: mergedExpanded,
  }
  const toggleSection = (kind: PRSectionKind) => {
    if (kind === 'forge_prs') setForgeExpanded(!forgeExpanded)
    else if (kind === 'external_prs') setExternalExpanded(!externalExpanded)
    else setMergedExpanded(!mergedExpanded)
  }
  // Window-level scroll because the /prs route scrolls at the document level
  // — there's no single scroll container around the three Panes. We capture
  // window.scrollY and restore it on remount in the same useLayoutEffect
  // pattern as QueuePane/WorkersPane.
  const [scrollTop, setScrollTop] = useUIState<number>('pr.scroll', 0, { storage: 'session' })

  // Apply filter + sort. Each section is filtered and sorted independently so
  // the per-section counts reflect what the user is actually seeing.
  const visibleItems = useMemo<Record<PRSectionKind, PRItem[]>>(
    () => ({
      forge_prs: sortPRs(filterPRs(forge_prs, filter), sortKey),
      external_prs: sortPRs(filterPRs(external_prs, filter), sortKey),
      recently_merged: sortPRs(filterPRs(recently_merged, filter), sortKey),
    }),
    [forge_prs, external_prs, recently_merged, filter, sortKey],
  )

  // Restore window scroll before the browser paints so users see no jump from
  // 0 → saved on back-navigation. The dep on `loading` re-fires once polled
  // data arrives so the page has the height the saved scrollTop expects.
  useLayoutEffect(() => {
    if (scrollTop > 0 && window.scrollY !== scrollTop) {
      window.scrollTo(0, scrollTop)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [loading])

  // Throttle scroll capture so we don't write to storage on every pixel.
  // useUIState debounces writes by 150ms, but updating React state on every
  // scroll event would still cause wasteful renders.
  useEffect(() => {
    let rafId = 0
    const onScroll = () => {
      if (rafId) return
      rafId = window.requestAnimationFrame(() => {
        rafId = 0
        setScrollTop(window.scrollY)
      })
    }
    window.addEventListener('scroll', onScroll, { passive: true })
    return () => {
      window.removeEventListener('scroll', onScroll)
      if (rafId) window.cancelAnimationFrame(rafId)
    }
  }, [setScrollTop])

  const { run, busy } = useAction()
  const toast = useToast()
  // actingKey tracks which row+action is mid-flight so we can disable just
  // that button rather than every button on the page (the global `busy`
  // flag from useAction would do the latter).
  const [actingKey, setActingKey] = useState<string | null>(null)
  const [pending, setPending] = useState<PendingAction | null>(null)
  // expandedFindingsKey is the row whose Assay findings panel is open. Only
  // one panel is visible at a time so the page keeps a compact footprint —
  // mirrors WorkersPane's single-open resolve panel. The key is the same
  // identity PRRow uses for its React key so it survives re-sorts.
  const [expandedFindingsKey, setExpandedFindingsKey] = useState<string | null>(null)

  const requestAction = (pr: PRItem, kind: PRActionKind) => {
    setPending({ pr, kind })
  }

  const closeConfirm = () => setPending(null)

  const handleConfirm = async () => {
    if (!pending) return
    const { pr, kind } = pending
    if (pr.id === undefined) {
      // Defensive: forge_prs and recently_merged always have IDs in state.db,
      // and external_prs are reconciled into the same table with synthetic
      // ext-* bead IDs (still a real numeric PR ID). If we ever surface a
      // PR row without an ID, report it so the user knows the action didn't run.
      toast.push('Cannot run action: PR record has no database ID', 'error')
      closeConfirm()
      return
    }
    const key = `${pr.id}-${kind}`
    setActingKey(key)
    const ok = await run(() => prActions.run(pr.id!, kind), {
      successMessage: actionSuccessMessage(kind, pr),
      onSuccess: refresh,
    })
    setActingKey(null)
    if (ok) closeConfirm()
  }

  return (
    <div className="flex min-h-full flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <p className="text-xs text-slate-500">
          {fetchedAt
            ? `Last updated ${formatRelative(fetchedAt)}`
            : loading
              ? 'Loading…'
              : 'Not yet loaded'}
        </p>
        <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
          <div className="relative w-full sm:w-64">
            <input
              ref={filterInputRef}
              type="text"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Filter PRs (title, bead, anvil, branch, #)"
              aria-label="Filter PRs"
              className={`w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1 text-sm text-slate-100 placeholder:text-slate-500 focus:border-amber-400/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300 ${filter.trim() ? 'pr-7' : ''}`}
            />
            {filter.trim() && (
              <button
                type="button"
                onClick={() => {
                  setFilter('')
                  filterInputRef.current?.focus()
                }}
                aria-label="Clear filter"
                className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-0.5 text-slate-400 hover:text-slate-200 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
              >
                <X size={14} aria-hidden />
              </button>
            )}
          </div>
          <select
            value={sortKey}
            onChange={(e) => setSortKey(e.target.value as PRSortKey)}
            aria-label="Sort PRs"
            data-testid="prs-sort-select"
            className="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1 text-sm text-slate-100 focus:border-amber-400/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300 sm:w-auto"
          >
            {SORT_OPTIONS.map((opt) => (
              <option key={opt.value} value={opt.value}>
                {opt.label}
              </option>
            ))}
          </select>
          <button
            type="button"
            onClick={refresh}
            disabled={loading}
            className="inline-flex items-center gap-1.5 rounded-md border border-slate-700 bg-slate-800/60 px-2.5 py-1 text-xs text-slate-300 transition-colors hover:border-slate-600 hover:bg-slate-700 disabled:cursor-not-allowed disabled:opacity-60"
          >
            <RefreshCw size={12} aria-hidden className={loading ? 'animate-spin' : undefined} />
            Refresh
          </button>
        </div>
      </div>

      <main className="flex flex-col gap-4">
        {SECTION_ORDER.map((kind) => (
          <PRSectionContainer
            key={kind}
            kind={kind}
            items={visibleItems[kind]}
            loading={loading && visibleItems[kind].length === 0}
            error={error}
            onAction={requestAction}
            actingKey={actingKey}
            expanded={expandedBySection[kind]}
            onToggle={() => toggleSection(kind)}
            expandedFindingsKey={expandedFindingsKey}
            onToggleFindings={(key) =>
              setExpandedFindingsKey((prev) => (prev === key ? null : key))
            }
          />
        ))}
      </main>

      <footer className="text-center text-xs text-slate-500">
        Cached for {PRS_CACHE_TTL_MS / 1000}s · Hearth 2.0
      </footer>

      <ConfirmModal
        open={pending !== null}
        title={pending ? actionTitle(pending.kind) : ''}
        message={pending ? actionMessage(pending.kind, pending.pr) : ''}
        confirmLabel={pending ? actionConfirmLabel(pending.kind) : 'Confirm'}
        tone={pending && isDestructive(pending.kind) ? 'danger' : 'primary'}
        busy={busy}
        onConfirm={handleConfirm}
        onCancel={closeConfirm}
      />
    </div>
  )
}

interface PRSectionContainerProps {
  kind: PRSectionKind
  items: PRItem[]
  loading?: boolean
  error?: string | null
  onAction: (pr: PRItem, kind: PRActionKind) => void
  actingKey: string | null
  expanded: boolean
  onToggle: () => void
  // expandedFindingsKey / onToggleFindings drive the inline Assay findings
  // panel: the key identifies which row's panel is open (single-open), and
  // the toggle flips it. Both are threaded down to PRRow.
  expandedFindingsKey: string | null
  onToggleFindings: (key: string) => void
}

function PRSectionContainer({
  kind,
  items,
  loading,
  error,
  onAction,
  actingKey,
  expanded,
  onToggle,
  expandedFindingsKey,
  onToggleFindings,
}: PRSectionContainerProps) {
  return (
    <Pane
      title={PR_SECTION_TITLES[kind]}
      icon={<GitPullRequest size={16} className={SECTION_ICON_CLASSES[kind]} aria-hidden />}
      count={items.length}
      loading={loading}
      error={error ?? null}
      collapsible
      expanded={expanded}
      onToggle={onToggle}
    >
      <div className="px-4 pt-3 text-xs text-slate-400">{PR_SECTION_DESCRIPTIONS[kind]}</div>
      {items.length === 0 ? (
        <EmptyState message={PR_SECTION_EMPTY_MESSAGES[kind]} />
      ) : (
        <ul className="divide-y divide-slate-800" data-testid={`prs-${kind}`}>
          {items.map((pr) => {
            const rowKey = pr.id ?? `${pr.repo ?? pr.anvil}#${pr.number}`
            return (
              <PRRow
                key={rowKey}
                rowKey={String(rowKey)}
                pr={pr}
                section={kind}
                onAction={onAction}
                actingKey={actingKey}
                findingsExpanded={expandedFindingsKey === String(rowKey)}
                onToggleFindings={() => onToggleFindings(String(rowKey))}
              />
            )
          })}
        </ul>
      )}
    </Pane>
  )
}

interface PRRowProps {
  pr: PRItem
  // rowKey is the stable identity PRSectionContainer assigns the row; used to
  // wire the findings panel's aria-controls / id without re-deriving it.
  rowKey: string
  section: PRSectionKind
  onAction: (pr: PRItem, kind: PRActionKind) => void
  actingKey: string | null
  findingsExpanded: boolean
  onToggleFindings: () => void
}

function PRRow({
  pr,
  rowKey,
  section,
  onAction,
  actingKey,
  findingsExpanded,
  onToggleFindings,
}: PRRowProps) {
  const isExternal = pr.is_external || (pr.bead_id?.startsWith('ext-') ?? false)
  const isMerged = section === 'recently_merged'
  const conflicting = pr.is_conflicting === true
  const counters = (pr.ci_fix_count ?? 0) + (pr.review_fix_count ?? 0) + (pr.rebase_count ?? 0)
  // The Forge daemon emits CIPassing with `omitempty`, so a JSON-absent
  // ci_passing means "either not yet checked or failing". For forge PRs we
  // err on the side of always offering Fix CI; the user confirms before
  // anything is dispatched. External PRs hide the bellows-managed actions
  // entirely since they have no Forge worktree to operate against.
  const showBellowsActions = !isExternal && !isMerged

  const acting = (kind: PRActionKind) =>
    pr.id !== undefined && actingKey === `${pr.id}-${kind}`

  return (
    <li className="px-4 py-3">
      <div className="flex flex-col gap-2">
        <div>
          <p className="text-sm text-slate-100">{pr.title || '(no title)'}</p>
          <p className="mt-0.5 text-xs text-slate-500">
            {pr.anvil} · #{pr.number}
            {pr.branch ? ` · ${pr.branch}` : ''}
            {pr.bead_id && !pr.bead_id.startsWith('ext-') ? ` · ${pr.bead_id}` : ''}
            {pr.author ? ` · ${pr.author}` : ''}
          </p>
        </div>

        {(conflicting || pr.bellows_assigned) && (
          <div className="flex flex-wrap items-center gap-1.5">
            {conflicting && (
              <span className="inline-flex items-center gap-1 rounded border border-amber-500/25 bg-amber-500/15 px-1.5 py-0.5 text-xs text-amber-300">
                <AlertTriangle size={12} />
                conflict
              </span>
            )}
            {pr.bellows_assigned && (
              <span className="inline-flex items-center gap-1 rounded border border-indigo-500/25 bg-indigo-500/15 px-1.5 py-0.5 text-xs text-indigo-300">
                <Bell size={12} />
                bellows
              </span>
            )}
          </div>
        )}

        {!isMerged && (
          <div className="flex flex-wrap items-center gap-1.5">
            <ActionButton
              icon={<GitMerge size={13} aria-hidden />}
              label="Merge"
              tone="green"
              onClick={() => onAction(pr, 'merge')}
              busy={acting('merge')}
              disabled={pr.id === undefined}
            />
            <ActionButton
              icon={<ShieldCheck size={13} aria-hidden />}
              label="Approve"
              tone="purple"
              onClick={() => onAction(pr, 'approve')}
              busy={acting('approve')}
              disabled={pr.id === undefined}
            />
            {!pr.bellows_assigned && (
              <ActionButton
                icon={<Bell size={13} aria-hidden />}
                label="Bellows"
                tone="indigo"
                onClick={() => onAction(pr, 'bellows')}
                busy={acting('bellows')}
                disabled={pr.id === undefined}
              />
            )}
            {showBellowsActions && (
              <ActionButton
                icon={<Wrench size={13} aria-hidden />}
                label="Fix CI"
                tone="red"
                onClick={() => onAction(pr, 'fix-ci')}
                busy={acting('fix-ci')}
                disabled={pr.id === undefined || !pr.branch}
              />
            )}
            {showBellowsActions && conflicting && (
              <ActionButton
                icon={<RotateCcw size={13} aria-hidden />}
                label="Fix conflicts"
                tone="amber"
                onClick={() => onAction(pr, 'fix-conflicts')}
                busy={acting('fix-conflicts')}
                disabled={pr.id === undefined || !pr.branch}
              />
            )}
            {showBellowsActions && (
              <ActionButton
                icon={<MessageSquare size={13} aria-hidden />}
                label="Fix comments"
                tone="cyan"
                onClick={() => onAction(pr, 'fix-comments')}
                busy={acting('fix-comments')}
                disabled={pr.id === undefined || !pr.branch}
              />
            )}
            {showBellowsActions && counters > 0 && (
              <ActionButton
                icon={<RotateCcw size={13} aria-hidden />}
                label="Reset counters"
                tone="orange"
                onClick={() => onAction(pr, 'reset-counters')}
                busy={acting('reset-counters')}
                disabled={pr.id === undefined}
              />
            )}
            <ActionButton
              icon={<XCircle size={13} aria-hidden />}
              label="Close"
              tone="red"
              onClick={() => onAction(pr, 'close')}
              busy={acting('close')}
              disabled={pr.id === undefined}
            />
          </div>
        )}

        {/* Findings toggle is available for any PR with a state.db id —
            including merged PRs — so an operator can inspect what Assay
            flagged after the fact. */}
        {pr.id !== undefined && (
          <div className="flex flex-wrap items-center gap-1.5">
            <button
              type="button"
              onClick={onToggleFindings}
              aria-expanded={findingsExpanded}
              aria-controls={`pr-findings-${rowKey}`}
              data-testid={`pr-findings-toggle-${pr.id}`}
              className="inline-flex items-center gap-1 rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-xs font-medium text-slate-300 transition-colors hover:border-slate-600 hover:bg-slate-700 focus:outline-none focus-visible:ring-1 focus-visible:ring-cyan-300"
            >
              <ScanSearch size={13} aria-hidden />
              <span>{findingsExpanded ? 'Hide findings' : 'Findings'}</span>
            </button>
          </div>
        )}
      </div>

      {pr.id !== undefined && findingsExpanded && (
        <div id={`pr-findings-${rowKey}`} className="mt-3">
          <PRFindingsPanel pr={pr} onClose={onToggleFindings} />
        </div>
      )}
    </li>
  )
}

type ActionTone = 'green' | 'purple' | 'red' | 'amber' | 'indigo' | 'cyan' | 'orange'

const TONE_CLASSES: Record<ActionTone, string> = {
  green: 'border-emerald-600/30 bg-emerald-600/15 text-emerald-200 hover:bg-emerald-600/25',
  purple: 'border-purple-600/30 bg-purple-600/15 text-purple-200 hover:bg-purple-600/25',
  red: 'border-red-600/30 bg-red-600/15 text-red-200 hover:bg-red-600/25',
  amber: 'border-amber-600/30 bg-amber-600/15 text-amber-200 hover:bg-amber-600/25',
  indigo: 'border-indigo-600/30 bg-indigo-600/15 text-indigo-200 hover:bg-indigo-600/25',
  cyan: 'border-cyan-600/30 bg-cyan-600/15 text-cyan-200 hover:bg-cyan-600/25',
  orange: 'border-orange-600/30 bg-orange-600/15 text-orange-200 hover:bg-orange-600/25',
}

interface ActionButtonProps {
  icon: ReactNode
  label: string
  tone: ActionTone
  busy: boolean
  disabled?: boolean
  onClick: () => void
}

function ActionButton({ icon, label, tone, busy, disabled, onClick }: ActionButtonProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled || busy}
      aria-label={label}
      className={`inline-flex items-center gap-1 rounded-md border px-2 py-1 text-xs font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${TONE_CLASSES[tone]}`}
    >
      {icon}
      <span className="hidden sm:inline">{busy ? '…' : label}</span>
    </button>
  )
}

function actionTitle(kind: PRActionKind): string {
  switch (kind) {
    case 'merge':
      return 'Merge PR'
    case 'close':
      return 'Close PR'
    case 'approve':
      return 'Approve PR'
    case 'bellows':
      return 'Assign bellows'
    case 'fix-ci':
      return 'Fix CI'
    case 'fix-comments':
      return 'Fix review comments'
    case 'fix-conflicts':
      return 'Fix conflicts'
    case 'reset-counters':
      return 'Reset PR counters'
  }
}

function actionMessage(kind: PRActionKind, pr: PRItem): string {
  const ref = `#${pr.number}${pr.title ? ` (${pr.title})` : ''}`
  switch (kind) {
    case 'merge':
      return `Merge PR ${ref} using the configured strategy?`
    case 'close':
      return `Close PR ${ref}? This calls gh pr close on the daemon.`
    case 'approve':
      return `Approve PR ${ref} via gh pr review --approve?`
    case 'bellows':
      // Per Forge-i1g7, bellows adoption is scoped per-instance via the
      // <!-- forge-managed: <id> --> body marker. This action manually
      // attaches lifecycle management to the PR for THIS forge instance —
      // useful for taking over a sibling instance's Forge-created PR.
      // NOTE: this has no durable effect on externally-created (ext-*) PRs;
      // the daemon's reconcile loop will clear the assignment on the next cycle.
      return `Assign this Forge instance to manage PR ${ref}? This works for Forge-created PRs (e.g. from a sibling instance). For externally-created PRs the daemon reconcile will revert the assignment on the next cycle.`
    case 'fix-ci':
      return `Spawn a quench worker to fix CI on PR ${ref}?`
    case 'fix-comments':
      return `Spawn a burnish worker to address review comments on PR ${ref}?`
    case 'fix-conflicts':
      return `Rebase PR ${ref} onto its base branch to resolve conflicts?`
    case 'reset-counters':
      return `Reset CI/review/rebase fix counters on PR ${ref} and re-open it for bellows?`
  }
}

function actionConfirmLabel(kind: PRActionKind): string {
  switch (kind) {
    case 'merge':
      return 'Merge'
    case 'close':
      return 'Close'
    case 'approve':
      return 'Approve'
    case 'bellows':
      return 'Assign'
    case 'fix-ci':
      return 'Fix CI'
    case 'fix-comments':
      return 'Fix comments'
    case 'fix-conflicts':
      return 'Rebase'
    case 'reset-counters':
      return 'Reset'
  }
}

function actionSuccessMessage(kind: PRActionKind, pr: PRItem): string {
  const ref = `#${pr.number}`
  switch (kind) {
    case 'merge':
      return `Merge requested for ${ref}`
    case 'close':
      return `Closed ${ref}`
    case 'approve':
      return `Approved ${ref}`
    case 'bellows':
      return `Bellows assigned to ${ref}`
    case 'fix-ci':
      return `CI fix started for ${ref}`
    case 'fix-comments':
      return `Review fix started for ${ref}`
    case 'fix-conflicts':
      return `Rebase started for ${ref}`
    case 'reset-counters':
      return `Counters reset on ${ref}`
  }
}

function isDestructive(kind: PRActionKind): boolean {
  return kind === 'close'
}

function formatRelative(timestamp: number): string {
  const secs = Math.max(0, Math.round((Date.now() - timestamp) / 1000))
  if (secs < 5) return 'just now'
  if (secs < 60) return `${secs}s ago`
  const mins = Math.round(secs / 60)
  if (mins < 60) return `${mins}m ago`
  const hours = Math.round(mins / 60)
  return `${hours}h ago`
}
