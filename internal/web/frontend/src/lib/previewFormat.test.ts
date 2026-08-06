import { describe, expect, it } from 'vitest'
import { formatCountdown, formatDuration } from './previewFormat'

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
