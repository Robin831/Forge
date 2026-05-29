import { useMemo } from 'react'
import { ChevronRight } from 'lucide-react'
import type { WorkerInfo } from '../api'

// StageKey labels the six visible columns of the pipeline bar. The Bellows
// PR-monitor is intentionally folded into the "pr" stage as a sub-label
// rather than getting its own column — see the per-bead description.
// "ready_to_merge" sits after the PR stage and counts PRs the daemon has
// promoted (CI green, no pending reviews, no unresolved threads, not
// conflicting) so an operator can see what's waiting on a human merge click.
type StageKey = 'schematic' | 'smith' | 'temper' | 'warden' | 'pr' | 'ready_to_merge'

const STAGES: StageKey[] = ['schematic', 'smith', 'temper', 'warden', 'pr', 'ready_to_merge']

const STAGE_LABEL: Record<StageKey, string> = {
  schematic: 'Schematic',
  smith: 'Smith',
  temper: 'Temper',
  warden: 'Warden',
  pr: 'PR',
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
  ready_to_merge: 'border-t-emerald-500',
}

// phaseToStage projects a backend worker phase string onto one of the six
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
// importance: rebase > ci-fix > review-fix > monitoring.
function prSubLabel(prWorkers: WorkerInfo[]): string | null {
  if (prWorkers.length === 0) return null
  const hasPhase = (p: string) => prWorkers.some((w) => w.phase === p)
  if (hasPhase('rebase')) return 'rebase'
  if (hasPhase('quench') || hasPhase('cifix')) return 'ci-fix'
  if (hasPhase('burnish') || hasPhase('reviewfix')) return 'review-fix'
  if (hasPhase('bellows')) return 'monitoring'
  return null
}

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
    if (
      w.status !== 'pending' &&
      w.status !== 'running' &&
      w.status !== 'monitoring' &&
      w.status !== 'reviewing'
    )
      continue
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
      if (
        w.status !== 'pending' &&
        w.status !== 'running' &&
        w.status !== 'monitoring' &&
        w.status !== 'reviewing'
      )
        continue
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
      <div className="flex items-center justify-between px-2.5 py-1.5">
        <span className={labelClass}>{STAGE_LABEL[stage.key]}</span>
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
      <div className="flex items-center gap-1">
        {STAGES.map((s) => (
          <span
            key={s}
            aria-label={STAGE_LABEL[s]}
            className={`h-1.5 w-4 rounded-sm ${
              s === stage
                ? activeMarkerClass(stage)
                : 'bg-slate-800'
            }`}
          />
        ))}
      </div>
    </li>
  )
}

function activeMarkerClass(stage: StageKey): string {
  switch (stage) {
    case 'schematic':
      return 'bg-purple-400'
    case 'smith':
      return 'bg-orange-400'
    case 'temper':
      return 'bg-yellow-400'
    case 'warden':
      return 'bg-cyan-400'
    case 'pr':
      return 'bg-blue-400'
    case 'ready_to_merge':
      return 'bg-emerald-400'
  }
}
