import { AlertOctagon } from 'lucide-react'
import type { WedgedAnvil } from '../api'
import { relativeTime } from '../lib/format'

// WedgedAnvilsBanner surfaces anvils whose beads (Dolt) database is mid-merge
// with unresolved conflicts. While an anvil is wedged every bd write against it
// is rolled back, so nothing dispatched there can succeed — the daemon skips
// those beads entirely and refuses manual dispatch.
//
// This is the browser counterpart of the Hearth TUI's anvil-kind needs-attention
// entry: the needs-attention list is bead-centric (driven by the retries table)
// and a wedge belongs to no bead, so it is rendered as a dashboard-level banner
// fed by the status payload's wedged_anvils. Like the TUI entry it offers no
// actions — resolving a merge conflict is a semantic decision that belongs to
// the operator, and the daemon clears the banner itself once dolt_conflicts is
// empty.
// detectedLabel renders "detected <relative>" for a real timestamp. Go marshals
// a zero time.Time as year 0001 (the `omitempty` tag does not apply to structs),
// so that value is treated as "unknown" rather than rendered as a millennia-old
// wedge.
function detectedLabel(detectedAt?: string): string {
  if (!detectedAt || detectedAt.startsWith('0001-01-01')) return ''
  const rel = relativeTime(detectedAt)
  return rel ? `detected ${rel}` : ''
}

export default function WedgedAnvilsBanner({
  anvils,
}: {
  anvils: WedgedAnvil[]
}) {
  if (anvils.length === 0) return null

  return (
    <section
      aria-label="Wedged anvils"
      className="rounded-lg border border-red-500/40 bg-red-500/10 p-4"
    >
      <div className="flex items-start gap-3">
        <AlertOctagon size={18} className="mt-0.5 shrink-0 text-red-400" aria-hidden />
        <div className="min-w-0 flex-1">
          <h2 className="text-sm font-semibold text-red-200">
            {anvils.length === 1
              ? '1 anvil is wedged'
              : `${anvils.length} anvils are wedged`}{' '}
            — no work can be dispatched there
          </h2>
          <ul className="mt-2 space-y-2">
            {anvils.map((a) => (
              <li key={a.anvil} className="text-xs text-red-100/90">
                <span className="font-mono font-semibold">{a.anvil}</span>
                {a.conflict_tables ? (
                  <> — conflicts: {a.conflict_tables}</>
                ) : null}
                {a.divergence_known ? (
                  <>
                    {' '}
                    ({a.branch || 'local'} ahead {a.ahead ?? 0} / behind{' '}
                    {a.behind ?? 0})
                  </>
                ) : null}
                {detectedLabel(a.detected_at) ? (
                  <> · {detectedLabel(a.detected_at)}</>
                ) : null}
                {a.detail ? (
                  <p className="mt-1 text-red-100/70">{a.detail}</p>
                ) : null}
              </li>
            ))}
          </ul>
          <p className="mt-2 text-xs text-red-100/70">
            Resolve the conflicts in the beads database — there is nothing to
            dismiss here, this banner clears itself on the next poll once{' '}
            <span className="font-mono">dolt_conflicts</span> is empty.
          </p>
        </div>
      </div>
    </section>
  )
}
