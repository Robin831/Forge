import { Loader2 } from 'lucide-react'
import type { ReactNode } from 'react'

interface PaneProps {
  title: string
  icon?: ReactNode
  count?: number
  loading?: boolean
  error?: string | null
  children: ReactNode
}

// Pane is the shared chrome for the three dashboard panes (queue, workers,
// events). It owns the title bar, count badge, error/loading rendering, and
// the scrollable body so the children only need to render their list rows.
export default function Pane({
  title,
  icon,
  count,
  loading,
  error,
  children,
}: PaneProps) {
  return (
    <section
      aria-label={title}
      className="flex min-h-[20rem] flex-col rounded-xl border border-slate-800 bg-slate-900/60"
    >
      <header className="flex items-center gap-2 border-b border-slate-800 px-4 py-3">
        {icon}
        <h2 className="text-sm font-semibold text-slate-200">{title}</h2>
        {typeof count === 'number' && (
          <span className="ml-auto rounded-full bg-slate-800 px-2 py-0.5 text-xs text-slate-300">
            {count}
          </span>
        )}
        {loading && (
          <Loader2
            size={14}
            className="ml-auto animate-spin text-slate-500"
            aria-label="Loading"
          />
        )}
      </header>

      <div className="flex-1 overflow-y-auto" role="region">
        {error ? (
          <div className="m-4 rounded-md border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-200">
            {error}
          </div>
        ) : (
          children
        )}
      </div>
    </section>
  )
}

export function EmptyState({ message }: { message: string }) {
  return (
    <p className="px-4 py-8 text-center text-sm text-slate-500">{message}</p>
  )
}
