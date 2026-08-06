import { describe, expect, it } from 'vitest'
import { formatCountdown, formatDuration, previewIdleCountdown } from './previewFormat'

describe('formatDuration', () => {
  it('renders sub-minute durations in whole seconds', () => {
    expect(formatDuration(0)).toBe('0s')
    expect(formatDuration(1)).toBe('1s')
    expect(formatDuration(59)).toBe('59s')
  })

  it('renders minutes with zero-padded seconds', () => {
    expect(formatDuration(60)).toBe('1m 00s')
    expect(formatDuration(65)).toBe('1m 05s')
    expect(formatDuration(200)).toBe('3m 20s')
    expect(formatDuration(3599)).toBe('59m 59s')
  })

  it('drops to hours + minutes past an hour', () => {
    expect(formatDuration(3600)).toBe('1h 00m')
    expect(formatDuration(7500)).toBe('2h 05m')
    expect(formatDuration(86399)).toBe('23h 59m')
  })

  it('drops to days + hours past a day', () => {
    expect(formatDuration(86400)).toBe('1d 0h')
    expect(formatDuration(97200)).toBe('1d 3h')
  })

  it('truncates fractional seconds rather than rendering a decimal', () => {
    expect(formatDuration(90.9)).toBe('1m 30s')
  })

  it('degrades a missing or nonsensical duration to 0s', () => {
    expect(formatDuration(-5)).toBe('0s')
    expect(formatDuration(Number.NaN)).toBe('0s')
    expect(formatDuration(Number.POSITIVE_INFINITY)).toBe('0s')
  })
})

describe('formatCountdown', () => {
  const now = Date.parse('2026-08-06T12:00:00Z')

  it('returns null when the reaper is disabled (no deadline)', () => {
    expect(formatCountdown(null, now)).toBeNull()
    expect(formatCountdown(undefined, now)).toBeNull()
    expect(formatCountdown('', now)).toBeNull()
  })

  it('returns null for an unparseable deadline rather than NaN', () => {
    expect(formatCountdown('not-a-timestamp', now)).toBeNull()
  })

  it('counts down to the deadline', () => {
    expect(formatCountdown('2026-08-06T12:00:45Z', now)).toBe('45s')
    expect(formatCountdown('2026-08-06T12:08:12Z', now)).toBe('8m 12s')
    expect(formatCountdown('2026-08-06T14:30:00Z', now)).toBe('2h 30m')
  })

  it('rounds up so a live preview never reads 0s', () => {
    expect(formatCountdown('2026-08-06T12:00:00.400Z', now)).toBe('1s')
  })

  it('reads a passed deadline as due now, not as elapsed time', () => {
    expect(formatCountdown('2026-08-06T12:00:00Z', now)).toBe('due now')
    expect(formatCountdown('2026-08-06T11:00:00Z', now)).toBe('due now')
  })
})

describe('previewIdleCountdown', () => {
  const fetchedAt = Date.parse('2026-08-06T12:00:00Z')

  it('returns null when there is no preview to count down', () => {
    expect(previewIdleCountdown(null, fetchedAt, fetchedAt)).toBeNull()
    expect(previewIdleCountdown(undefined, fetchedAt, fetchedAt)).toBeNull()
  })

  it('returns null when the reaper is disabled — no seconds and no deadline', () => {
    expect(
      previewIdleCountdown(
        { idle_remaining_seconds: null, idle_deadline: null },
        fetchedAt,
        fetchedAt,
      ),
    ).toBeNull()
    expect(previewIdleCountdown({}, fetchedAt, fetchedAt)).toBeNull()
  })

  it("renders the daemon's seconds remaining at the moment they were fetched", () => {
    expect(previewIdleCountdown({ idle_remaining_seconds: 492 }, fetchedAt, fetchedAt)).toBe(
      '8m 12s',
    )
  })

  it('ages the fetched value by the time elapsed since, so it ticks between polls', () => {
    expect(previewIdleCountdown({ idle_remaining_seconds: 492 }, fetchedAt, fetchedAt + 3_000)).toBe(
      '8m 09s',
    )
    expect(
      previewIdleCountdown({ idle_remaining_seconds: 492 }, fetchedAt, fetchedAt + 60_000),
    ).toBe('7m 12s')
  })

  it('clamps at due now rather than counting past the deadline', () => {
    expect(previewIdleCountdown({ idle_remaining_seconds: 0 }, fetchedAt, fetchedAt)).toBe('due now')
    expect(
      previewIdleCountdown({ idle_remaining_seconds: 30 }, fetchedAt, fetchedAt + 120_000),
    ).toBe('due now')
  })

  it('ignores an unset or future fetch anchor instead of ageing by nonsense', () => {
    // Nothing fetched yet, and a clock that ran backwards: both contribute no
    // elapsed time rather than inflating or shrinking the countdown.
    expect(previewIdleCountdown({ idle_remaining_seconds: 60 }, 0, fetchedAt)).toBe('1m 00s')
    expect(previewIdleCountdown({ idle_remaining_seconds: 60 }, fetchedAt, fetchedAt - 5_000)).toBe(
      '1m 00s',
    )
  })

  it('falls back to the absolute deadline when the daemon sends no seconds', () => {
    expect(
      previewIdleCountdown(
        { idle_remaining_seconds: null, idle_deadline: '2026-08-06T12:08:12Z' },
        fetchedAt,
        fetchedAt,
      ),
    ).toBe('8m 12s')
  })

  it('prefers the seconds remaining over a deadline that disagrees', () => {
    // A browser clock an hour behind the daemon's would read the deadline as
    // an hour further out; the relative value is immune to that.
    expect(
      previewIdleCountdown(
        { idle_remaining_seconds: 60, idle_deadline: '2026-08-06T13:00:00Z' },
        fetchedAt,
        fetchedAt,
      ),
    ).toBe('1m 00s')
  })
})
