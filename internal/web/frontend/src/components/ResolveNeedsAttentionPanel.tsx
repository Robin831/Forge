import { useEffect, useMemo, useState } from 'react'
import { AlertTriangle, ExternalLink, X } from 'lucide-react'
import type { EscalationDetail, EscalationType, ResolveVerb } from '../api/forge'
import ConfirmModal from './ConfirmModal'
import {
  resolveKey,
  useEscalation,
  useResolveActions,
  useResolveStatus,
} from '../stores/resolveStore'

// EscalationType is shared with the API layer (api/forge.ts) so the
// needs-attention list and this panel agree on the discriminator. Re-export
// it here for the handful of older imports that reach for it via the panel.
export type { EscalationType }

// BEAD_VERBS lists the resolve verbs applicable when the bead failed to
// dispatch — no worker process ever started, so 'stop' (kill worker) is
// intentionally absent: the daemon would have nothing to kill. The two
// dispatch overrides — approve-as-is and warden-rerun — are included
// because dispatch_failed often means Warden rejected (hallucination,
// truncation) and the operator wants to ship the worker's branch anyway
// or ask Warden to look again.
const BEAD_VERBS: readonly ResolveVerb[] = [
  'retry',
  'clarify',
  'unclarify',
  'clear',
  'warden-rerun',
  'approve-as-is',
]

// WORKER_VERBS lists the resolve verbs applicable when a smith worker
// failed mid-execution. warden-rerun is included so the operator can
// nudge Warden without killing+respawning Smith; approve-as-is is
// intentionally omitted because shipping a still-running worker's branch
// is ambiguous (the worker may be midway through a write).
const WORKER_VERBS: readonly ResolveVerb[] = [
  'retry',
  'stop',
  'clarify',
  'unclarify',
  'clear',
  'warden-rerun',
]

// VERB_LABEL maps each verb to the button label rendered in the action
// row. Kept here (rather than co-located with RESOLVE_VERBS in api/forge)
// so the wording stays close to the panel that uses it.
const VERB_LABEL: Record<ResolveVerb, string> = {
  retry: 'Retry',
  stop: 'Stop worker',
  clarify: 'Needs clarification',
  unclarify: 'Clear clarification',
  clear: 'Clear flag',
  'approve-as-is': 'Approve as-is (skip Warden)',
  'warden-rerun': 'Re-run Warden',
  'create-pr': 'Create PR',
}

// VerbSpec is one entry in an escalation type's action set. `label` overrides
// the default VERB_LABEL wording when a type wants escalation-specific copy
// (e.g. the stranded-branch type renames approve-as-is to "Open PR from
// branch") without changing the underlying verb the backend receives.
interface VerbSpec {
  verb: ResolveVerb
  label?: string
}

// STRANDED_BRANCH_VERBS is the action set for a
// dispatch_blocked_stranded_branch escalation: a prior worker pushed
// origin/forge/<bead> but never opened a PR. The three operator choices map
// to existing resolve verbs — open a PR from that branch (approve-as-is),
// reset the branch and re-dispatch (retry), or accept the situation and clear
// the flag (clear) — but are labelled for the stranded-branch context.
const STRANDED_BRANCH_VERBS: readonly VerbSpec[] = [
  { verb: 'approve-as-is', label: 'Open PR from branch' },
  { verb: 'retry', label: 'Reset branch & retry' },
  { verb: 'clear', label: 'Accept & clear' },
]

// CLARIFICATION_VERBS is the action set for a clarification escalation: the
// operator either records the clarification (clarify, requires an audit note)
// or clears the flag once the bead is unambiguous (unclarify).
const CLARIFICATION_VERBS: readonly VerbSpec[] = [
  { verb: 'clarify' },
  { verb: 'unclarify' },
]

// PR_CREATE_FAILED_VERBS is the action set for a pr_create_failed escalation:
// a prior worker pushed origin/forge/<bead> and finished its work, but the
// final PR open failed (auth/transient/protected-branch). The primary recovery
// is "Create PR" — open a PR for the existing branch without re-running Smith;
// the operator can instead reset & re-dispatch through Smith, or accept the
// situation and clear the flag.
const PR_CREATE_FAILED_VERBS: readonly VerbSpec[] = [
  { verb: 'create-pr' },
  { verb: 'retry', label: 'Reset & retry' },
  { verb: 'clear', label: 'Accept & clear' },
]

// verbSpecsFor maps an escalation type to its ordered action set. The two
// recovery/worker modes share WORKER_VERBS; dispatch_failed keeps the broader
// BEAD_VERBS (no live worker to stop); the bead-centric types added by
// Forge-iz6s get their own tailored sets.
function verbSpecsFor(type: EscalationType): readonly VerbSpec[] {
  switch (type) {
    case 'dispatch_blocked_stranded_branch':
      return STRANDED_BRANCH_VERBS
    case 'pr_create_failed':
      return PR_CREATE_FAILED_VERBS
    case 'clarification':
      return CLARIFICATION_VERBS
    case 'dispatch_failed':
      return BEAD_VERBS.map((verb) => ({ verb }))
    case 'smith_failed':
    case 'recovery_failed':
    default:
      return WORKER_VERBS.map((verb) => ({ verb }))
  }
}

export interface ResolveNeedsAttentionPanelProps {
  // escalationId is the bead id the operator is triaging; the store
  // caches escalation detail under this key so re-renders are cheap.
  escalationId: string
  // escalationType drives the header copy and which resolve actions render.
  // The bead-centric needs-attention list passes the real type derived from
  // the bead's latest lifecycle event; the legacy failed-worker affordance
  // still passes 'smith_failed'.
  escalationType: EscalationType
  // anvil, when provided, disambiguates the escalation fetch for a bead id
  // that exists in more than one anvil. The needs-attention list always
  // supplies it; the worker-row affordance can omit it (the worker's anvil
  // is already unambiguous).
  anvil?: string
  // onClose, when provided, renders a close button in the header. The
  // panel does not own its own modal chrome — the parent decides whether
  // to render it inline or in a dialog.
  onClose?: () => void
}

// DESTRUCTIVE_VERBS lists the verbs that prompt for confirmation before
// dispatching. Retry kicks the pipeline back to Smith and stop kills a
// running worker; approve-as-is bypasses Warden and ships the existing
// branch — all three are easy to fire by accident from the keyboard.
const DESTRUCTIVE_VERBS: ReadonlySet<ResolveVerb> = new Set<ResolveVerb>([
  'retry',
  'stop',
  'approve-as-is',
  // create-pr opens a real GitHub PR (outward-facing); confirm before firing.
  'create-pr',
])


const TYPE_TITLE: Record<EscalationType, string> = {
  dispatch_failed: 'Dispatch failed — needs attention',
  smith_failed: 'Smith failed — needs attention',
  recovery_failed: 'Recovery failed — needs attention',
  dispatch_blocked_stranded_branch: 'Stranded branch — needs attention',
  clarification: 'Clarification needed',
  pr_create_failed: 'PR creation failed — needs attention',
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
  anvil,
  onClose,
}: ResolveNeedsAttentionPanelProps) {
  const entry = useEscalation(escalationId, anvil)
  const { fetchEscalation, run, reset } = useResolveActions()
  const auditNoteId = `resolve-audit-note-${escalationId}`
  const [auditNote, setAuditNote] = useState('')
  // confirmVerb is non-null while the confirmation modal is open for a
  // destructive verb. Storing the verb + label (rather than a boolean) lets
  // the modal title/body adapt per verb (retry / stop / approve-as-is) and
  // reflect the escalation-specific label (e.g. "Open PR from branch"), and
  // lets the confirm callback know which action to dispatch.
  const [confirmVerb, setConfirmVerb] = useState<{
    verb: ResolveVerb
    label: string
  } | null>(null)

  useEffect(() => {
    if (!escalationId) return
    void fetchEscalation(escalationId, anvil)
  }, [escalationId, anvil, fetchEscalation])

  const detail = entry.data
  const contextBlock = useMemo(
    () => (detail ? buildContextBlock(detail) : ''),
    [detail],
  )

  const isLoading = entry.status === 'loading' && !detail
  const isError = entry.status === 'error' && !detail

  const verbSpecs = useMemo(() => verbSpecsFor(escalationType), [escalationType])

  // The resolve-store entry is keyed on (anvil, beadID). When the
  // escalation detail hasn't loaded yet we fall back to a placeholder key
  // so the hook still has a stable string; the action buttons themselves
  // are only rendered inside the `{detail && ...}` branch so the
  // placeholder is never actually clicked through.
  const actionKey = detail
    ? resolveKey(escalationId, detail.anvil)
    : `__pending__/${escalationId}`
  const actionEntry = useResolveStatus(actionKey)
  const actionPending = actionEntry.status === 'pending'
  const actionError = actionEntry.status === 'error' ? actionEntry.error : undefined
  // The create-pr verb returns a structured success payload (PR number + URL)
  // that we surface inline as a clickable link; no other verb populates it.
  const createPRResult =
    actionEntry.status === 'success' && actionEntry.verb === 'create-pr'
      ? actionEntry.result
      : undefined
  const actionsDisabled = actionPending || actionEntry.status === 'error'
  const anvilMissing = detail != null && !detail.anvil

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
              htmlFor={auditNoteId}
              className="text-xs font-semibold uppercase tracking-wide text-slate-400"
            >
              Audit note
            </label>
            <textarea
              id={auditNoteId}
              value={auditNote}
              onChange={(e) => setAuditNote(e.target.value)}
              placeholder="Optional note recorded with any resolve action (visible in the event log)."
              rows={3}
              className="mt-2 w-full resize-y rounded-md border border-slate-700 bg-slate-950 px-3 py-2 text-sm text-slate-100 placeholder-slate-500 focus:border-amber-400 focus:outline-none focus:ring-1 focus:ring-amber-400/50"
            />
          </div>

          <div>
            <h3 className="text-xs font-semibold uppercase tracking-wide text-slate-400">
              Resolve actions
            </h3>
            {anvilMissing ? (
              <p className="mt-2 rounded-md border border-amber-700/40 bg-amber-900/20 px-3 py-2 text-xs text-amber-300">
                Anvil is ambiguous — retry the fetch with an explicit{' '}
                <code className="rounded bg-slate-800 px-1 font-mono">
                  ?anvil=
                </code>{' '}
                parameter before resolving.
              </p>
            ) : (
              <div
                role="group"
                aria-label="Resolve actions"
                className="mt-2 flex flex-wrap gap-2"
              >
                {verbSpecs.map(({ verb, label }) => {
                  const verbLabel = label ?? VERB_LABEL[verb]
                  const invoke = () => {
                    const note = auditNote.trim()
                    void run(actionKey, escalationId, {
                      verb,
                      anvil: detail.anvil,
                      note: note === '' ? undefined : note,
                    })
                  }
                  const onClick = () => {
                    if (DESTRUCTIVE_VERBS.has(verb)) {
                      setConfirmVerb({ verb, label: verbLabel })
                      return
                    }
                    invoke()
                  }
                  const isActive =
                    actionPending && actionEntry.verb === verb
                  const isVerbDisabled =
                    actionsDisabled ||
                    (verb === 'clarify' && auditNote.trim() === '')
                  return (
                    <button
                      key={verb}
                      type="button"
                      data-verb={verb}
                      onClick={onClick}
                      disabled={isVerbDisabled}
                      title={
                        verb === 'clarify' && auditNote.trim() === ''
                          ? 'Enter an audit note before marking as needs clarification'
                          : undefined
                      }
                      className="inline-flex items-center gap-1.5 rounded-md border border-slate-700 bg-slate-800 px-3 py-1.5 text-sm font-medium text-slate-100 hover:bg-slate-700 focus:outline-none focus-visible:ring-2 focus-visible:ring-amber-400 disabled:cursor-not-allowed disabled:opacity-50"
                    >
                      {isActive ? `${verbLabel}…` : verbLabel}
                    </button>
                  )
                })}
              </div>
            )}
            {actionError && (
              <div
                role="alert"
                className="mt-2 flex items-start gap-2 rounded-md border border-red-700/40 bg-red-900/20 px-3 py-2 text-sm text-red-200"
              >
                <span className="flex-1">{actionError}</span>
                <button
                  type="button"
                  onClick={() => reset(actionKey)}
                  className="shrink-0 underline hover:no-underline focus:outline-none focus-visible:ring-1 focus-visible:ring-red-400"
                >
                  Dismiss
                </button>
              </div>
            )}
            {createPRResult && (
              <div
                role="status"
                aria-live="polite"
                className="mt-2 flex flex-wrap items-center gap-2 rounded-md border border-emerald-700/40 bg-emerald-900/20 px-3 py-2 text-sm text-emerald-200"
              >
                <span>
                  {createPRResult.prNumber
                    ? `Opened PR #${createPRResult.prNumber}.`
                    : createPRResult.message || 'PR created.'}
                </span>
                {createPRResult.prUrl && (
                  <a
                    href={createPRResult.prUrl}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1 font-medium underline hover:no-underline focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400"
                  >
                    View on GitHub
                    <ExternalLink size={12} aria-hidden />
                  </a>
                )}
              </div>
            )}
          </div>

        </>
      )}
      <ConfirmModal
        open={confirmVerb !== null}
        title={confirmVerb ? `Confirm: ${confirmVerb.label}` : ''}
        message={
          confirmVerb?.verb === 'stop'
            ? 'Killing the worker stops the running smith process and prevents re-dispatch until the flag is cleared. Continue?'
            : confirmVerb?.verb === 'approve-as-is'
              ? "This will skip the Warden review and open a PR from the worker's existing branch. Continue?"
              : confirmVerb?.verb === 'create-pr'
                ? 'This opens a pull request for the already-pushed branch (without re-running Smith) and hands it to Bellows. Continue?'
                : 'Retrying clears the needs-attention flag and re-dispatches this bead on the next poll. Continue?'
        }
        tone="danger"
        busy={actionPending}
        onCancel={() => setConfirmVerb(null)}
        onConfirm={() => {
          if (confirmVerb && detail) {
            const verb = confirmVerb.verb
            const note = auditNote.trim()
            // Close the modal before dispatching so rapid clicks cannot
            // fire the action more than once before React re-renders.
            setConfirmVerb(null)
            void run(actionKey, escalationId, {
              verb,
              anvil: detail.anvil,
              note: note === '' ? undefined : note,
            })
          }
        }}
      />
    </section>
  )
}
