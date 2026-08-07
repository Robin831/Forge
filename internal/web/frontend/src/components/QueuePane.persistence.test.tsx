import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router'
import QueuePane from './QueuePane'
import type { QueueItem } from '../api'
import { KEY_PREFIX } from '../hooks/useUIState'
import { captureScrollWithFakeTimers } from '../test/uiStateTimers'

function item(overrides: Partial<QueueItem>): QueueItem {
  return {
    bead_id: 'bd-1',
    anvil: 'forge',
    title: 'Example bead',
    priority: 2,
    status: 'open',
    labels: [],
    section: 'ready',
    created_at: '',
    updated_at: '',
    ...overrides,
  }
}

// QueueRoot renders the queue pane at "/" — its own page, no shared layout,
// so navigating to /bead/* fully unmounts it. This mirrors App.tsx where
// DashboardPage and BeadDetailPage are sibling top-level routes; the
// remount-on-return is exactly the scenario the persistence has to handle.
function QueueRoot({ items }: { items: QueueItem[] }) {
  return <QueuePane items={items} loading={false} error={null} />
}

function BeadStub() {
  const loc = useLocation()
  return (
    <div data-testid="bead-stub">
      bead detail: {loc.pathname}
      {loc.search}
    </div>
  )
}

function renderApp(items: QueueItem[], initialPath = '/') {
  const router = createMemoryRouter(
    [
      { path: '/', element: <QueueRoot items={items} /> },
      { path: '/bead/:bead_id', element: <BeadStub /> },
    ],
    { initialEntries: [initialPath] },
  )
  const utils = render(<RouterProvider router={router} />)
  return { ...utils, router }
}

const TEST_ITEMS: QueueItem[] = [
  item({
    bead_id: 'a1',
    anvil: 'forge',
    section: 'ready',
    priority: 1,
    title: 'alpha',
  }),
  item({
    bead_id: 'a2',
    anvil: 'forge',
    section: 'unlabeled',
    priority: 2,
    title: 'beta',
  }),
  item({
    bead_id: 'b1',
    anvil: 'heimdall',
    section: 'ready',
    priority: 0,
    title: 'gamma',
  }),
]

beforeEach(() => {
  sessionStorage.clear()
  localStorage.clear()
})

// Safety net the sibling persistence suites already have: a test that throws
// while the clock is frozen must not leave fake timers installed for the next
// one, which would then hang on its first navigation instead of failing.
afterEach(() => {
  vi.useRealTimers()
})

describe('QueuePane persistence', () => {
  it('restores filter, sort, and expansion state after back-navigation', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(TEST_ITEMS)

    // Set a filter that matches "beta" only.
    const filterInput = screen.getByRole('textbox', { name: /Filter beads/ })
    await user.type(filterInput, 'beta')
    expect(filterInput).toHaveValue('beta')

    // Pick a non-default sort. (The bead description mentions "sort by anvil";
    // QueuePane doesn't expose anvil sorting, so we use title-asc — the
    // observable effect is the same: a non-default sortKey that must be
    // restored from localStorage after the round-trip.)
    await user.selectOptions(screen.getByTestId('queue-sort-select'), 'title-asc')

    // Expand the forge anvil and then collapse the Unlabeled bucket inside
    // it. We toggle Unlabeled open once before collapsing it because missing
    // keys mean collapsed already — we want an explicit `false` in storage so
    // we can distinguish "never touched" from "deliberately closed".
    await user.click(screen.getByRole('button', { name: /forge/ }))
    const unlabeledHeader = screen.getByRole('button', { name: /Unlabeled \(1\)/ })
    await user.click(unlabeledHeader)
    expect(unlabeledHeader).toHaveAttribute('aria-expanded', 'true')
    await user.click(unlabeledHeader)
    expect(unlabeledHeader).toHaveAttribute('aria-expanded', 'false')

    // Navigate to the bead detail. This fully unmounts QueuePane because the
    // routes are siblings; useUIState's unmount cleanup flushes any debounced
    // writes synchronously so storage is guaranteed to be current before the
    // back-nav remount reads from it.
    await act(async () => {
      await router.navigate('/bead/a1?anvil=forge&from=queue')
    })
    expect(screen.getByTestId('bead-stub')).toHaveTextContent('from=queue')
    expect(
      screen.queryByRole('textbox', { name: /Filter beads/ }),
    ).not.toBeInTheDocument()

    // Back-navigation remounts QueuePane. useUIState reads storage
    // synchronously inside its lazy useState initialiser, so the very first
    // paint after pop already carries restored values — no default-state
    // flicker. We assert on the first sync paint without any extra awaits.
    await act(async () => {
      await router.navigate(-1)
    })

    const restoredInput = screen.getByRole('textbox', { name: /Filter beads/ })
    expect(restoredInput).toHaveValue('beta')
    expect(screen.getByTestId('queue-sort-select')).toHaveValue('title-asc')
    const forgeHeader = screen.getByRole('button', { name: /forge/ })
    expect(forgeHeader).toHaveAttribute('aria-expanded', 'true')
    const restoredBucket = screen.getByRole('button', { name: /Unlabeled \(1\)/ })
    expect(restoredBucket).toHaveAttribute('aria-expanded', 'false')
  })

  it('restores state after a remount (refresh) from persisted storage', async () => {
    const user = userEvent.setup()
    const first = renderApp(TEST_ITEMS)

    await user.selectOptions(screen.getByTestId('queue-sort-select'), 'title-asc')
    await user.click(screen.getByRole('button', { name: /heimdall/ }))
    expect(screen.getByRole('button', { name: /heimdall/ })).toHaveAttribute(
      'aria-expanded',
      'true',
    )

    // Tear the entire tree down — what a hard refresh looks like to React.
    // sessionStorage and localStorage survive the unmount, so the fresh mount
    // must rehydrate from them.
    first.unmount()

    renderApp(TEST_ITEMS)

    expect(screen.getByTestId('queue-sort-select')).toHaveValue('title-asc')
    expect(screen.getByRole('button', { name: /heimdall/ })).toHaveAttribute(
      'aria-expanded',
      'true',
    )
    expect(screen.getByRole('button', { name: /forge/ })).toHaveAttribute(
      'aria-expanded',
      'false',
    )
  })

  it('hydrates from seeded storage on first paint with no default-state flash', () => {
    // Pre-seed storage as if a previous session wrote these values, then mount
    // the QueuePane. The first synchronous render must already reflect them —
    // we assert without any awaits, so any post-mount effect would necessarily
    // come after the first paint and show as a flash.
    sessionStorage.setItem(
      `${KEY_PREFIX}root.queue-pane.search`,
      JSON.stringify('gamma'),
    )
    localStorage.setItem(
      `${KEY_PREFIX}root.queue-pane.sort`,
      JSON.stringify('title-asc'),
    )
    localStorage.setItem(
      `${KEY_PREFIX}root.queue-pane.bucket-expanded`,
      JSON.stringify({ 'anvil:heimdall': true }),
    )

    renderApp(TEST_ITEMS)

    expect(screen.getByRole('textbox', { name: /Filter beads/ })).toHaveValue(
      'gamma',
    )
    expect(screen.getByTestId('queue-sort-select')).toHaveValue('title-asc')
    expect(screen.getByRole('button', { name: /heimdall/ })).toHaveAttribute(
      'aria-expanded',
      'true',
    )
  })

  it('appends from=queue to bead detail links', async () => {
    const user = userEvent.setup()
    renderApp(TEST_ITEMS)
    await user.click(screen.getByRole('button', { name: /forge/ }))
    await user.click(screen.getByRole('button', { name: /Ready \(1\)/ }))
    const link = screen.getByRole('link', { name: 'a1' })
    expect(link).toHaveAttribute(
      'href',
      '/bead/a1?anvil=forge&from=queue',
    )
  })

  it('captures scroll position and restores it after back-navigation', async () => {
    const { router } = renderApp(TEST_ITEMS)

    // Locate the Pane's scrollable body element (the div with role="region").
    const scrollBody = document.querySelector('div[role="region"]') as HTMLElement
    expect(scrollBody).not.toBeNull()

    // jsdom has no layout engine so el.scrollTop always reads 0. Override the
    // property on this specific element so the onScroll handler reads 300.
    let _scrollTop = 300
    Object.defineProperty(scrollBody, 'scrollTop', {
      configurable: true,
      get: () => _scrollTop,
      set: (v: number) => { _scrollTop = v },
    })

    // Dispatch the scroll event under fake timers and flush rAF plus the 150ms
    // debounce that runs inside the rAF callback. After this, sessionStorage
    // already has the captured scroll position (300) and we are back on real
    // timers — the navigations below must not run on a frozen clock.
    await captureScrollWithFakeTimers(() => {
      scrollBody.dispatchEvent(new Event('scroll'))
    })

    // Navigate away — QueuePane unmounts. useUIState's unmount cleanup is a
    // no-op here (debounce timer already fired), but the value is in storage.
    await act(async () => {
      await router.navigate('/bead/a1?anvil=forge&from=queue')
    })

    expect(screen.getByTestId('bead-stub')).toBeInTheDocument()

    // Spy on scrollTop writes during the back-navigation remount to verify that
    // useLayoutEffect calls el.scrollTop = 300 before the browser paints.
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
      // Navigate back — QueuePane remounts, reads 300 from sessionStorage, and
      // useLayoutEffect sets el.scrollTop = 300 before the browser paints.
      await act(async () => {
        await router.navigate(-1)
      })
    } finally {
      if (origDesc) {
        Object.defineProperty(Element.prototype, 'scrollTop', origDesc)
      }
    }

    // The layout effect must have written the restored position to the body.
    expect(scrollTopWrites).toContain(300)
  })
})
