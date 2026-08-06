// Duration formatting for the Kiln preview surfaces: how long a service has
// been up, and how long a preview has left before the idle reaper takes it.
//
// It is deliberately separate from lib/format.ts's relativeTime: that renders a
// *past* instant as prose ("3m ago") and rounds hard, which reads wrong for a
// countdown — an operator watching a preview expire wants the seconds.

const MINUTE = 60
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

function pad(n: number): string {
  return n < 10 ? `0${n}` : String(n)
}

// formatDuration renders a whole number of seconds as a compact two-unit
// duration: `45s`, `3m 20s`, `2h 05m`, `1d 3h`. Two units is the point — the
// seconds matter while a preview is minutes from expiry and stop mattering once
// it is hours away.
//
// Anything that is not a positive finite number renders as `0s`, so a missing
// or negative uptime from the daemon degrades to a number rather than `NaNs`.
export function formatDuration(totalSeconds: number): string {
  if (!Number.isFinite(totalSeconds) || totalSeconds <= 0) return '0s'
  const secs = Math.floor(totalSeconds)
  if (secs < MINUTE) return `${secs}s`
  if (secs < HOUR) return `${Math.floor(secs / MINUTE)}m ${pad(secs % MINUTE)}s`
  if (secs < DAY) return `${Math.floor(secs / HOUR)}h ${pad(Math.floor((secs % HOUR) / MINUTE))}m`
  return `${Math.floor(secs / DAY)}d ${Math.floor((secs % DAY) / HOUR)}h`
}

// formatCountdown renders the time left until an ISO-8601 deadline.
//
// Returns null when there is no deadline to count down to — the daemon sends
// `idle_deadline: null` when the idle reaper is disabled, and callers render
// nothing at all rather than a fake "never". A deadline already in the past
// reads as `due now`: the reaper runs on its own ticker, so the moment past the
// deadline is "any tick now", not "expired an hour ago".
//
// The remaining time is rounded *up* so a countdown never shows `0s` while the
// preview is still alive.
export function formatCountdown(
  deadline: string | null | undefined,
  now: number = Date.now(),
): string | null {
  if (!deadline) return null
  const at = Date.parse(deadline)
  if (Number.isNaN(at)) return null
  const remainingMs = at - now
  if (remainingMs <= 0) return 'due now'
  return formatDuration(Math.ceil(remainingMs / 1000))
}
