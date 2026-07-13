import { useState, type FormEvent, type KeyboardEvent } from 'react'
import { Send } from 'lucide-react'
import { actions } from '../api'
import { useAction } from '../hooks/useAction'

interface SteerComposerProps {
  beadID: string
  // disabledReason is null when the bead is steerable; otherwise it is a
  // human-readable explanation (rendered as a tooltip + helper text) for why
  // the input is disabled. Derive it from steerDisabledReason() so the UI
  // mirrors the daemon's steer validation.
  disabledReason: string | null
  // compact renders a tighter layout (no surrounding card chrome) for use
  // inside the WorkerLogModal footer.
  compact?: boolean
  // paused marks a steerable-but-parked worker. A paused pipeline only consumes
  // a message on resume, so the composer delivers it as a resume-with-message
  // (POST /api/bead/{id}/resume) instead of a plain steer, and phrases the
  // affordance truthfully ("applies on resume"). Derive it from
  // steerIsResumeDelivery() so the routing mirrors the daemon acceptance matrix.
  paused?: boolean
}

// SteerComposer renders a single-line "steer" message input that delivers a
// human course-correction to a bead's in-flight Smith pipeline via
// POST /api/bead/{id}/steer. It is shared between the WorkerLogModal (for the
// active worker) and the BeadDetailPage (when the bead is active). When
// disabledReason is set the input is disabled and the reason is surfaced as a
// tooltip and helper line so the operator understands why steering is
// unavailable (no active pipeline or a non-Claude session).
//
// A paused worker is steerable, but its message applies on resume: with
// `paused` set the composer submits via the resume endpoint (resume-with-
// message) and relabels itself accordingly, so the operator always knows
// whether the message steers a live spawn or continues a parked one.
export default function SteerComposer({ beadID, disabledReason, compact, paused }: SteerComposerProps) {
  const [message, setMessage] = useState('')
  const { run, busy } = useAction()
  const disabled = disabledReason !== null

  // Paused workers route through resume-with-message; everyone else steers.
  const pausedHint = paused
    ? 'Worker is paused — your message will apply on resume.'
    : null

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault()
    const trimmed = message.trim()
    if (!trimmed || busy || disabled) return
    const ok = paused
      ? await run(() => actions.resume(beadID, trimmed), {
          successMessage: 'Resume requested with your message',
        })
      : await run(() => actions.steer(beadID, trimmed), {
          successMessage: 'Steer message delivered',
        })
    if (ok) setMessage('')
  }

  // Esc clears any draft and blurs the input. stopPropagation keeps the key
  // from bubbling to the WorkerLogModal's window-level Escape listener (which
  // would otherwise close the modal out from under the operator mid-edit) — we
  // deliberately scope the shortcut to the composer rather than hijacking the
  // global one.
  const handleKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Escape') {
      e.stopPropagation()
      setMessage('')
      e.currentTarget.blur()
    }
  }

  const canSubmit = message.trim().length > 0 && !busy && !disabled
  // Title/tooltip: the disabled reason wins; otherwise a paused worker explains
  // the resume-on-message affordance so the tooltip is never the misleading
  // "no active pipeline" for a parked-but-steerable worker.
  const title = disabledReason ?? pausedHint ?? undefined
  const submitLabel = paused ? 'Resume' : 'Steer'

  return (
    <form
      onSubmit={handleSubmit}
      className={
        compact
          ? 'border-t border-slate-800 bg-slate-950/40 px-4 py-3'
          : 'rounded-xl border border-slate-800 bg-slate-900/60 p-4'
      }
      aria-label="Steer worker"
      title={title}
    >
      {!compact && (
        <h3 className="mb-2 flex items-center gap-1.5 text-sm font-semibold text-slate-200">
          <Send size={14} className="text-amber-400" aria-hidden /> Steer
        </h3>
      )}
      <div className="flex items-center gap-2">
        <label htmlFor={`steer-composer-${beadID}`} className="sr-only">
          Steer message
        </label>
        <input
          id={`steer-composer-${beadID}`}
          type="text"
          value={message}
          onChange={(e) => setMessage(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder={
            disabled
              ? 'Steering unavailable'
              : paused
                ? 'Send a message to apply on resume…'
                : 'Send a course-correction to the worker…'
          }
          disabled={disabled || busy}
          className="min-w-0 flex-1 rounded-md border border-slate-700 bg-slate-900 px-2.5 py-1.5 text-sm text-slate-200 placeholder:text-slate-500 focus:border-amber-400/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 disabled:opacity-60"
        />
        <button
          type="submit"
          disabled={!canSubmit}
          title={title}
          className="inline-flex shrink-0 items-center gap-1.5 rounded-md border border-amber-400/40 bg-amber-500/10 px-3 py-1.5 text-xs text-amber-200 hover:bg-amber-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 disabled:opacity-50"
        >
          <Send size={12} aria-hidden />
          {busy ? 'Sending…' : submitLabel}
        </button>
      </div>
      {disabledReason ? (
        <p className="mt-1.5 text-xs text-slate-500">{disabledReason}</p>
      ) : (
        pausedHint && <p className="mt-1.5 text-xs text-amber-300/80">{pausedHint}</p>
      )}
    </form>
  )
}
