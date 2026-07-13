import { useEffect } from 'react'
import { Pause, Play } from 'lucide-react'
import { actions } from '../api'
import { useAction } from '../hooks/useAction'

interface DispatchToggleProps {
  // paused reflects the daemon's current dispatch state, sourced from
  // GET /api/status (dispatch_paused). The component is otherwise stateless —
  // the parent re-renders it from the next status poll after a toggle.
  paused: boolean
  // pausedSince is the RFC3339 timestamp of when the manual pause began
  // (status.paused_since). Undefined when not manually paused or unknown; when
  // present it is rendered in the banner as "paused since <time>".
  pausedSince?: string
}

// formatPausedSince renders an RFC3339 timestamp as a short human-readable
// local time, falling back to the raw value if it cannot be parsed.
function formatPausedSince(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

// DispatchToggle renders the daemon-wide pause/resume control plus a clear
// visual indicator when auto-dispatch is paused. Pausing stops NEW workers
// from being dispatched while leaving running workers untouched (they finish
// normally) — useful for draining the active set to zero before restarting the
// daemon. The keyboard shortcut "p" toggles the state from anywhere on the
// dashboard (ignored while typing in an input/textarea).
export default function DispatchToggle({ paused, pausedSince }: DispatchToggleProps) {
  const { run, busy } = useAction()

  const toggle = () => {
    if (busy) return
    if (paused) {
      void run(() => actions.resumeDispatch(), {
        successMessage: 'Dispatch resumed — new workers will be dispatched',
      })
    } else {
      void run(() => actions.pauseDispatch(), {
        successMessage: 'Dispatch paused — running workers continue, no new dispatch',
      })
    }
  }

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'p' && e.key !== 'P') return
      if (e.metaKey || e.ctrlKey || e.altKey) return
      const target = e.target as HTMLElement | null
      const tag = target?.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || target?.isContentEditable) return
      e.preventDefault()
      toggle()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
    // toggle closes over `paused` and `busy`; re-bind when they change so the
    // shortcut always reflects the current state.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [paused, busy])

  return (
    <div className="flex items-center gap-2">
      {paused && (
        <span
          className="inline-flex items-center gap-1.5 rounded-full border border-amber-500/40 bg-amber-500/10 px-2.5 py-1 text-xs font-medium text-amber-200"
          aria-live="polite"
        >
          <Pause size={10} aria-hidden />
          {pausedSince ? `dispatch paused since ${formatPausedSince(pausedSince)}` : 'dispatch paused'}
        </span>
      )}
      <button
        type="button"
        onClick={toggle}
        disabled={busy}
        aria-pressed={paused}
        title={
          paused
            ? 'Resume auto-dispatch (shortcut: p)'
            : 'Pause auto-dispatch — running workers keep going (shortcut: p)'
        }
        className={`inline-flex items-center gap-1.5 rounded-lg border px-3 py-1.5 text-sm font-medium transition-colors disabled:opacity-50 ${
          paused
            ? 'border-emerald-500/40 bg-emerald-500/10 text-emerald-200 hover:bg-emerald-500/20'
            : 'border-slate-700 bg-slate-800 text-slate-300 hover:border-amber-400/40 hover:text-amber-200'
        }`}
      >
        {paused ? <Play size={14} aria-hidden /> : <Pause size={14} aria-hidden />}
        <span>{paused ? 'Resume' : 'Pause'}</span>
      </button>
    </div>
  )
}
