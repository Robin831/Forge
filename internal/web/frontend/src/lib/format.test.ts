import { describe, expect, it } from 'vitest'

import { eventClasses } from './format'

// eventClasses is the web mirror of the Hearth TUI's eventTypeColor, and both
// are order-sensitive for the same reason: 'partial' has to match before the
// 'fail' and 'complete' rules, or assay_partial falls through to the neutral
// default and reads like an ordinary informational row. The TUI side pins that
// in internal/hearth/assay_event_style_test.go; this is the same pin on this
// side, so a later reorder recolours the feed loudly rather than silently.
describe('eventClasses', () => {
  it('colours the three terminal Assay events by outcome', () => {
    expect(eventClasses('assay_partial')).toBe('text-amber-300')
    expect(eventClasses('assay_completed')).toBe('text-emerald-300')
    expect(eventClasses('assay_failed')).toBe('text-red-300')
  })

  it('leaves neighbouring event types unchanged', () => {
    expect(eventClasses('warden_pass')).toBe('text-emerald-300')
    expect(eventClasses('pr_merged')).toBe('text-emerald-300')
    expect(eventClasses('smith_failed')).toBe('text-red-300')
    expect(eventClasses('pr_created')).toBe('text-purple-300')
    expect(eventClasses('bead_paused')).toBe('text-amber-300')
    expect(eventClasses('bead_resumed')).toBe('text-emerald-300')
    expect(eventClasses('bead_claimed')).toBe('text-sky-300')
    expect(eventClasses('poll')).toBe('text-slate-300')
  })
})
