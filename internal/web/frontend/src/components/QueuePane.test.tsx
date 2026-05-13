import '@testing-library/jest-dom/vitest'
import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import QueuePane, { groupQueueItems } from './QueuePane'
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
})
