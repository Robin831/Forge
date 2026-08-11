// Formats an ISO-8601 timestamp as a relative "5s ago" / "2m ago" / "3h ago"
// string. Falls back to the original string when the input is unparseable.
export function relativeTime(input?: string | null): string {
  if (!input) return ''
  const t = Date.parse(input)
  if (Number.isNaN(t)) return input
  const diffMs = Date.now() - t
  if (diffMs < 0) return 'just now'
  const seconds = Math.floor(diffMs / 1000)
  if (seconds < 5) return 'just now'
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  if (days < 7) return `${days}d ago`
  return new Date(t).toLocaleDateString()
}

const PRIORITY_LABEL: Record<number, string> = {
  0: 'P0',
  1: 'P1',
  2: 'P2',
  3: 'P3',
  4: 'P4',
}

export function priorityLabel(p: number): string {
  return PRIORITY_LABEL[p] ?? `P${p}`
}

export function priorityClasses(p: number): string {
  switch (p) {
    case 0:
      return 'bg-red-500/20 text-red-300 border-red-500/40'
    case 1:
      return 'bg-orange-500/20 text-orange-300 border-orange-500/40'
    case 2:
      return 'bg-amber-500/20 text-amber-300 border-amber-500/40'
    case 3:
      return 'bg-sky-500/20 text-sky-300 border-sky-500/40'
    default:
      return 'bg-slate-700/60 text-slate-300 border-slate-600/60'
  }
}

export function eventClasses(type: string): string {
  // Partial first: assay_partial is neither a success nor a failure — some
  // passes reviewed the head and some never did — and every rule below would
  // leave it in the neutral default, reading like an informational row.
  if (type.includes('partial')) {
    return 'text-amber-300'
  }
  if (type.includes('fail') || type.includes('error') || type.includes('stuck')) {
    return 'text-red-300'
  }
  // 'complete' rather than 'completed' so crucible_complete lands here too —
  // the same substring the TUI's eventTypeColor matches, so the two surfaces
  // cannot colour one event type differently. assay_completed matches either
  // way; crucible_complete only matches this one.
  if (
    type.includes('warden_pass') ||
    type.includes('pr_merged') ||
    type.includes('done') ||
    type.includes('complete')
  ) {
    return 'text-emerald-300'
  }
  if (type.includes('pr_created')) {
    return 'text-purple-300'
  }
  // bead_paused reads as a deliberate hold — amber, matching the paused status
  // chip; bead_resumed is a "back in motion" signal — emerald.
  if (type.includes('resumed')) {
    return 'text-emerald-300'
  }
  if (type.includes('paused')) {
    return 'text-amber-300'
  }
  if (type.includes('claimed') || type.includes('start')) {
    return 'text-sky-300'
  }
  return 'text-slate-300'
}
