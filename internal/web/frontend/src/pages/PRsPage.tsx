import { useState, type ReactNode } from 'react'
import {
  AlertTriangle,
  Bell,
  GitMerge,
  GitPullRequest,
  MessageSquare,
  RefreshCw,
  RotateCcw,
  ShieldCheck,
  Wrench,
  XCircle,
} from 'lucide-react'
import { useApiPoll } from '../hooks/useApiPoll'
import { useAction } from '../hooks/useAction'
import { useToast } from '../hooks/useToast'
import { prActions, type PRActionKind, type PRItem, type StatusResponse } from '../api'
import AppHeader from '../components/AppHeader'
import ConfirmModal from '../components/ConfirmModal'
import Pane, { EmptyState } from '../components/Pane'
import {
  PR_SECTION_DESCRIPTIONS,
  PR_SECTION_EMPTY_MESSAGES,
  PR_SECTION_TITLES,
  type PRSectionKind,
} from './prsTypes'
import { PRS_CACHE_TTL_MS, usePRsData } from './usePRsData'

const STATUS_POLL_INTERVAL_MS = 10_000

const SECTION_ORDER: PRSectionKind[] = ['forge_prs', 'external_prs', 'recently_merged']

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
  const items: Record<PRSectionKind, PRItem[]> = {
    forge_prs,
    external_prs,
    recently_merged,
  }

  const { run, busy } = useAction()
  const toast = useToast()
  // actingKey tracks which row+action is mid-flight so we can disable just
  // that button rather than every button on the page (the global `busy`
  // flag from useAction would do the latter).
  const [actingKey, setActingKey] = useState<string | null>(null)
  const [pending, setPending] = useState<PendingAction | null>(null)

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
    <div className="mx-auto flex min-h-full max-w-7xl flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <div className="flex items-center justify-between gap-3">
        <p className="text-xs text-slate-500">
          {fetchedAt
            ? `Last updated ${formatRelative(fetchedAt)}`
            : loading
              ? 'Loading…'
              : 'Not yet loaded'}
        </p>
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

      <main className="flex flex-col gap-4">
        {SECTION_ORDER.map((kind) => (
          <PRSectionContainer
            key={kind}
            kind={kind}
            items={items[kind]}
            loading={loading && items[kind].length === 0}
            error={error}
            onAction={requestAction}
            actingKey={actingKey}
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
}

function PRSectionContainer({
  kind,
  items,
  loading,
  error,
  onAction,
  actingKey,
}: PRSectionContainerProps) {
  return (
    <Pane
      title={PR_SECTION_TITLES[kind]}
      icon={<GitPullRequest size={16} className={SECTION_ICON_CLASSES[kind]} aria-hidden />}
      count={items.length}
      loading={loading}
      error={error ?? null}
    >
      <div className="px-4 pt-3 text-xs text-slate-400">{PR_SECTION_DESCRIPTIONS[kind]}</div>
      {items.length === 0 ? (
        <EmptyState message={PR_SECTION_EMPTY_MESSAGES[kind]} />
      ) : (
        <ul className="divide-y divide-slate-800" data-testid={`prs-${kind}`}>
          {items.map((pr) => (
            <PRRow
              key={pr.id ?? `${pr.repo ?? pr.anvil}#${pr.number}`}
              pr={pr}
              section={kind}
              onAction={onAction}
              actingKey={actingKey}
            />
          ))}
        </ul>
      )}
    </Pane>
  )
}

interface PRRowProps {
  pr: PRItem
  section: PRSectionKind
  onAction: (pr: PRItem, kind: PRActionKind) => void
  actingKey: string | null
}

function PRRow({ pr, section, onAction, actingKey }: PRRowProps) {
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
      </div>
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
