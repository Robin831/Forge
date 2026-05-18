import { ChevronDown, ChevronRight, Loader2 } from 'lucide-react'
import type { ReactNode, RefObject } from 'react'

interface PaneProps {
  title: string
  icon?: ReactNode
  count?: number
  loading?: boolean
  error?: string | null
  // headerExtra renders below the title row, inside the same header chrome.
  // Useful for inline controls such as a filter input that should live with
  // the pane title rather than the body.
  headerExtra?: ReactNode
  // bodyRef attaches to the scrollable body element so callers can read/write
  // scrollTop (used by QueuePane to persist scroll position across navigation).
  bodyRef?: RefObject<HTMLDivElement | null>
  // collapsible turns the title row into a toggle: a chevron is prepended and
  // the entire title row becomes a button. When `expanded` is false the body
  // (including headerExtra) is hidden so the only visible chrome is the title
  // bar and count badge. The PR pane uses this to let users hide entire
  // sections (forge / external / recently-merged) without losing their place.
  collapsible?: boolean
  expanded?: boolean
  onToggle?: () => void
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
  headerExtra,
  bodyRef,
  collapsible,
  expanded = true,
  onToggle,
  children,
}: PaneProps) {
  const isCollapsed = collapsible && !expanded
  // When `collapsible` the title row is rendered inside a <button>, so we
  // use a <span> there — <h2> inside <button> is invalid HTML per the spec.
  const TitleEl = collapsible ? 'span' : 'h2'
  const titleRow = (
    <div className="flex items-center gap-2">
      {collapsible &&
        (expanded ? (
          <ChevronDown size={14} className="text-slate-400" aria-hidden />
        ) : (
          <ChevronRight size={14} className="text-slate-400" aria-hidden />
        ))}
      {icon}
      <TitleEl className="text-sm font-semibold text-slate-200">{title}</TitleEl>
      {typeof count === 'number' && (
        <span className="ml-auto rounded-full bg-slate-800 px-2 py-0.5 text-xs text-slate-300">
          {count}
        </span>
      )}
      {loading && (
        <Loader2
          size={14}
          className={`${typeof count === 'number' ? '' : 'ml-auto '}animate-spin text-slate-500`}
          aria-label="Loading"
        />
      )}
    </div>
  )
  return (
    <section
      aria-label={title}
      className={`flex flex-col rounded-xl border border-slate-800 bg-slate-900/60 ${
        isCollapsed ? '' : 'min-h-[20rem]'
      }`}
    >
      <header
        className={`flex flex-col gap-2 px-4 py-3 ${isCollapsed ? '' : 'border-b border-slate-800'}`}
      >
        {collapsible ? (
          <button
            type="button"
            onClick={onToggle}
            aria-expanded={!!expanded}
            className="-mx-4 -my-3 flex flex-col gap-2 rounded-t-xl px-4 py-3 text-left hover:bg-slate-800/40 focus:bg-slate-800/40 focus:outline-none focus-visible:ring-1 focus-visible:ring-amber-300"
          >
            {titleRow}
          </button>
        ) : (
          titleRow
        )}
        {!isCollapsed && headerExtra}
      </header>

      {!isCollapsed && (
        <div ref={bodyRef} className="flex-1 overflow-y-auto" role="region">
          {error ? (
            <div className="m-4 rounded-md border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-200">
              {error}
            </div>
          ) : (
            children
          )}
        </div>
      )}
    </section>
  )
}

export function EmptyState({ message }: { message: string }) {
  return (
    <p className="px-4 py-8 text-center text-sm text-slate-500">{message}</p>
  )
}
