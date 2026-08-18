import { useMemo } from 'react'
import {
  ChevronRight,
  CircleCheck,
  ClipboardList,
  FlaskConical,
  GitPullRequest,
  Hammer,
  Microscope,
  ShieldCheck,
  type LucideIcon,
} from 'lucide-react'
import type { WorkerInfo } from '../api'
import PreviewButton from './PreviewButton'

// StageKey labels the seven visible columns of the pipeline bar. The Bellows
// PR-monitor is intentionally folded into the "pr" stage as a sub-label
// rather than getting its own column — see the per-bead description.
// "assay" sits between the PR stage and ready-to-merge: Bellows opens the PR,
// then triggers an Assay review (multi-pass Claude review worker), and only
// once CI and threads clear does the PR mature to ready-to-merge.
// "ready_to_merge" sits after the Assay stage and counts PRs the daemon has
// promoted (CI green, no pending reviews, no unresolved threads, not
// conflicting) so an operator can see what's waiting on a human merge click.
type StageKey = 'schematic' | 'smith' | 'temper' | 'warden' | 'pr' | 'assay' | 'ready_to_merge'

const STAGES: StageKey[] = ['schematic', 'smith', 'temper', 'warden', 'pr', 'assay', 'ready_to_merge']

const STAGE_LABEL: Record<StageKey, string> = {
  schematic: 'Schematic',
  smith: 'Smith',
  temper: 'Temper',
  warden: 'Warden',
  pr: 'PR',
  assay: 'Assay',
  ready_to_merge: 'Ready to merge',
}

// Stage accents mirror Hytte's Mezzanine PipelineBar palette so users moving
// between the two dashboards get a consistent visual mapping. Each value is a
// Tailwind class applied to the top border of the stage pill.
const STAGE_ACCENT: Record<StageKey, string> = {
  schematic: 'border-t-purple-500',
  smith: 'border-t-orange-500',
  temper: 'border-t-yellow-500',
  warden: 'border-t-cyan-500',
  pr: 'border-t-blue-500',
  assay: 'border-t-pink-500',
  ready_to_merge: 'border-t-emerald-500',
}

// STAGE_ICON gives each stage a distinct shape so stages are distinguishable
// without relying on the accent colour alone — important for color-vision
// deficiency, where adjacent accents (e.g. pink Assay vs emerald Ready to
// merge) are easily confused. The icon is shown on every stage pill (next to
// the label) and in the per-bead marker strip.
const STAGE_ICON: Record<StageKey, LucideIcon> = {
  schematic: ClipboardList,
  smith: Hammer,
  temper: FlaskConical,
  warden: ShieldCheck,
  pr: GitPullRequest,
  assay: Microscope,
  ready_to_merge: CircleCheck,
}

// phaseToStage projects a backend worker phase string onto one of the seven
// pipeline-bar stages. Bellows + its sub-workers (quench/burnish/rebase) all
// land on "pr" because to the user they are one logical stage; we surface the
// sub-state separately via prBellowsLabel. The daemon promotes a bellows
// synthetic monitor's phase to "ready_to_merge" once the underlying PR
// satisfies every ready-to-merge condition.
export function phaseToStage(phase: string | undefined): StageKey | null {
  switch (phase) {
    case 'schematic':
      return 'schematic'
    case 'smith':
      return 'smith'
    case 'temper':
      return 'temper'
    case 'warden':
      return 'warden'
    case 'bellows':
    case 'quench':
    case 'burnish':
    case 'rebase':
    case 'cifix':
    case 'reviewfix':
      return 'pr'
    case 'assay':
      return 'assay'
    case 'ready_to_merge':
      return 'ready_to_merge'
    // crucible, smelter, schematic-fail and any unknown phase fall through
    // and are not counted on the bar — they don't map cleanly to a stage.
    // "merged" is intentionally absent: once a PR merges its synthetic
    // bellows row is swept and the bead leaves the in-flight pipeline.
    default:
      return null
  }
}

// isBellowsMonitor reports whether the worker is the synthetic PR-monitor row
// that bellows upserts for each open PR. The synthetic row has a
// "bellows-<anvil>-<num>" id prefix and no log_path. Pipeline workers that
// temporarily enter phase=bellows are NOT synthetic monitors and must remain
// visible as regular clickable rows. The daemon promotes a synthetic
// monitor's phase to "ready_to_merge" once its PR is mergeable, so both
// phases qualify here.
export function isBellowsMonitor(w: WorkerInfo): boolean {
  return (
    (w.phase === 'bellows' || w.phase === 'ready_to_merge') &&
    !w.log_path &&
    w.id.startsWith('bellows-')
  )
}

// prSubLabel collapses any active bellows sub-state into a short caption
// shown under the PR pill. The order mirrors how a user would perceive
// importance: rebase > ci-fix > review-fix > detached > monitoring.
function prSubLabel(prWorkers: WorkerInfo[]): string | null {
  if (prWorkers.length === 0) return null
  const hasPhase = (p: string) => prWorkers.some((w) => w.phase === p)
  if (hasPhase('rebase')) return 'rebase'
  if (hasPhase('quench') || hasPhase('cifix')) return 'ci-fix'
  if (hasPhase('burnish') || hasPhase('reviewfix')) return 'review-fix'
  // A detached PR is muted: bellows keeps its row and keeps refreshing its
  // mergeability, but emits nothing and drives no lifecycle worker. Saying
  // "monitoring" for it would claim exactly the thing the operator turned off,
  // so it gets its own caption — and only when every monitor row here is
  // detached, since a fix worker still in flight is the more useful sub-state.
  const monitors = prWorkers.filter((w) => w.phase === 'bellows' || w.phase === 'ready_to_merge')
  if (monitors.length > 0 && monitors.every((w) => w.status === 'detached')) return 'detached'
  if (hasPhase('bellows')) return 'monitoring'
  return null
}

// ACTIVE_STATUSES are the worker statuses the pipeline bar renders. "detached"
// is here because a bellows monitor row for a muted PR is deliberately kept
// alive by the daemon (state.WorkerDetached is non-terminal): WorkersPane
// filters bellows monitors out on the grounds that their state is surfaced
// here, so dropping the status at this filter would make detaching a PR erase
// its monitor from the whole dashboard — which reads as a bug, not as a mute.
const ACTIVE_STATUSES: ReadonlySet<string> = new Set([
  'pending',
  'running',
  'monitoring',
  'reviewing',
  'detached',
])

interface StageInfo {
  key: StageKey
  count: number
  workers: WorkerInfo[]
  subLabel: string | null
}

function bucket(workers: WorkerInfo[]): Map<StageKey, WorkerInfo[]> {
  const m = new Map<StageKey, WorkerInfo[]>()
  for (const s of STAGES) m.set(s, [])
  for (const w of workers) {
    if (!ACTIVE_STATUSES.has(w.status)) continue
    const stage = phaseToStage(w.phase)
    if (!stage) continue
    m.get(stage)!.push(w)
  }
  return m
}

interface PipelineBarProps {
  workers: WorkerInfo[]
}

export default function PipelineBar({ workers }: PipelineBarProps) {
  const stages = useMemo<StageInfo[]>(() => {
    const buckets = bucket(workers)
    return STAGES.map<StageInfo>((key) => {
      const list = buckets.get(key) ?? []
      // Count unique beads (anvil+bead_id) rather than workers — a single bead
      // can contribute multiple workers to the same stage (e.g. bellows monitor
      // + quench), which would otherwise inflate the displayed count.
      const uniqueBeads = new Set(list.map((w) => `${w.anvil}:${w.bead_id}`))
      return {
        key,
        count: uniqueBeads.size,
        workers: list,
        subLabel: key === 'pr' ? prSubLabel(list) : null,
      }
    })
  }, [workers])

  // For the bead-row strip below the stage bar, we want one row per active
  // in-flight bead with a marker on its current stage. A bead is uniquely
  // identified by (anvil, bead_id). We pick the first worker for each bead
  // since multiple sub-workers (e.g. bellows monitor + quench) collapse onto
  // the same PR stage anyway.
  const beadRows = useMemo(() => {
    const seen = new Map<string, { worker: WorkerInfo; stage: StageKey }>()
    for (const w of workers) {
      if (!ACTIVE_STATUSES.has(w.status)) continue
      const stage = phaseToStage(w.phase)
      if (!stage) continue
      const key = `${w.anvil}:${w.bead_id}`
      // Prefer the non-bellows-monitor entry when a bead has both (e.g. quench
      // running while bellows monitors the PR) so the bead row shows the more
      // informative sub-state in the PR column. The daemon-promoted
      // "ready_to_merge" phase is a synthetic bellows monitor too, so we
      // treat it the same as "bellows" for replacement purposes.
      const existing = seen.get(key)
      const existingIsMonitor =
        existing &&
        (existing.worker.phase === 'bellows' || existing.worker.phase === 'ready_to_merge')
      const incomingIsMonitor = w.phase === 'bellows' || w.phase === 'ready_to_merge'
      if (!existing || (existingIsMonitor && !incomingIsMonitor)) {
        seen.set(key, { worker: w, stage })
      }
    }
    return Array.from(seen.values())
  }, [workers])

  return (
    <section
      aria-label="Pipeline overview"
      data-testid="pipeline-bar"
      className="rounded-xl border border-slate-800 bg-slate-900/40 p-3"
    >
      <h2 className="mb-2 px-1 text-xs font-semibold uppercase tracking-wide text-slate-400">
        Pipeline
      </h2>
      <div className="flex items-stretch gap-1 overflow-x-auto pb-1">
        {stages.map((s, i) => (
          <div key={s.key} className="flex min-w-0 flex-1 items-stretch">
            {i > 0 && (
              <div
                className="flex items-center px-0.5 text-slate-700"
                aria-hidden="true"
              >
                <ChevronRight size={14} />
              </div>
            )}
            <StagePill stage={s} />
          </div>
        ))}
      </div>

      {beadRows.length > 0 && (
        <ul
          aria-label="In-flight beads"
          className="mt-3 flex flex-col gap-1 border-t border-slate-800 pt-2"
        >
          {beadRows.map(({ worker, stage }) => (
            <BeadRow key={`${worker.anvil}:${worker.bead_id}`} worker={worker} stage={stage} />
          ))}
        </ul>
      )}
    </section>
  )
}

interface StagePillProps {
  stage: StageInfo
}

function StagePill({ stage }: StagePillProps) {
  // The ready-to-merge pill is the operator's "things you can click Merge on"
  // indicator, so the container glows emerald and the count badge swaps to
  // emerald (instead of the default amber) the moment it's non-zero. When the
  // count is zero we fall back to the standard muted styling so an empty
  // queue blends in with the other idle stages.
  const isReady = stage.key === 'ready_to_merge'
  const readyActive = isReady && stage.count > 0
  const StageIcon = STAGE_ICON[stage.key]
  const containerClass = readyActive
    ? `flex min-w-[88px] flex-1 flex-col rounded-md border border-emerald-500/50 border-t-2 bg-emerald-500/10 ${STAGE_ACCENT[stage.key]}`
    : `flex min-w-[88px] flex-1 flex-col rounded-md border border-slate-700/60 border-t-2 bg-slate-900/60 ${STAGE_ACCENT[stage.key]}`
  const labelClass = readyActive
    ? 'text-[11px] font-semibold text-emerald-100'
    : 'text-[11px] font-medium text-slate-200'
  const countClass =
    stage.count > 0
      ? readyActive
        ? 'flex h-[18px] min-w-[18px] items-center justify-center rounded-full px-1 text-[10px] font-bold bg-emerald-400/30 text-emerald-100'
        : 'flex h-[18px] min-w-[18px] items-center justify-center rounded-full px-1 text-[10px] font-semibold bg-amber-500/20 text-amber-200'
      : 'flex h-[18px] min-w-[18px] items-center justify-center rounded-full px-1 text-[10px] font-semibold bg-slate-800 text-slate-500'
  return (
    <div
      role="region"
      aria-label={`${STAGE_LABEL[stage.key]} stage`}
      data-testid={`pipeline-stage-${stage.key}`}
      className={containerClass}
    >
      <div className="flex items-center justify-between gap-1 px-2.5 py-1.5">
        <span className="flex min-w-0 items-center gap-1">
          <StageIcon size={12} className="shrink-0" aria-hidden="true" />
          <span className={`${labelClass} truncate`}>{STAGE_LABEL[stage.key]}</span>
        </span>
        <span className={countClass} data-testid={`pipeline-count-${stage.key}`}>
          {stage.count}
        </span>
      </div>
      {stage.subLabel && (
        <div
          className="border-t border-slate-700/40 px-2.5 py-1 text-[10px] uppercase tracking-wide text-blue-300"
          data-testid="pipeline-pr-sublabel"
        >
          Bellows: {stage.subLabel}
        </div>
      )}
    </div>
  )
}

interface BeadRowProps {
  worker: WorkerInfo
  stage: StageKey
}

// PREVIEW_STAGES are the bead-row stages that get inline Kiln preview
// controls. A preview is a detached checkout of the bead's pushed branch, so
// it only makes sense once the bead has reached the PR stage — before that
// there is nothing on the remote to build. Ready-to-merge is the case the
// controls exist for: judging a finished bead no longer requires a detour
// through the bead detail page.
const PREVIEW_STAGES: ReadonlySet<StageKey> = new Set(['pr', 'assay', 'ready_to_merge'])

function BeadRow({ worker, stage }: BeadRowProps) {
  return (
    <li
      data-testid="pipeline-bead-row"
      data-bead-id={worker.bead_id}
      className="flex items-center gap-2 rounded-md bg-slate-900/40 px-2 py-1 text-xs"
    >
      <span className="w-40 shrink-0 truncate font-mono text-[11px] text-slate-400">
        {worker.bead_id}
      </span>
      <span className="min-w-0 flex-1 truncate text-slate-200">
        {worker.title || worker.bead_id}
      </span>
      {worker.pr_number ? (
        worker.pr_url ? (
          <a
            href={worker.pr_url}
            target="_blank"
            rel="noreferrer"
            title={worker.pr_url}
            className="shrink-0 rounded-md border border-purple-500/40 bg-purple-500/10 px-1.5 py-0.5 text-[10px] text-purple-300 hover:bg-purple-500/20 hover:underline"
          >
            PR #{worker.pr_number}
          </a>
        ) : (
          <span className="shrink-0 rounded-md border border-purple-500/40 bg-purple-500/10 px-1.5 py-0.5 text-[10px] text-purple-300">
            PR #{worker.pr_number}
          </span>
        )
      ) : null}
      {/* A detached bead row is the mute made visible. Without it the row is
          indistinguishable from a monitored one, and the stage caption is an
          aggregate over every bead in the PR stage — it says "monitoring" as
          soon as one other PR is still watched. */}
      {worker.status === 'detached' && (
        <span
          data-testid="pipeline-bead-detached"
          title="Detached from Bellows: state is still refreshed, but no events are emitted and no fix workers run"
          className="shrink-0 rounded-md border border-slate-500/40 bg-slate-500/10 px-1.5 py-0.5 text-[10px] uppercase tracking-wide text-slate-300"
        >
          muted
        </span>
      )}
      {/* Start / status-spinner / Open / Stop for the bead's Kiln preview,
          right on the row — the button hides itself when Kiln is off or the
          anvil declares no preview manifest, so rows only gain controls where
          a preview is actually possible. */}
      {PREVIEW_STAGES.has(stage) && (
        <PreviewButton
          beadId={worker.bead_id}
          anvil={worker.anvil}
          compact
          className="shrink-0"
        />
      )}
      <div className="flex items-center gap-1">
        {STAGES.map((s) => {
          const MarkerIcon = STAGE_ICON[s]
          const active = s === stage
          return (
            <span
              key={s}
              title={active ? `${STAGE_LABEL[s]} (current)` : STAGE_LABEL[s]}
              aria-label={active ? `${STAGE_LABEL[s]} (current stage)` : STAGE_LABEL[s]}
            >
              <MarkerIcon
                size={13}
                className={active ? activeMarkerClass(stage) : 'text-slate-700'}
                aria-hidden="true"
              />
            </span>
          )
        })}
      </div>
    </li>
  )
}

// activeMarkerClass returns the text colour for the current-stage marker icon.
// Colour is a secondary cue only; the icon shape (STAGE_ICON) is what makes the
// stage identifiable for color-vision-deficient users.
function activeMarkerClass(stage: StageKey): string {
  switch (stage) {
    case 'schematic':
      return 'text-purple-400'
    case 'smith':
      return 'text-orange-400'
    case 'temper':
      return 'text-yellow-400'
    case 'warden':
      return 'text-cyan-400'
    case 'pr':
      return 'text-blue-400'
    case 'assay':
      return 'text-pink-400'
    case 'ready_to_merge':
      return 'text-emerald-400'
  }
}
