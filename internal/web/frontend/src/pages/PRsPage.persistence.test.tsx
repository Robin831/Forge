import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import type { PRItem } from '../api'
import { KEY_PREFIX } from '../hooks/useUIState'

const { useApiPollMock, usePRsDataMock } = vi.hoisted(() => ({
  useApiPollMock: vi.fn(),
  usePRsDataMock: vi.fn(),
}))

vi.mock('../hooks/useApiPoll', () => ({
  useApiPoll: (path: string, intervalMs?: number) => useApiPollMock(path, intervalMs),
}))

vi.mock('./usePRsData', () => ({
  usePRsData: () => usePRsDataMock(),
  PRS_CACHE_TTL_MS: 60_000,
}))

import PRsPage from './PRsPage'

function pr(overrides: Partial<PRItem>): PRItem {
  return {
    id: 1,
    number: 1,
    anvil: 'forge',
    status: 'open',
    is_external: false,
    title: 'Example PR',
    ...overrides,
  }
}

function BeadStub() {
  const loc = useLocation()
  return <div data-testid="bead-stub">bead detail: {loc.pathname}</div>
}

function renderApp(initialPath = '/prs') {
  const router = createMemoryRouter(
    [
      { path: '/prs', element: <PRsPage /> },
      { path: '/bead/:bead_id', element: <BeadStub /> },
    ],
    { initialEntries: [initialPath] },
  )
  const utils = render(<RouterProvider router={router} />)
  return { ...utils, router }
}

const TEST_FORGE_PRS: PRItem[] = [
  pr({
    id: 1,
    number: 101,
    anvil: 'forge',
    title: 'alpha forge pr',
    bead_id: 'Forge-aaaa',
    updated_at: '2026-01-02T00:00:00Z',
    created_at: '2026-01-01T00:00:00Z',
  }),
  pr({
    id: 2,
    number: 102,
    anvil: 'forge',
    title: 'beta forge pr',
    bead_id: 'Forge-bbbb',
    updated_at: '2026-01-03T00:00:00Z',
    created_at: '2026-01-01T00:00:00Z',
  }),
]

const TEST_EXTERNAL_PRS: PRItem[] = [
  pr({
    id: 3,
    number: 201,
    anvil: 'heimdall',
    title: 'gamma external pr',
    is_external: true,
    bead_id: 'ext-1',
    updated_at: '2026-01-04T00:00:00Z',
  }),
]

beforeEach(() => {
  sessionStorage.clear()
  localStorage.clear()
  useApiPollMock.mockReturnValue({
    data: { running: true, pid: 1 },
    error: null,
    loading: false,
  })
  usePRsDataMock.mockReturnValue({
    forge_prs: TEST_FORGE_PRS,
    external_prs: TEST_EXTERNAL_PRS,
    recently_merged: [],
    loading: false,
    error: null,
    fetchedAt: Date.now(),
    refresh: vi.fn(),
  })
})

afterEach(() => {
  vi.useRealTimers()
})

describe('PRsPage persistence', () => {
  it('restores filter, sort, and section-expanded state after back-navigation', async () => {
    const user = userEvent.setup()
    const { router } = renderApp()

    // Filter to "alpha" — narrows Forge PRs to just one row.
    const filterInput = screen.getByRole('textbox', { name: /Filter PRs/ })
    await user.type(filterInput, 'alpha')
    expect(filterInput).toHaveValue('alpha')

    // Pick a non-default sort.
    await user.selectOptions(screen.getByTestId('prs-sort-select'), 'title-asc')

    // Collapse the External PRs section. Sections start expanded — clicking
    // toggles them to false and writes `false` to localStorage, which is what
    // we need to distinguish "never touched" from "deliberately closed".
    const externalToggle = screen.getByRole('button', { name: /External PRs/ })
    expect(externalToggle).toHaveAttribute('aria-expanded', 'true')
    await user.click(externalToggle)
    expect(externalToggle).toHaveAttribute('aria-expanded', 'false')

    // Navigate to a bead detail. The routes are siblings, so PRsPage fully
    // unmounts; useUIState's unmount cleanup flushes debounced writes.
    await act(async () => {
      await router.navigate('/bead/Forge-aaaa')
    })
    expect(screen.getByTestId('bead-stub')).toBeInTheDocument()

    // Back-navigation remounts PRsPage. useUIState reads storage synchronously
    // inside its lazy useState initialiser, so the very first paint after pop
    // already carries restored values — no default-state flicker.
    await act(async () => {
      await router.navigate(-1)
    })

    expect(screen.getByRole('textbox', { name: /Filter PRs/ })).toHaveValue('alpha')
    expect(screen.getByTestId('prs-sort-select')).toHaveValue('title-asc')
    expect(screen.getByRole('button', { name: /External PRs/ })).toHaveAttribute(
      'aria-expanded',
      'false',
    )
    // Sections we didn't touch stay expanded.
    expect(screen.getByRole('button', { name: /Forge PRs/ })).toHaveAttribute(
      'aria-expanded',
      'true',
    )
  })

  it('restores state after a remount (refresh) from persisted storage', async () => {
    const user = userEvent.setup()
    const first = renderApp()

    await user.selectOptions(screen.getByTestId('prs-sort-select'), 'number-asc')
    await user.click(screen.getByRole('button', { name: /Recently merged/ }))
    expect(screen.getByRole('button', { name: /Recently merged/ })).toHaveAttribute(
      'aria-expanded',
      'false',
    )

    // Tear the entire tree down — what a hard refresh looks like to React.
    // localStorage survives the unmount so the fresh mount rehydrates from it.
    first.unmount()

    renderApp()

    expect(screen.getByTestId('prs-sort-select')).toHaveValue('number-asc')
    expect(screen.getByRole('button', { name: /Recently merged/ })).toHaveAttribute(
      'aria-expanded',
      'false',
    )
    expect(screen.getByRole('button', { name: /Forge PRs/ })).toHaveAttribute(
      'aria-expanded',
      'true',
    )
  })

  it('hydrates from seeded storage on first paint with no default-state flash', () => {
    // Pre-seed storage as if a previous session wrote these values, then mount
    // PRsPage. The first synchronous render must already reflect them — we
    // assert without any awaits, so any post-mount effect would necessarily
    // come after the first paint and show as a flash.
    sessionStorage.setItem(`${KEY_PREFIX}prs.pr.filter`, JSON.stringify('beta'))
    localStorage.setItem(`${KEY_PREFIX}prs.pr.sort`, JSON.stringify('title-asc'))
    localStorage.setItem(
      `${KEY_PREFIX}prs.pr.section.external_prs.expanded`,
      JSON.stringify(false),
    )

    renderApp()

    expect(screen.getByRole('textbox', { name: /Filter PRs/ })).toHaveValue('beta')
    expect(screen.getByTestId('prs-sort-select')).toHaveValue('title-asc')
    expect(screen.getByRole('button', { name: /External PRs/ })).toHaveAttribute(
      'aria-expanded',
      'false',
    )
  })

  it('captures window scroll position and restores it after back-navigation', async () => {
    // Use fake timers so we can advance through rAF (shimmed as setTimeout in
    // jsdom) and the 150ms useUIState debounce without real-time waits.
    vi.useFakeTimers()

    const { router } = renderApp()

    // jsdom has no layout engine so window.scrollY always reads 0. Override
    // the property so the onScroll handler reads 250 and writes it to storage.
    let _scrollY = 250
    const origScrollY = Object.getOwnPropertyDescriptor(window, 'scrollY')
    Object.defineProperty(window, 'scrollY', {
      configurable: true,
      get: () => _scrollY,
    })

    // Dispatch the scroll event — triggers the rAF-throttled onScroll handler.
    window.dispatchEvent(new Event('scroll'))

    // Flush rAF (shimmed as setTimeout(0) in jsdom) and the 150ms debounce
    // that runs inside the rAF callback. After this, sessionStorage has the
    // captured scroll position (250).
    await act(async () => {
      vi.runAllTimers()
    })

    // Navigate away — PRsPage unmounts. The debounce timer already fired so
    // sessionStorage already has the captured scroll position.
    await act(async () => {
      await router.navigate('/bead/Forge-aaaa')
    })
    expect(screen.getByTestId('bead-stub')).toBeInTheDocument()

    // Pretend the new page started at scroll 0 so the useLayoutEffect's
    // condition (`window.scrollY !== scrollTop`) is satisfied and scrollTo
    // is actually invoked when we navigate back.
    _scrollY = 0

    // Spy on window.scrollTo to verify that useLayoutEffect restores the
    // scroll position before the browser paints.
    const scrollToSpy = vi.fn()
    window.scrollTo = scrollToSpy

    try {
      await act(async () => {
        await router.navigate(-1)
      })
    } finally {
      if (origScrollY) {
        Object.defineProperty(window, 'scrollY', origScrollY)
      } else {
        delete (window as unknown as Record<string, unknown>).scrollY
      }
      vi.useRealTimers()
    }

    // useLayoutEffect should have called scrollTo(0, 250) before paint.
    expect(scrollToSpy).toHaveBeenCalledWith(0, 250)
  })
})
