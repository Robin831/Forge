import { act } from '@testing-library/react'
import { vi } from 'vitest'

/**
 * Capture a pane's scroll position with fake timers installed, then hand the
 * clock straight back to real timers.
 *
 * `dispatchScroll` is expected to fire the scroll event the pane listens for.
 * The handler is rAF-throttled (rAF is setTimeout-shimmed in jsdom) and writes
 * through useUIState's 150ms debounce, so a frozen clock plus `runAllTimers()`
 * is the only way to settle it without real-time waits.
 *
 * The important half is the `finally`: fake timers must not stay installed
 * across a router navigation. React's async `act()` and react-router's
 * `navigate()` both yield to the macrotask queue, and while the clock is frozen
 * nothing advances that queue from inside the await — the navigation never
 * settles and the test dies on its 5s timeout with no useful failure message.
 * That is the shape reported by Forge-admm, Forge-t9ay and Forge-g82p in turn.
 * Scoping the fake clock to the one flush that needs it makes the whole class
 * impossible instead of re-fixing it per test.
 */
export async function captureScrollWithFakeTimers(dispatchScroll: () => void) {
  vi.useFakeTimers()
  try {
    dispatchScroll()
    await act(async () => {
      vi.runAllTimers()
    })
  } finally {
    vi.useRealTimers()
  }
}
