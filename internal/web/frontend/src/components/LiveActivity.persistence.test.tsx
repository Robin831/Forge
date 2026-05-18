import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import type { EventInfo } from '../api'
import { KEY_PREFIX } from '../hooks/useUIState'

const { useEventSourceMock } = vi.hoisted(() => ({
  useEventSourceMock: vi.fn(),
}))

vi.mock('../hooks/useEventSource', () => ({
  useEventSource: (url: string, opts?: unknown) => useEventSourceMock(url, opts),
}))

import LiveActivity from './LiveActivity'

function setEvents(items: EventInfo[]) {
  useEventSourceMock.mockReturnValue({
    items,
    status: 'open',
    error: null,
    clear: () => {},
  })
}

const TEST_EVENTS: EventInfo[] = [
  {
    id: 1,
    timestamp: '2026-01-01T00:00:00Z',
    type: 'bead_claimed',
    message: 'alpha event',
    bead_id: 'bd-1',
    anvil: 'forge',
  },
  {
    id: 2,
    timestamp: '2026-01-01T00:00:01Z',
    type: 'smith_done',
    message: 'beta event',
    bead_id: 'bd-2',
    anvil: 'heimdall',
  },
  {
    id: 3,
    timestamp: '2026-01-01T00:00:02Z',
    type: 'warden_pass',
    message: 'gamma event',
    bead_id: 'bd-3',
    anvil: 'forge',
  },
]

function BeadStub() {
  const loc = useLocation()
  return <div data-testid="bead-stub">bead detail: {loc.pathname}</div>
}

function renderApp(initialPath = '/') {
  const router = createMemoryRouter(
    [
      { path: '/', element: <LiveActivity /> },
      { path: '/bead/:bead_id', element: <BeadStub /> },
    ],
    { initialEntries: [initialPath] },
  )
  const utils = render(<RouterProvider router={router} />)
  return { ...utils, router }
}

beforeEach(() => {
  setEvents(TEST_EVENTS)
  sessionStorage.clear()
  localStorage.clear()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('LiveActivity persistence', () => {
  it('restores filter and paused state after back-navigation', async () => {
    const user = userEvent.setup()
    const { router } = renderApp()

    // Apply a filter and pause the stream.
    await user.type(screen.getByRole('textbox', { name: /Filter events/ }), 'smith')
    expect(screen.getAllByRole('listitem')).toHaveLength(1)

    await user.click(screen.getByRole('button', { name: /Pause live updates/ }))
    // Once paused the button toggles to "Resume" so users see it's frozen.
    expect(screen.getByRole('button', { name: /Resume live updates/ })).toBeInTheDocument()

    // Navigate away — LiveActivity unmounts. useUIState flushes any debounced
    // writes on unmount so storage is current before the back-nav remount.
    await act(async () => {
      await router.navigate('/bead/bd-1')
    })
    expect(screen.getByTestId('bead-stub')).toBeInTheDocument()

    // Back-navigation remounts LiveActivity. The lazy useState initialiser in
    // useUIState reads storage synchronously so the first paint already
    // reflects the restored filter and paused-snapshot state.
    await act(async () => {
      await router.navigate(-1)
    })

    expect(screen.getByRole('textbox', { name: /Filter events/ })).toHaveValue('smith')
    expect(screen.getByRole('button', { name: /Resume live updates/ })).toBeInTheDocument()
  })

  it('restores filter and paused state after a hard remount from storage', async () => {
    const user = userEvent.setup()
    const first = renderApp()

    await user.type(screen.getByRole('textbox', { name: /Filter events/ }), 'warden')
    await user.click(screen.getByRole('button', { name: /Pause live updates/ }))

    // Tear the whole tree down — what a hard refresh looks like to React.
    // localStorage survives so the fresh mount rehydrates from it.
    first.unmount()

    renderApp()

    expect(screen.getByRole('textbox', { name: /Filter events/ })).toHaveValue('warden')
    expect(screen.getByRole('button', { name: /Resume live updates/ })).toBeInTheDocument()
  })

  it('hydrates from seeded storage on first paint with no default-state flash', () => {
    // Pre-seed localStorage as if a previous session wrote these values, then
    // mount LiveActivity. The first synchronous render must already reflect
    // them — any post-mount effect would necessarily come after the first
    // paint and show as a flash.
    localStorage.setItem(
      `${KEY_PREFIX}root.liveActivity.filter`,
      JSON.stringify('alpha'),
    )
    localStorage.setItem(
      `${KEY_PREFIX}root.liveActivity.paused`,
      JSON.stringify(true),
    )

    renderApp()

    expect(screen.getByRole('textbox', { name: /Filter events/ })).toHaveValue('alpha')
    expect(screen.getByRole('button', { name: /Resume live updates/ })).toBeInTheDocument()
    // Filter matches only the alpha event — one row visible on first paint.
    expect(screen.getAllByRole('listitem')).toHaveLength(1)
  })

  it('captures scroll position and restores it after back-navigation', async () => {
    // Use fake timers to advance through rAF (shimmed as setTimeout in jsdom)
    // and the 150ms useUIState debounce without real-time waits.
    vi.useFakeTimers()

    const { router } = renderApp()

    // Locate the Pane's scrollable body (the div with role="region").
    const scrollBody = document.querySelector('div[role="region"]') as HTMLElement
    expect(scrollBody).not.toBeNull()

    // jsdom has no layout engine so scrollTop always reads 0. Override it on
    // this element so the onScroll handler sees a non-zero value.
    let _scrollTop = 150
    Object.defineProperty(scrollBody, 'scrollTop', {
      configurable: true,
      get: () => _scrollTop,
      set: (v: number) => {
        _scrollTop = v
      },
    })

    scrollBody.dispatchEvent(new Event('scroll'))

    await act(async () => {
      vi.runAllTimers()
    })

    await act(async () => {
      await router.navigate('/bead/bd-1')
    })
    expect(screen.getByTestId('bead-stub')).toBeInTheDocument()

    // Spy on scrollTop writes during the back-navigation remount to verify
    // that useLayoutEffect restores the saved position before paint.
    const scrollTopWrites: number[] = []
    const origDesc = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollTop')
    Object.defineProperty(Element.prototype, 'scrollTop', {
      configurable: true,
      get: origDesc?.get ?? (() => 0),
      set(v: number) {
        scrollTopWrites.push(v)
        origDesc?.set?.call(this, v)
      },
    })

    try {
      await act(async () => {
        await router.navigate(-1)
      })
    } finally {
      if (origDesc) {
        Object.defineProperty(Element.prototype, 'scrollTop', origDesc)
      } else {
        delete (Element.prototype as unknown as Record<string, unknown>).scrollTop
      }
    }

    expect(scrollTopWrites).toContain(150)
  })
})
