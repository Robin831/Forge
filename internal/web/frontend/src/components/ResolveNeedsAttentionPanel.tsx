import { useEffect, useMemo, useState } from 'react'
import { AlertTriangle, ExternalLink, X } from 'lucide-react'
import type { EscalationDetail } from '../api/forge'
import { useEscalation, useResolveActions } from '../stores/resolveStore'

// EscalationType narrows the panel to the two failure modes the daemon
// raises today. Both share the same data shape but consumers may want to
// label the panel differently (e.g. "Smith failed" vs. "Dispatch failed"
// in the header copy).
export type EscalationType = 'dispatch_failed' | 'smith_failed'

export interface ResolveNeedsAttentionPanelProps {
  // escalationId is the bead id the operator is triaging; the store
  // caches escalation detail under this key so re-renders are cheap.
  escalationId: string
  // escalationType drives the header copy and any future type-specific
  // affordances; data fetching is identical for both kinds today.
  escalationType: EscalationType
  // onClose, when provided, renders a close button in the header. The
  // panel does not own its own modal chrome — the parent decides whether
  // to render it inline or in a dialog.
  onClose?: () => void
}

const TYPE_TITLE: Record<EscalationType, string> = {
  dispatch_failed: 'Dispatch failed — needs attention',
  smith_failed: 'Smith failed — needs attention',
}

// formatCommitList renders a list of commits as monospace lines. The
// daemon already truncates to maxEscalationCommits, so we only need to
// render what we received.
function formatCommitList(commits: readonly string[] | undefined): string {
  if (!commits || commits.length === 0) return '(none)'
  return commits.join('\n')
}

// buildContextBlock assembles the diff / branch context block from the
// escalation response. We render it as one preformatted text node so the
// operator can copy-paste it into a bug report.
function buildContextBlock(detail: EscalationDetail): string {
  const lines: string[] = []
  if (detail.branch) lines.push(`branch: ${detail.branch}`)
  if (detail.worktree_path) {
    lines.push(
      `worktree: ${detail.worktree_path}${
        detail.worktree_exists ? '' : ' (missing)'
      }`,
    )
  }
  const ctx = detail.context
  if (ctx) {
    if (ctx.parent_base) lines.push(`parent base: ${ctx.parent_base}`)
    if (ctx.diff_range) lines.push(`diff range: ${ctx.diff_range}`)
    if (ctx.origin_branch_ref) {
      lines.push(
        `origin branch: ${ctx.origin_branch_ref}${
          ctx.origin_branch_exists ? '' : ' (not pushed)'
        }`,
      )
    }
    lines.push('')
    lines.push('origin commits:')
    lines.push(formatCommitList(ctx.origin_commits))
    lines.push('')
    lines.push('local commits:')
    lines.push(formatCommitList(ctx.local_commits))
    if (ctx.diff_stat) {
      lines.push('')
      lines.push('diff stat:')
      lines.push(ctx.diff_stat)
    }
  }
  return lines.join('\n')
}

// SkeletonBlock is a single placeholder bar used while the escalation
// detail is loading. Three of these stacked roughly approximate the final
// layout so the panel does not jump on first paint.
function SkeletonBlock({ className = '' }: { className?: string }) {
  return (
    <div
      aria-hidden
      className={`animate-pulse rounded-md bg-slate-800/70 ${className}`}
    />
  )
}

export default function ResolveNeedsAttentionPanel({
  escalationId,
  escalationType,
  onClose,
}: ResolveNeedsAttentionPanelProps) {
  const entry = useEscalation(escalationId)
  const { fetchEscalation } = useResolveActions()
  const [auditNote, setAuditNote] = useState('')
  // prFormOpen toggles the inline title/body fallback form. We default to
  // closed so the panel renders compactly; clicking "Open PR manually"
  // expands it when no anchor URL is buildable.
  const [prFormOpen, setPrFormOpen] = useState(false)
  const [prTitle, setPrTitle] = useState('')
  const [prBody, setPrBody] = useState('')

  useEffect(() => {
    if (!escalationId) return
    void fetchEscalation(escalationId)
  }, [escalationId, fetchEscalation])

  const detail = entry.data
  const contextBlock = useMemo(
    () => (detail ? buildContextBlock(detail) : ''),
    [detail],
  )

  const isLoading = entry.status === 'loading' && !detail
  const isError = entry.status === 'error' && !detail

  return (
    <section
      aria-label={TYPE_TITLE[escalationType]}
      className="flex flex-col gap-4 rounded-xl border border-amber-700/40 bg-slate-900/80 p-5 text-slate-100 shadow-xl"
    >
      <header className="flex items-start gap-3">
        <AlertTriangle
          size={20}
          className="mt-0.5 shrink-0 text-amber-400"
          aria-hidden
        />
        <div className="flex-1">
          <h2 className="text-base font-semibold text-amber-200">
            {TYPE_TITLE[escalationType]}
          </h2>
          <p className="mt-1 font-mono text-xs text-slate-400">
            {escalationId}
            {detail?.anvil ? ` · ${detail.anvil}` : ''}
          </p>
        </div>
        {onClose && (
          <button
            type="button"
            onClick={onClose}
            aria-label="Close panel"
            className="rounded-md border border-slate-700 bg-slate-800 p-1.5 text-slate-300 hover:bg-slate-700 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-400"
          >
            <X size={14} aria-hidden />
          </button>
        )}
      </header>

      {isLoading && (
        <div className="flex flex-col gap-3" role="status" aria-live="polite">
          <span className="sr-only">Loading escalation detail…</span>
          <SkeletonBlock className="h-4 w-2/3" />
          <SkeletonBlock className="h-20 w-full" />
          <SkeletonBlock className="h-32 w-full" />
        </div>
      )}

      {isError && (
        <div
          role="alert"
          className="rounded-md border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-200"
        >
          Failed to load escalation: {entry.error ?? 'unknown error'}
        </div>
      )}

      {detail && (
        <>
          <div>
            <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-400">
              Escalation message
            </h3>
            <div className="mt-2 whitespace-pre-wrap rounded-md border border-slate-800 bg-slate-950/60 px-3 py-2 text-sm text-slate-100">
              {detail.escalation_message || '(no message recorded)'}
            </div>
            {detail.errors && detail.errors.length > 0 && (
              <ul className="mt-2 list-disc space-y-1 pl-5 text-xs text-amber-300">
                {detail.errors.map((e, i) => (
                  <li key={i}>{e}</li>
                ))}
              </ul>
            )}
          </div>

          <div>
            <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-400">
              Commit &amp; diff context
            </h3>
            <pre className="mt-2 max-h-64 overflow-auto rounded-md border border-slate-800 bg-slate-950/60 px-3 py-2 font-mono text-xs leading-relaxed text-slate-200">
              {contextBlock || '(no git context available)'}
            </pre>
          </div>

          <div>
            <label
              htmlFor="resolve-audit-note"
              className="text-xs font-semibold uppercase tracking-wide text-slate-400"
            >
              Audit note
            </label>
            <textarea
              id="resolve-audit-note"
              value={auditNote}
              onChange={(e) => setAuditNote(e.target.value)}
              placeholder="Optional note recorded with any resolve action (visible in the event log)."
              rows={3}
              className="mt-2 w-full resize-y rounded-md border border-slate-700 bg-slate-950 px-3 py-2 text-sm text-slate-100 placeholder-slate-500 focus:border-amber-400 focus:outline-none focus:ring-1 focus:ring-amber-400/50"
            />
          </div>

          <div>
            <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-400">
              Open PR manually
            </h3>
            <p className="mt-1 text-xs text-slate-400">
              The daemon does not know the GitHub repository URL for this
              anvil, so opening a PR directly is not wired up. Use the form
              below to draft the title and body, then paste them into
              <code className="mx-1 rounded bg-slate-800 px-1 py-0.5 font-mono text-[11px] text-slate-200">
                gh pr create
              </code>
              from the worktree.
            </p>
            {!prFormOpen ? (
              <button
                type="button"
                onClick={() => setPrFormOpen(true)}
                className="mt-2 inline-flex items-center gap-1.5 rounded-md border border-slate-700 bg-slate-800 px-3 py-1.5 text-sm text-slate-200 hover:bg-slate-700 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-400"
              >
                <ExternalLink size={14} aria-hidden />
                Draft PR title &amp; body
              </button>
            ) : (
              <div className="mt-2 flex flex-col gap-3">
                <label className="text-xs font-medium text-slate-400">
                  Title
                  <input
                    type="text"
                    value={prTitle}
                    onChange={(e) => setPrTitle(e.target.value)}
                    placeholder={detail.branch ?? 'PR title'}
                    className="mt-1 w-full rounded-md border border-slate-700 bg-slate-950 px-3 py-2 text-sm text-slate-100 placeholder-slate-500 focus:border-amber-400 focus:outline-none focus:ring-1 focus:ring-amber-400/50"
                  />
                </label>
                <label className="text-xs font-medium text-slate-400">
                  Body
                  <textarea
                    value={prBody}
                    onChange={(e) => setPrBody(e.target.value)}
                    placeholder="Summary, test plan, etc."
                    rows={5}
                    className="mt-1 w-full resize-y rounded-md border border-slate-700 bg-slate-950 px-3 py-2 text-sm text-slate-100 placeholder-slate-500 focus:border-amber-400 focus:outline-none focus:ring-1 focus:ring-amber-400/50"
                  />
                </label>
              </div>
            )}
          </div>
        </>
      )}
    </section>
  )
}
