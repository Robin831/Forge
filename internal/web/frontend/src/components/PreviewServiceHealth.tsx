import { CircleCheck, Loader2, XCircle } from 'lucide-react'
import type { PreviewServiceHealth } from '../api/previews'

interface HealthStyle {
  classes: string
  Icon: typeof CircleCheck
  spin?: boolean
}

const HEALTH: Record<string, HealthStyle> = {
  healthy: {
    classes: 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300',
    Icon: CircleCheck,
  },
  starting: {
    classes: 'border-sky-500/40 bg-sky-500/10 text-sky-300',
    Icon: Loader2,
    spin: true,
  },
  failed: {
    classes: 'border-red-500/40 bg-red-500/10 text-red-300',
    Icon: XCircle,
  },
}

const UNKNOWN: HealthStyle = {
  classes: 'border-slate-700 bg-slate-800/60 text-slate-300',
  Icon: Loader2,
}

export interface PreviewServiceHealthBadgeProps {
  health: PreviewServiceHealth
  /** Failure detail, surfaced as the badge's tooltip. */
  error?: string
  testId?: string
}

// PreviewServiceHealthBadge renders one service's health. It is the per-service
// counterpart to PreviewStatusChip (which renders the preview as a whole) and
// is shared by the bead-detail panel and the previews overview so a service
// never looks healthy on one page and starting on the other.
//
// An unrecognised health value renders with its own label rather than being
// folded into one of the three known states — the honest reading of a value
// this client does not know is the value itself.
export default function PreviewServiceHealthBadge({
  health,
  error,
  testId,
}: PreviewServiceHealthBadgeProps) {
  const { classes, Icon, spin } = HEALTH[health] ?? UNKNOWN
  return (
    <span
      data-testid={testId}
      data-health={health}
      title={error ? `${health} — ${error}` : health}
      className={`inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${classes}`}
    >
      <Icon size={11} aria-hidden className={spin ? 'animate-spin' : undefined} />
      {health}
    </span>
  )
}
