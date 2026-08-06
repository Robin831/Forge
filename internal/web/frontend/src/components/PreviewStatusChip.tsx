import { AlertTriangle, CircleCheck, Loader2, MonitorOff, XCircle } from 'lucide-react'
import type { PreviewStatus } from '../api/previews'

// CHIP describes each preview state once: the label the operator reads, the
// tint, and the icon. Keyed by PreviewStatus so adding a state to the machine
// forces an entry here rather than silently rendering nothing.
const CHIP: Record<
  PreviewStatus,
  { label: string; classes: string; Icon: typeof CircleCheck; spin?: boolean }
> = {
  idle: {
    label: 'No preview',
    classes: 'border-slate-700 bg-slate-800/60 text-slate-300',
    Icon: MonitorOff,
  },
  starting: {
    label: 'Starting…',
    classes: 'border-sky-500/40 bg-sky-500/10 text-sky-300',
    Icon: Loader2,
    spin: true,
  },
  healthy: {
    label: 'Healthy',
    classes: 'border-emerald-500/40 bg-emerald-500/10 text-emerald-300',
    Icon: CircleCheck,
  },
  degraded: {
    label: 'Degraded',
    classes: 'border-amber-500/40 bg-amber-500/10 text-amber-300',
    Icon: AlertTriangle,
  },
  failed: {
    label: 'Failed',
    classes: 'border-red-500/40 bg-red-500/10 text-red-300',
    Icon: XCircle,
  },
  stopping: {
    label: 'Stopping…',
    classes: 'border-slate-600 bg-slate-800/60 text-slate-300',
    Icon: Loader2,
    spin: true,
  },
}

export interface PreviewStatusChipProps {
  status: PreviewStatus
  /** Failure detail, surfaced as the chip's tooltip and accessible label. */
  error?: string | null
  /** Test hook so a caller can address the chip of a specific bead. */
  testId?: string
  className?: string
}

// PreviewStatusChip renders one preview state as a compact badge. It is purely
// presentational — the state machine lives in usePreview — so the bead-detail
// panel and the previews overview can render the same badge from their own
// data without going through PreviewButton.
export default function PreviewStatusChip({
  status,
  error,
  testId,
  className = '',
}: PreviewStatusChipProps) {
  const { label, classes, Icon, spin } = CHIP[status]
  const title = error ? `${label} — ${error}` : label
  return (
    <span
      data-testid={testId}
      data-status={status}
      title={title}
      aria-label={`Preview ${title}`}
      className={`inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs ${classes} ${className}`}
    >
      <Icon size={12} aria-hidden className={spin ? 'animate-spin' : undefined} />
      <span>{label}</span>
    </span>
  )
}
