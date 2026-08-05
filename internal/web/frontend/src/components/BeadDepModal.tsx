import { useEffect, useRef, useState } from 'react'
import { ExternalLink, Loader2 } from 'lucide-react'
import { Link } from 'react-router'
import { apiGet, ApiError, type BeadBrief, type BeadDetailResponse } from '../api'
import { useAuth } from '../auth'
import { priorityClasses, priorityLabel } from '../lib/format'

const STATUS_BADGE: Record<string, string> = {
  done: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  failed: 'bg-red-500/20 text-red-300 border-red-500/40',
  timeout: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  running: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  pending: 'bg-slate-700/60 text-slate-200 border-slate-600/60',
  open: 'bg-sky-500/20 text-sky-300 border-sky-500/40',
  in_progress: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  closed: 'bg-slate-700/60 text-slate-300 border-slate-600/60',
}

function badgeClass(s: string): string {
  return STATUS_BADGE[s] ?? 'bg-slate-800 text-slate-300 border-slate-700'
}

const DESCRIPTION_LIMIT = 280

function descriptionExcerpt(text: string): string {
  const trimmed = text.trim()
  if (trimmed.length <= DESCRIPTION_LIMIT) return trimmed
  return `${trimmed.slice(0, DESCRIPTION_LIMIT).trimEnd()}…`
}

export interface BeadDepModalProps {
  open: boolean
  initialBrief: BeadBrief | null
  onClose: () => void
}

// BeadDepModal pops up a graph-walking view for a single bead. The user can
// click any dep entry to drill into that bead without leaving the page, or hit
// "Open full page" to navigate to the dedicated /bead route. The modal
// implements a focus trap cycling Tab through all focusable elements within it,
// ESC-to-dismiss, and click-outside dismiss; the shape mirrors ConfirmModal so
// it visually fits with the rest of the dashboard.
export default function BeadDepModal({ open, initialBrief, onClose }: BeadDepModalProps) {
  const [brief, setBrief] = useState<BeadBrief | null>(initialBrief)
  const [data, setData] = useState<BeadDetailResponse | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const closeRef = useRef<HTMLButtonElement | null>(null)
  const modalRef = useRef<HTMLDivElement | null>(null)
  const { logout } = useAuth()

  // Reset to the initial brief whenever the parent opens the modal with a new
  // target. Clears stale data synchronously to avoid showing wrong content
  // before the fetch effect fires. Clearing on close avoids leaking state.
  useEffect(() => {
    if (open) {
      setBrief(initialBrief)
      setData(null)
      setError(null)
    } else {
      setBrief(null)
      setData(null)
      setError(null)
    }
  }, [open, initialBrief])

  // Clears stale data synchronously then navigates to a dep bead.
  const drillInto = (b: BeadBrief) => {
    setData(null)
    setError(null)
    setBrief(b)
  }

  useEffect(() => {
    if (!open || !brief) return
    const controller = new AbortController()
    setLoading(true)
    setError(null)
    setData(null)
    const qs = brief.anvil
      ? `?anvil=${encodeURIComponent(brief.anvil)}`
      : ''
    apiGet<BeadDetailResponse>(
      `/api/bead/${encodeURIComponent(brief.bead_id)}${qs}`,
      controller.signal,
    )
      .then((d) => {
        setData(d)
        setLoading(false)
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return
        if (err instanceof ApiError && err.status === 401) {
          void logout()
          return
        }
        const msg =
          err instanceof ApiError ? err.message : (err as Error)?.message || 'failed to load'
        setError(msg)
        setLoading(false)
      })
    return () => controller.abort()
  }, [open, brief, logout])

  useEffect(() => {
    if (!open) return
    const t = window.setTimeout(() => {
      closeRef.current?.focus()
    }, 10)
    return () => window.clearTimeout(t)
  }, [open])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose()
        return
      }
      if (e.key === 'Tab' && modalRef.current) {
        const focusable = modalRef.current.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
        )
        const elements = Array.from(focusable)
        if (elements.length === 0) return
        const first = elements[0]
        const last = elements[elements.length - 1]
        if (e.shiftKey) {
          if (document.activeElement === first) {
            e.preventDefault()
            last.focus()
          }
        } else {
          if (document.activeElement === last) {
            e.preventDefault()
            first.focus()
          }
        }
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open || !brief) return null

  const title = data?.queue?.title || brief.title || brief.bead_id
  const description = data?.queue?.description ?? ''
  const status = data?.queue?.status || brief.status
  const priority = data?.queue?.priority ?? brief.priority
  const anvil = data?.anvil || brief.anvil
  const blocks = data?.blocks ?? []
  const blockedBy = data?.blocked_by ?? []
  const fullPagePath = anvil
    ? `/bead/${encodeURIComponent(brief.bead_id)}?anvil=${encodeURIComponent(anvil)}`
    : `/bead/${encodeURIComponent(brief.bead_id)}`

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="bead-dep-modal-title"
      className="fixed inset-0 z-50 flex items-center justify-center bg-slate-950/70 p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
      ref={modalRef}
    >
      <div className="flex max-h-[85vh] w-full max-w-2xl flex-col rounded-xl border border-slate-800 bg-slate-900 shadow-xl">
        <header className="flex items-start gap-3 border-b border-slate-800 px-5 py-4">
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-baseline gap-2">
              <h2
                id="bead-dep-modal-title"
                className="truncate text-base font-semibold text-slate-100"
              >
                {title}
              </h2>
              <span
                className={`shrink-0 rounded-md border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${priorityClasses(priority)}`}
              >
                {priorityLabel(priority)}
              </span>
              <span
                className={`shrink-0 rounded-md border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${badgeClass(status)}`}
              >
                {status}
              </span>
            </div>
            <p className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-slate-500">
              <span className="font-mono text-slate-400">{brief.bead_id}</span>
              {anvil && (
                <>
                  <span aria-hidden>·</span>
                  <span>{anvil}</span>
                </>
              )}
            </p>
          </div>
          <Link
            to={fullPagePath}
            onClick={onClose}
            className="inline-flex shrink-0 items-center gap-1.5 rounded-md border border-slate-700 bg-slate-800 px-2.5 py-1 text-xs text-slate-200 hover:border-amber-400/40 hover:text-amber-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300"
          >
            <ExternalLink size={12} aria-hidden /> Open full page
          </Link>
        </header>

        <div className="flex-1 overflow-y-auto px-5 py-4">
          {loading && (
            <div className="flex items-center gap-2 text-xs text-slate-500">
              <Loader2 size={14} className="animate-spin" aria-label="Loading" />
              Loading…
            </div>
          )}
          {error && (
            <p className="rounded-md border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-200">
              {error}
            </p>
          )}
          {description && (
            <p className="mt-1 whitespace-pre-wrap text-sm text-slate-300">
              {descriptionExcerpt(description)}
            </p>
          )}
          {!loading && !error && !description && (
            <p className="text-sm text-slate-500">No description.</p>
          )}

          <div className="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-2">
            <DepColumn
              title="Blocks"
              items={blocks}
              onSelect={drillInto}
              emptyMessage="Nothing."
            />
            <DepColumn
              title="Blocked by"
              items={blockedBy}
              onSelect={drillInto}
              emptyMessage="Nothing."
            />
          </div>
        </div>

        <footer className="flex justify-end border-t border-slate-800 px-5 py-3">
          <button
            ref={closeRef}
            type="button"
            onClick={onClose}
            className="rounded-md border border-slate-700 bg-slate-800 px-3 py-1.5 text-sm text-slate-200 hover:bg-slate-700 focus:outline-none focus-visible:ring-2 focus-visible:ring-slate-400"
          >
            Close
          </button>
        </footer>
      </div>
    </div>
  )
}

interface DepColumnProps {
  title: string
  items: BeadBrief[]
  onSelect: (b: BeadBrief) => void
  emptyMessage: string
}

function DepColumn({ title, items, onSelect, emptyMessage }: DepColumnProps) {
  return (
    <div>
      <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-400">
        {title}
        <span className="ml-1 text-slate-600">({items.length})</span>
      </h3>
      {items.length === 0 ? (
        <p className="mt-2 text-xs text-slate-500">{emptyMessage}</p>
      ) : (
        <ul className="mt-2 space-y-1.5">
          {items.map((b) => (
            <li key={`${b.bead_id}-${b.anvil ?? ''}`}>
              <button
                type="button"
                onClick={() => onSelect(b)}
                className="block w-full rounded-md border border-slate-800 bg-slate-950/40 px-2.5 py-2 text-left text-xs hover:border-amber-400/40 hover:bg-slate-800/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300"
              >
                <div className="flex flex-wrap items-baseline gap-1.5">
                  <span className="font-mono text-slate-300">{b.bead_id}</span>
                  <span
                    className={`shrink-0 rounded-md border px-1.5 py-0.5 text-[9px] font-semibold uppercase tracking-wide ${priorityClasses(b.priority)}`}
                  >
                    {priorityLabel(b.priority)}
                  </span>
                  <span
                    className={`shrink-0 rounded-md border px-1.5 py-0.5 text-[9px] font-semibold uppercase tracking-wide ${badgeClass(b.status)}`}
                  >
                    {b.status}
                  </span>
                </div>
                <p className="mt-1 truncate text-slate-200">{b.title}</p>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
