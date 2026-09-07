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

  it('does not count a lifecycle fix worker against capacity', () => {
    // ActiveDispatchWorkers excludes the background phases, so a running
    // quench worker holds no Smith slot. It has a real id and a real log path,
    // so isBellowsMonitor does not catch it — only the phase does. Its row
    // still renders; it is the capacity math it stays out of.
    renderPane(
      [
        worker({ id: 'w-1', status: 'running' }),
        worker({
          id: 'w-2',
          bead_id: 'bd-2',
          status: 'running',
          phase: 'quench',
          log_path: '/logs/bd-2/quench.log',
        }),
      ],
      3,
    )

    // 3 cap − 1 dispatch worker = 2 idle, not 1.
    expect(screen.getAllByTestId('workers-idle-slot')).toHaveLength(2)
    expect(screen.getAllByText('bd-2').length).toBeGreaterThan(0)
  })

  it('counts a schematic-phase worker, which the daemon also counts', () => {
    renderPane(
      [
        worker({ id: 'w-1', status: 'running', phase: 'schematic' }),
        worker({ id: 'w-2', bead_id: 'bd-2', status: 'running', phase: 'smith' }),
      ],
      3,
    )

    expect(screen.getAllByTestId('workers-idle-slot')).toHaveLength(1)
  })

  it('renders a stalled row with its own status chip', () => {
    renderPane([worker({ id: 'w-1', status: 'stalled' })], 0)

    const chip = screen.getByText('stalled')
    expect(chip.className).toContain('orange')
  })
})
