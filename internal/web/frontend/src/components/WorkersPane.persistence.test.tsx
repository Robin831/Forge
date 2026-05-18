import '@testing-library/jest-dom/vitest'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import WorkersPane from './WorkersPane'
import type { WorkerInfo } from '../api'
import { KEY_PREFIX } from '../hooks/useUIState'

function worker(overrides: Partial<WorkerInfo>): WorkerInfo {
  return {
    id: 'w-1',
    bead_id: 'bd-1',
    anvil: 'forge',
    status: 'running',
    started_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

function WorkersRoot({ workers }: { workers: WorkerInfo[] }) {
  return <WorkersPane workers={workers} loading={false} error={null} />
}

function BeadStub() {
  const loc = useLocation()
  return (
    <div data-testid="bead-stub">
      bead detail: {loc.pathname}
    </div>
  )
}

function renderApp(workers: WorkerInfo[], initialPath = '/') {
  const router = createMemoryRouter(
    [
      { path: '/', element: <WorkersRoot workers={workers} /> },
      { path: '/bead/:bead_id', element: <BeadStub /> },
    ],
    { initialEntries: [initialPath] },
  )
  const utils = render(<RouterProvider router={router} />)
  return { ...utils, router }
}

const TEST_WORKERS: WorkerInfo[] = [
  worker({
    id: 'w-1',
    bead_id: 'bd-1',
    anvil: 'forge',
    status: 'running',
    started_at: '2024-01-02T00:00:00Z',
    title: 'Alpha bead',
  }),
  worker({
    id: 'w-2',
    bead_id: 'bd-2',
    anvil: 'heimdall',
    status: 'done',
    started_at: '2024-01-01T00:00:00Z',
    title: 'Beta bead',
  }),
]

beforeEach(() => {
  sessionStorage.clear()
  localStorage.clear()
})

describe('WorkersPane persistence', () => {
  it('restores group collapsed state after back-navigation', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(TEST_WORKERS)

    // Both groups start expanded by default (no stored state).
    const forgeHeader = screen.getByTestId('workers-group-forge')
    expect(forgeHeader).toHaveAttribute('aria-expanded', 'true')

    // Collapse the forge group.
    await user.click(forgeHeader)
    expect(forgeHeader).toHaveAttribute('aria-expanded', 'false')

    // Navigate away — WorkersPane unmounts. useUIState's unmount cleanup
    // flushes any debounced writes so storage is guaranteed current before
    // the back-nav remount reads from it.
    await act(async () => {
      await router.navigate('/bead/bd-1')
    })
    expect(screen.getByTestId('bead-stub')).toBeInTheDocument()

    // Back-navigation remounts WorkersPane. useUIState reads storage
    // synchronously inside its lazy useState initialiser, so the very first
    // paint already carries restored values.
    await act(async () => {
      await router.navigate(-1)
    })

    expect(screen.getByTestId('workers-group-forge')).toHaveAttribute('aria-expanded', 'false')
    // heimdall was never toggled so it stays expanded.
    expect(screen.getByTestId('workers-group-heimdall')).toHaveAttribute(
      'aria-expanded',
      'true',
    )
  })

  it('restores collapsed state after a remount (refresh) from persisted storage', async () => {
    const user = userEvent.setup()
    const first = renderApp(TEST_WORKERS)

    // Collapse the heimdall group.
    await user.click(screen.getByTestId('workers-group-heimdall'))
    expect(screen.getByTestId('workers-group-heimdall')).toHaveAttribute(
      'aria-expanded',
      'false',
    )

    // Tear the entire tree down — what a hard refresh looks like to React.
    // localStorage survives the unmount so the fresh mount rehydrates from it.
    first.unmount()

    renderApp(TEST_WORKERS)

    expect(screen.getByTestId('workers-group-heimdall')).toHaveAttribute(
      'aria-expanded',
      'false',
    )
    expect(screen.getByTestId('workers-group-forge')).toHaveAttribute(
      'aria-expanded',
      'true',
    )
  })

  it('hydrates from seeded storage on first paint with no default-state flash', () => {
    // Pre-seed localStorage as if a previous session wrote this value, then
    // mount WorkersPane. The first synchronous render must already reflect it —
    // any post-mount effect would necessarily come after the first paint.
    localStorage.setItem(
      `${KEY_PREFIX}root.workers-pane.group-collapsed`,
      JSON.stringify({ heimdall: true }),
    )

    renderApp(TEST_WORKERS)

    expect(screen.getByTestId('workers-group-heimdall')).toHaveAttribute(
      'aria-expanded',
      'false',
    )
    expect(screen.getByTestId('workers-group-forge')).toHaveAttribute(
      'aria-expanded',
      'true',
    )
  })

  it('captures scroll position and restores it after back-navigation', async () => {
    // Use fake timers so we can advance through rAF (shimmed as setTimeout in
    // jsdom) and the 150ms useUIState debounce without real-time waits.
    vi.useFakeTimers()

    const { router } = renderApp(TEST_WORKERS)

    // Locate the Pane's scrollable body element (the div with role="region").
    const scrollBody = document.querySelector('div[role="region"]') as HTMLElement
    expect(scrollBody).not.toBeNull()

    // jsdom has no layout engine so el.scrollTop always reads 0. Override the
    // property on this specific element so the onScroll handler reads 200.
    let _scrollTop = 200
    Object.defineProperty(scrollBody, 'scrollTop', {
      configurable: true,
      get: () => _scrollTop,
      set: (v: number) => {
        _scrollTop = v
      },
    })

    // Dispatch the scroll event — triggers the rAF-throttled onScroll handler.
    scrollBody.dispatchEvent(new Event('scroll'))

    // Flush rAF (shimmed as setTimeout(0) in jsdom) and all queued timers,
    // including the 150ms debounce that runs inside the rAF callback.
    await act(async () => {
      vi.runAllTimers()
    })

    // Navigate away — WorkersPane unmounts, debounce timer already fired so
    // sessionStorage already has the captured scroll position.
    await act(async () => {
      await router.navigate('/bead/bd-1')
    })
    expect(screen.getByTestId('bead-stub')).toBeInTheDocument()

    // Spy on scrollTop writes during the back-navigation remount to verify
    // that useLayoutEffect calls el.scrollTop = 200 before the browser paints.
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
      }
      vi.useRealTimers()
    }

    expect(scrollTopWrites).toContain(200)
  })
})
