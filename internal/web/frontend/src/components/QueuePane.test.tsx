import '@testing-library/jest-dom/vitest'
import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import QueuePane, { groupQueueItems, sortItems } from './QueuePane'
import type { QueueItem } from '../api'

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

function renderPane(items: QueueItem[]) {
  return render(
    <MemoryRouter>
      <QueuePane items={items} loading={false} error={null} />
    </MemoryRouter>,
  )
}

describe('groupQueueItems', () => {
  it('groups items by anvil and routes them to the right bucket', () => {
    const items: QueueItem[] = [
      item({ bead_id: 'a1', anvil: 'forge', section: 'ready' }),
      item({ bead_id: 'a2', anvil: 'forge', section: 'unlabeled' }),
      item({ bead_id: 'a3', anvil: 'forge', section: 'in_progress' }),
      item({ bead_id: 'b1', anvil: 'heimdall', section: 'ready' }),
    ]
    const groups = groupQueueItems(items)
    expect(groups.map((g) => g.anvil)).toEqual(['forge', 'heimdall'])
    const forge = groups[0]
    expect(forge.total).toBe(3)
    expect(forge.buckets.ready.map((i) => i.bead_id)).toEqual(['a1'])
    expect(forge.buckets.unlabeled.map((i) => i.bead_id)).toEqual(['a2'])
    expect(forge.buckets.in_progress.map((i) => i.bead_id)).toEqual(['a3'])
    expect(groups[1].buckets.ready.map((i) => i.bead_id)).toEqual(['b1'])
  })

  it('falls back to ready for unknown section values', () => {
    const groups = groupQueueItems([
      item({ bead_id: 'x', anvil: 'forge', section: 'mystery-future-value' }),
    ])
    expect(groups[0].buckets.ready.map((i) => i.bead_id)).toEqual(['x'])
  })
})

describe('sortItems', () => {
  it('places items with missing timestamps first for asc sorts (treated as oldest)', () => {
    const items = [
      item({ bead_id: 'has-ts', updated_at: '2024-01-02T00:00:00Z' }),
      item({ bead_id: 'no-ts', updated_at: '' }),
    ]
    const asc = sortItems(items, 'updated-asc')
    expect(asc.map((i) => i.bead_id)).toEqual(['no-ts', 'has-ts'])
  })

  it('places items with missing timestamps last for desc sorts (treated as oldest)', () => {
    const items = [
      item({ bead_id: 'no-ts', updated_at: '' }),
      item({ bead_id: 'has-ts', updated_at: '2024-01-02T00:00:00Z' }),
    ]
    const desc = sortItems(items, 'updated-desc')
    expect(desc.map((i) => i.bead_id)).toEqual(['has-ts', 'no-ts'])
  })

  it('places items with missing created_at first for created-asc', () => {
    const items = [
      item({ bead_id: 'has-ts', created_at: '2024-06-01T00:00:00Z' }),
      item({ bead_id: 'no-ts', created_at: '' }),
    ]
    const asc = sortItems(items, 'created-asc')
    expect(asc.map((i) => i.bead_id)).toEqual(['no-ts', 'has-ts'])
  })
})

describe('QueuePane', () => {
  it('renders one collapsed section per anvil with counts', () => {
    renderPane([
      item({ bead_id: 'a1', anvil: 'forge', section: 'ready' }),
      item({ bead_id: 'a2', anvil: 'forge', section: 'unlabeled' }),
      item({ bead_id: 'b1', anvil: 'heimdall', section: 'ready' }),
    ])
    const forgeHeader = screen.getByRole('button', { name: /forge/ })
    const heimdallHeader = screen.getByRole('button', { name: /heimdall/ })
    expect(forgeHeader).toHaveAttribute('aria-expanded', 'false')
    expect(heimdallHeader).toHaveAttribute('aria-expanded', 'false')
    // Counts are visible on the collapsed headers.
    expect(within(forgeHeader).getByText('2')).toBeInTheDocument()
    expect(within(heimdallHeader).getByText('1')).toBeInTheDocument()
    // No bead rows render while everything is collapsed.
    expect(screen.queryByText('a1')).not.toBeInTheDocument()
    expect(screen.queryByText('b1')).not.toBeInTheDocument()
  })

  it('expands an anvil to reveal collapsed status buckets with counts', async () => {
    const user = userEvent.setup()
    renderPane([
      item({ bead_id: 'a1', anvil: 'forge', section: 'ready' }),
      item({ bead_id: 'a2', anvil: 'forge', section: 'unlabeled' }),
      item({ bead_id: 'a3', anvil: 'forge', section: 'in_progress' }),
    ])
    await user.click(screen.getByRole('button', { name: /forge/ }))
    expect(screen.getByRole('button', { name: /Ready \(1\)/ })).toHaveAttribute(
      'aria-expanded',
      'false',
    )
    expect(
      screen.getByRole('button', { name: /Unlabeled \(1\)/ }),
    ).toHaveAttribute('aria-expanded', 'false')
    expect(
      screen.getByRole('button', { name: /In progress \(1\)/ }),
    ).toHaveAttribute('aria-expanded', 'false')
    // Items inside the buckets are still hidden until each bucket is opened.
    expect(screen.queryByText('a1')).not.toBeInTheDocument()
  })

  it('shows items only after expanding both anvil and bucket', async () => {
    const user = userEvent.setup()
    renderPane([
      item({ bead_id: 'a1', anvil: 'forge', section: 'ready' }),
    ])
    await user.click(screen.getByRole('button', { name: /forge/ }))
    await user.click(screen.getByRole('button', { name: /Ready \(1\)/ }))
    expect(screen.getByText('a1')).toBeInTheDocument()
  })

  it('omits empty buckets when the anvil is expanded', async () => {
    const user = userEvent.setup()
    renderPane([
      item({ bead_id: 'a1', anvil: 'forge', section: 'ready' }),
      item({ bead_id: 'a2', anvil: 'forge', section: 'ready' }),
    ])
    await user.click(screen.getByRole('button', { name: /forge/ }))
    expect(screen.getByRole('button', { name: /Ready \(2\)/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Unlabeled/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /In progress/ })).not.toBeInTheDocument()
  })

  it('omits anvils with zero items entirely', () => {
    renderPane([item({ bead_id: 'a1', anvil: 'forge', section: 'ready' })])
    expect(screen.queryByRole('button', { name: /heimdall/ })).not.toBeInTheDocument()
  })

  it('renders an empty state when there are no items', () => {
    renderPane([])
    expect(screen.getByText('No beads in queue.')).toBeInTheDocument()
  })

  it('reorders the visible list when the sort dropdown changes', async () => {
    const user = userEvent.setup()
    // Priority order (default) puts P0/zebra first, then P2/alpha, then P3/middle.
    // Title order is alpha < middle < zebra — so the reorder is observable.
    renderPane([
      item({ bead_id: 'a1', anvil: 'forge', section: 'ready', priority: 2, title: 'alpha' }),
      item({ bead_id: 'a2', anvil: 'forge', section: 'ready', priority: 0, title: 'zebra' }),
      item({ bead_id: 'a3', anvil: 'forge', section: 'ready', priority: 3, title: 'middle' }),
    ])
    await user.click(screen.getByRole('button', { name: /forge/ }))
    await user.click(screen.getByRole('button', { name: /Ready \(3\)/ }))

    // The bead_id <Link> is the only link rendered in each row, so their DOM
    // order reflects the visible row order — robust against unrelated markup.
    const idsInOrder = () =>
      screen.getAllByRole('link').map((a) => a.textContent)

    expect(idsInOrder()).toEqual(['a2', 'a1', 'a3'])

    await user.selectOptions(screen.getByTestId('queue-sort-select'), 'title-asc')

    expect(idsInOrder()).toEqual(['a1', 'a3', 'a2'])
  })

  it('shows a clear button only when the filter has text and resets the filter on click', async () => {
    const user = userEvent.setup()
    renderPane([
      item({ bead_id: 'a1', anvil: 'forge', section: 'ready', title: 'alpha' }),
      item({ bead_id: 'a2', anvil: 'forge', section: 'ready', title: 'beta' }),
    ])

    // The clear button is hidden while the filter is empty.
    expect(screen.queryByRole('button', { name: /Clear filter/ })).not.toBeInTheDocument()

    const filterInput = screen.getByRole('textbox', { name: /Filter beads/ })
    await user.type(filterInput, 'alpha')

    const clearBtn = screen.getByRole('button', { name: /Clear filter/ })
    expect(clearBtn).toBeInTheDocument()
    // Only the matching item's anvil is now visible in the filtered groups.
    expect(screen.getByRole('button', { name: /forge/ })).toHaveTextContent('1')

    await user.click(clearBtn)

    expect(filterInput).toHaveValue('')
    expect(screen.queryByRole('button', { name: /Clear filter/ })).not.toBeInTheDocument()
    // After clearing, both items contribute to the count again.
    expect(screen.getByRole('button', { name: /forge/ })).toHaveTextContent('2')
  })

  it('renders a relative-time label on each row with an ISO tooltip', async () => {
    const user = userEvent.setup()
    const oneHourAgo = new Date(Date.now() - 60 * 60 * 1000).toISOString()
    const twoHoursAgo = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString()
    renderPane([
      item({ bead_id: 'a1', anvil: 'forge', section: 'ready', updated_at: oneHourAgo }),
      item({ bead_id: 'a2', anvil: 'forge', section: 'ready', updated_at: twoHoursAgo }),
    ])
    await user.click(screen.getByRole('button', { name: /forge/ }))
    await user.click(screen.getByRole('button', { name: /Ready \(2\)/ }))

    const firstRow = screen.getByText('a1').closest('li')!
    const secondRow = screen.getByText('a2').closest('li')!
    expect(within(firstRow).getByText('Updated 1h ago')).toBeInTheDocument()
    expect(within(firstRow).getByText('Updated 1h ago')).toHaveAttribute(
      'title',
      oneHourAgo,
    )
    expect(within(secondRow).getByText('Updated 2h ago')).toBeInTheDocument()
    expect(within(secondRow).getByText('Updated 2h ago')).toHaveAttribute(
      'title',
      twoHoursAgo,
    )
  })
})
