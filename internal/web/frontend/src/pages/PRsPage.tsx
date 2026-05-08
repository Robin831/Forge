import { GitPullRequest, RefreshCw } from 'lucide-react'
import { useApiPoll } from '../hooks/useApiPoll'
import type { PRItem, StatusResponse } from '../api'
import AppHeader from '../components/AppHeader'
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

// PRsPage is the shell for the Hearth 2.0 /prs tab. The data hooks
// (Forge-9ye8) populate each section from state.db; per-PR action buttons
// (Forge-x7dy) plug into the row renderer below.
export default function PRsPage() {
  const status = useApiPoll<StatusResponse>('/api/status', STATUS_POLL_INTERVAL_MS)

  const { forge_prs, external_prs, recently_merged, loading, error, refresh, fetchedAt } =
    usePRsData()
  const items: Record<PRSectionKind, PRItem[]> = {
    forge_prs,
    external_prs,
    recently_merged,
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
          />
        ))}
      </main>

      <footer className="text-center text-xs text-slate-500">
        Cached for {PRS_CACHE_TTL_MS / 1000}s · Hearth 2.0
      </footer>
    </div>
  )
}

interface PRSectionContainerProps {
  kind: PRSectionKind
  items: PRItem[]
  loading?: boolean
  error?: string | null
}

function PRSectionContainer({ kind, items, loading, error }: PRSectionContainerProps) {
  return (
    <Pane
      title={PR_SECTION_TITLES[kind]}
      icon={<GitPullRequest size={16} className={SECTION_ICON_CLASSES[kind]} aria-hidden />}
      count={items.length}
      loading={loading}
      error={error ?? null}
    >
      <div className="px-4 pt-3 text-xs text-slate-400">
        {PR_SECTION_DESCRIPTIONS[kind]}
      </div>
      {items.length === 0 ? (
        <EmptyState message={PR_SECTION_EMPTY_MESSAGES[kind]} />
      ) : (
        <ul className="divide-y divide-slate-800" data-testid={`prs-${kind}`}>
          {items.map((pr) => (
            <li key={pr.id ?? `${pr.repo ?? pr.anvil}#${pr.number}`} className="px-4 py-3">
              <p className="text-sm text-slate-100">{pr.title || '(no title)'}</p>
              <p className="mt-0.5 text-xs text-slate-500">
                {pr.anvil} · #{pr.number}
                {pr.branch ? ` · ${pr.branch}` : ''}
                {pr.bead_id && !pr.bead_id.startsWith('ext-') ? ` · ${pr.bead_id}` : ''}
              </p>
            </li>
          ))}
        </ul>
      )}
    </Pane>
  )
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
