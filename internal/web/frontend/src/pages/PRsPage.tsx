import { GitPullRequest } from 'lucide-react'
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

const POLL_INTERVAL_MS = 10000

const SECTION_ORDER: PRSectionKind[] = ['forge_prs', 'external_prs', 'recently_merged']

const SECTION_ICON_CLASSES: Record<PRSectionKind, string> = {
  forge_prs: 'text-amber-400',
  external_prs: 'text-sky-400',
  recently_merged: 'text-emerald-400',
}

// PRsPage is the shell for the Hearth 2.0 /prs tab. The data-fetching layer
// (Forge-9ye8) and per-PR actions (Forge-x7dy) plug into the section bodies
// below — for now each section renders its empty state so the navigation,
// layout, and types are in place for the follow-up sub-tasks.
export default function PRsPage() {
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)

  // Sub-task Forge-9ye8 will replace these placeholders with a real
  // useApiPoll<PRsResponse>('/api/prs/all', ...) call.
  const items: Record<PRSectionKind, PRItem[]> = {
    forge_prs: [],
    external_prs: [],
    recently_merged: [],
  }

  return (
    <div className="mx-auto flex min-h-full max-w-7xl flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <main className="flex flex-col gap-4">
        {SECTION_ORDER.map((kind) => (
          <PRSectionContainer key={kind} kind={kind} items={items[kind]} />
        ))}
      </main>

      <footer className="text-center text-xs text-slate-500">
        Polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
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
              </p>
            </li>
          ))}
        </ul>
      )}
    </Pane>
  )
}
