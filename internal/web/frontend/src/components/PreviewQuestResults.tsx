import { AlertTriangle, CircleCheck, CircleSlash, Loader2, XCircle } from 'lucide-react'
import type { QuestOutcomeSummary, QuestRunStatus, QuestRunSummary } from '../api/previews'
import { formatDuration } from '../lib/previewFormat'

// BADGE describes each run status once: the label, the tint and the icon.
//
// Note what `failed` is *not* tinted: red-on-red like a broken pipeline. A
// quest run against a preview is informational — nothing in Forge gates a
// merge, a PR or a pipeline stage on it — so a failure is an amber "look at
// this", not a block. The only red here is `error`, and that is about the run
// falling over rather than about the branch.
type KnownQuestRunStatus = 'running' | 'passed' | 'failed' | 'skipped' | 'error'

const BADGE: Record<
  KnownQuestRunStatus | 'unknown',
  { label: string; classes: string; Icon: typeof CircleCheck; spin?: boolean }
> = {
  running: {
    label: 'Running quests…',
    classes: 'border-sky-500/40 bg-sky-500/10 text-sky-300',
    Icon: Loader2,
    spin: true,
  },
  passed: {
    label: 'Quests passed',
    classes: 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300',
    Icon: CircleCheck,
  },
  failed: {
    label: 'Quests failed',
    classes: 'border-amber-500/40 bg-amber-500/10 text-amber-300',
    Icon: AlertTriangle,
  },
  skipped: {
    label: 'Quests skipped',
    classes: 'border-slate-700 bg-slate-800/60 text-slate-300',
    Icon: CircleSlash,
  },
  error: {
    label: 'Quest run errored',
    classes: 'border-red-500/40 bg-red-500/10 text-red-300',
    Icon: XCircle,
  },
  unknown: {
    label: 'Quest run',
    classes: 'border-slate-700 bg-slate-800/60 text-slate-300',
    Icon: CircleSlash,
  },
}

// badgeFor tolerates a status this build does not know about — a newer daemon
// is not a reason to render nothing — by falling back to the neutral badge.
function badgeFor(status: QuestRunStatus) {
  return BADGE[status as KnownQuestRunStatus] ?? BADGE.unknown
}

export interface PreviewQuestResultsProps {
  run: QuestRunSummary
  /** Test hook so a caller can address the block of a specific bead. */
  testId?: string
}

// PreviewQuestResults renders one quest run: the status badge, a row per quest
// with its verdict and duration, and thumbnails of whatever the run captured.
//
// It is presentational — the dispatch and polling live in usePreviewQuests — so
// the bead-detail panel and any future surface render the same block from the
// same data.
export default function PreviewQuestResults({ run, testId }: PreviewQuestResultsProps) {
  const { label, classes, Icon, spin } = badgeFor(run.status)
  const failures = run.quests.filter((q) => !q.passed).length
  const summary =
    run.status === 'running'
      ? `${run.quests.length} so far`
      : `${run.quests.length - failures}/${run.quests.length} passed`

  return (
    <section
      data-testid={testId}
      data-status={run.status}
      aria-label="Preview quest run"
      className="border-t border-slate-800/60"
    >
      <div className="flex flex-wrap items-center gap-2 px-4 py-2 text-xs">
        <span
          data-testid="quest-run-status"
          data-status={run.status}
          className={`inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 ${classes}`}
        >
          <Icon size={12} aria-hidden className={spin ? 'animate-spin' : undefined} />
          <span>{label}</span>
        </span>
        {run.quests.length > 0 && <span className="text-slate-500">{summary}</span>}
        {run.status !== 'running' && run.duration_seconds > 0 && (
          <span className="text-slate-500">in {formatDuration(run.duration_seconds)}</span>
        )}
        {/* The one thing an operator must not have to infer: a red run here
            changes nothing about whether the branch can merge. */}
        {(run.status === 'failed' || run.status === 'error') && (
          <span data-testid="quest-run-advisory" className="ml-auto text-slate-500">
            Informational — does not block the PR
          </span>
        )}
      </div>

      {run.skip_reason && (
        <p data-testid="quest-run-skip-reason" className="px-4 pb-2 text-xs text-slate-400">
          {run.skip_reason}
        </p>
      )}
      {run.error && (
        <p data-testid="quest-run-error" className="px-4 pb-2 text-xs text-red-200">
          {run.error}
        </p>
      )}

      {run.quests.length > 0 && (
        <ul className="divide-y divide-slate-800/60 border-t border-slate-800/60">
          {run.quests.map((quest) => (
            <QuestRow key={quest.name} quest={quest} />
          ))}
        </ul>
      )}
    </section>
  )
}

// QuestRow is one quest's verdict, its failure detail and its screenshots.
function QuestRow({ quest }: { quest: QuestOutcomeSummary }) {
  return (
    <li data-testid={`quest-row-${quest.name}`} data-passed={String(quest.passed)} className="px-4 py-2">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        {quest.passed ? (
          <CircleCheck size={13} className="text-emerald-400" aria-label="passed" />
        ) : (
          <AlertTriangle size={13} className="text-amber-400" aria-label="failed" />
        )}
        <span className="font-mono text-slate-200">{quest.name}</span>
        {quest.duration_seconds > 0 && (
          <span className="text-slate-500">{formatDuration(quest.duration_seconds)}</span>
        )}
        {!quest.passed && quest.failed_step >= 0 && (
          <span className="text-slate-500">step {quest.failed_step}</span>
        )}
      </div>

      {!quest.passed && quest.error_message && (
        <p data-testid={`quest-error-${quest.name}`} className="mt-1 text-xs text-amber-200">
          {quest.error_message}
        </p>
      )}

      {quest.screenshots.length > 0 && (
        <ul className="mt-2 flex flex-wrap gap-2">
          {quest.screenshots.map((shot) => (
            <li key={shot.url}>
              {/* Opens the full image in a tab rather than a lightbox: the
                  thumbnail is evidence to glance at, and a browser tab already
                  does zoom and save better than a modal would. */}
              <a href={shot.url} target="_blank" rel="noreferrer" title={shot.name}>
                <img
                  src={shot.url}
                  alt={`${quest.name} — ${shot.name}`}
                  data-testid={`quest-screenshot-${quest.name}`}
                  loading="lazy"
                  className="h-16 w-28 rounded border border-slate-700 bg-slate-950 object-cover object-top transition-colors hover:border-slate-500"
                />
              </a>
            </li>
          ))}
        </ul>
      )}
    </li>
  )
}
