import { useState, type FormEvent } from 'react'
import { PlayCircle } from 'lucide-react'
import { actions } from '../api'
import { useAction } from '../hooks/useAction'

interface ResumeWithMessageComposerProps {
  // beadID is the needs-attention bead to resume. The action is keyed purely by
  // bead id; the daemon resolves the resumable worker row (branch + session).
  beadID: string
  // branch is the surviving forge/<bead> branch the worktree is recreated from.
  // Rendered inline so the operator knows which branch the resume runs against.
  branch?: string
  // onResumed, when provided, fires after a successful dispatch — the panel uses
  // it to close/refresh so the resolved bead drops off on the next poll.
  onResumed?: () => void
}

// ResumeWithMessageComposer renders the "Resume with message" affordance on a
// needs-attention row: a multi-line operator message plus a submit button that
// POSTs to /api/bead/{id}/resume-with-message. It mirrors SteerComposer, but the
// target bead has no live pipeline — its worktree was torn down while the
// forge/<bead> branch survived — so the daemon recreates the worktree from that
// branch and resumes the recorded Claude session (falling back to a fresh
// session seeded with the message when the transcript or branch is gone).
//
// The parent gates rendering on resumeWithMessageEligible (escalation type +
// branch existence), so this component assumes the bead is eligible and only
// enforces a non-empty message before enabling submit.
export default function ResumeWithMessageComposer({
  beadID,
  branch,
  onResumed,
}: ResumeWithMessageComposerProps) {
  const [message, setMessage] = useState('')
  const { run, busy } = useAction()
  const messageId = `resume-with-message-${beadID}`

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault()
    const trimmed = message.trim()
    if (!trimmed || busy) return
    const ok = await run(() => actions.resumeWithMessage(beadID, trimmed), {
      successMessage: 'Resume-with-message dispatched',
      onSuccess: onResumed,
    })
    if (ok) setMessage('')
  }

  const canSubmit = message.trim().length > 0 && !busy

  return (
    <form onSubmit={handleSubmit}>
      <label
        htmlFor={messageId}
        className="text-xs font-semibold uppercase tracking-wide text-slate-400"
      >
        Resume with message
      </label>
      <p className="mt-1 text-xs text-slate-500">
        Recreates the worktree from{' '}
        {branch ? (
          <code className="rounded bg-slate-800 px-1 font-mono text-slate-300">
            {branch}
          </code>
        ) : (
          'the surviving branch'
        )}{' '}
        and resumes the Claude session with your message.
      </p>
      <textarea
        id={messageId}
        value={message}
        onChange={(e) => setMessage(e.target.value)}
        placeholder="Tell the worker what to do on resume…"
        rows={3}
        disabled={busy}
        className="mt-2 w-full resize-y rounded-md border border-slate-700 bg-slate-950 px-3 py-2 text-sm text-slate-100 placeholder-slate-500 focus:border-amber-400 focus:outline-none focus:ring-1 focus:ring-amber-400/50 disabled:opacity-60"
      />
      <div className="mt-2 flex justify-end">
        <button
          type="submit"
          disabled={!canSubmit}
          className="inline-flex items-center gap-1.5 rounded-md border border-amber-400/40 bg-amber-500/10 px-3 py-1.5 text-sm font-medium text-amber-200 hover:bg-amber-500/20 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-300 disabled:cursor-not-allowed disabled:opacity-50"
        >
          <PlayCircle size={14} aria-hidden />
          {busy ? 'Resuming…' : 'Resume with message'}
        </button>
      </div>
    </form>
  )
}
