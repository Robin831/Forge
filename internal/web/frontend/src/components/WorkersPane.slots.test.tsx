import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import WorkersPane from './WorkersPane'
import type { WorkerInfo } from '../api'

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

function renderPane(workers: WorkerInfo[], maxTotalSmiths: number) {
  return render(
    <MemoryRouter>
      <WorkersPane
        workers={workers}
        loading={false}
        error={null}
        maxTotalSmiths={maxTotalSmiths}
      />
    </MemoryRouter>,
  )
}

afterEach(cleanup)

describe('WorkersPane idle-slot math', () => {
  it('counts a stalled worker against capacity', () => {
    // The daemon counts stalled workers in ActiveDispatchWorkers, so a pane
    // that excluded them offered one idle slot more than could be filled.
    renderPane(
      [
        worker({ id: 'w-1', status: 'running' }),
        worker({ id: 'w-2', bead_id: 'bd-2', status: 'stalled' }),
      ],
      3,
    )

    expect(screen.getAllByTestId('workers-idle-slot')).toHaveLength(1)
  })

  it('counts every slot-holding status the same way', () => {
    renderPane(
      [
        worker({ id: 'w-1', status: 'pending' }),
        worker({ id: 'w-2', bead_id: 'bd-2', status: 'running' }),
        worker({ id: 'w-3', bead_id: 'bd-3', status: 'reviewing' }),
        worker({ id: 'w-4', bead_id: 'bd-4', status: 'paused' }),
        worker({ id: 'w-5', bead_id: 'bd-5', status: 'stalled' }),
      ],
      6,
    )

    expect(screen.getAllByTestId('workers-idle-slot')).toHaveLength(1)
  })

  it('does not count terminal workers against capacity', () => {
    renderPane(
      [
        worker({ id: 'w-1', status: 'running' }),
        worker({ id: 'w-2', bead_id: 'bd-2', status: 'done' }),
      ],
      3,
    )

    expect(screen.getAllByTestId('workers-idle-slot')).toHaveLength(2)
  })

  it('renders a stalled row with its own status chip', () => {
    renderPane([worker({ id: 'w-1', status: 'stalled' })], 0)

    const chip = screen.getByText('stalled')
    expect(chip.className).toContain('orange')
  })
})
