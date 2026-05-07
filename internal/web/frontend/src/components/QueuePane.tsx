import { List } from 'lucide-react'
import type { QueueItem } from '../api'
import { priorityClasses, priorityLabel } from '../lib/format'
import Pane, { EmptyState } from './Pane'

interface QueuePaneProps {
  items: QueueItem[]
  loading: boolean
  error: string | null
}

export default function QueuePane({ items, loading, error }: QueuePaneProps) {
  return (
    <Pane
      title="Queue"
      icon={<List size={16} className="text-cyan-400" aria-hidden />}
      count={items.length}
      loading={loading}
      error={error}
    >
      {items.length === 0 && !loading ? (
        <EmptyState message="No beads in queue." />
      ) : (
        <ul className="divide-y divide-slate-800">
          {items.map((item) => (
            <li key={`${item.anvil}:${item.bead_id}`} className="px-4 py-3">
              <div className="flex items-start gap-2">
                <span
                  className={`shrink-0 rounded-md border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${priorityClasses(item.priority)}`}
                >
                  {priorityLabel(item.priority)}
                </span>
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium text-slate-100">
                    {item.title || item.bead_id}
                  </p>
                  <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
                    <span className="font-mono text-slate-400">{item.bead_id}</span>
                    <span aria-hidden>·</span>
                    <span>{item.anvil}</span>
                    {item.section && (
                      <>
                        <span aria-hidden>·</span>
                        <span className="capitalize">{item.section}</span>
                      </>
                    )}
                  </p>
                  {item.labels.length > 0 && (
                    <div className="mt-1.5 flex flex-wrap gap-1">
                      {item.labels.slice(0, 4).map((label) => (
                        <span
                          key={label}
                          className="rounded-full bg-slate-800 px-2 py-0.5 text-[10px] text-slate-300"
                        >
                          {label}
                        </span>
                      ))}
                    </div>
                  )}
                </div>
              </div>
            </li>
          ))}
        </ul>
      )}
    </Pane>
  )
}
