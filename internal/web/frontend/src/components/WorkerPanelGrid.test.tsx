import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import type { WorkerInfo } from '../api'

// Render WorkerPanel as a lightweight stub so the grid tests isolate the slot
// math and idle-placeholder logic from the panel's SSE/log rendering.
vi.mock('./WorkerPanel', () => ({
  default: ({ worker }: { worker: WorkerInfo }) => (
    <div data-testid={`panel-${worker.id}`}>{worker.bead_id}</div>
  ),
}))

import WorkerPanelGrid, { isSlotWorker } from './WorkerPanelGrid'

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

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('WorkerPanelGrid slot math', () => {
  it('renders one panel per active worker and fills idle slots up to the cap', () => {
    render(
      <WorkerPanelGrid
        workers={[
          worker({ id: 'w-1', status: 'running' }),
          worker({ id: 'w-2', status: 'pending' }),
        ]}
        maxTotalSmiths={4}
      />,
    )

    expect(screen.getByTestId('panel-w-1')).toBeInTheDocument()
    expect(screen.getByTestId('panel-w-2')).toBeInTheDocument()
    // 4 slots − 2 active = 2 idle placeholders.
    expect(screen.getAllByTestId('worker-panel-idle-slot')).toHaveLength(2)
    expect(screen.queryByTestId('worker-panel-grid-empty')).not.toBeInTheDocument()
  })

  it('never renders negative idle slots when active workers exceed the cap', () => {
    render(
      <WorkerPanelGrid
        workers={[
          worker({ id: 'w-1', status: 'running' }),
          worker({ id: 'w-2', status: 'running' }),
          worker({ id: 'w-3', status: 'running' }),
        ]}
        maxTotalSmiths={1}
      />,
    )

    expect(screen.getAllByTestId(/^panel-/)).toHaveLength(3)
    expect(screen.queryByTestId('worker-panel-idle-slot')).not.toBeInTheDocument()
  })

  it('excludes bellows monitors and terminal-status workers from the active set', () => {
    render(
      <WorkerPanelGrid
        workers={[
          worker({ id: 'w-1', status: 'running' }),
          worker({ id: 'w-2', status: 'done' }),
          worker({ id: 'w-3', status: 'failed' }),
          // Synthetic bellows PR monitor: no log_path, bellows- id prefix.
          worker({
            id: 'bellows-forge-42',
            status: 'running',
            phase: 'bellows',
            log_path: undefined,
          }),
        ]}
        maxTotalSmiths={3}
      />,
    )

    expect(screen.getByTestId('panel-w-1')).toBeInTheDocument()
    expect(screen.queryByTestId('panel-w-2')).not.toBeInTheDocument()
    expect(screen.queryByTestId('panel-w-3')).not.toBeInTheDocument()
    expect(screen.queryByTestId('panel-bellows-forge-42')).not.toBeInTheDocument()
    // 3 cap − 1 active = 2 idle slots.
    expect(screen.getAllByTestId('worker-panel-idle-slot')).toHaveLength(2)
  })

  it('lingers recently-finished workers as extra panels without eating idle slots', () => {
    render(
      <WorkerPanelGrid
        workers={[
          worker({ id: 'w-live', status: 'running' }),
          worker({
            id: 'w-done',
            status: 'done',
            completed_at: '2024-01-01T00:10:00Z',
          }),
          worker({
            id: 'w-failed',
            status: 'failed',
            completed_at: '2024-01-01T00:05:00Z',
          }),
          // Bellows monitor rows never linger, finished or not.
          worker({
            id: 'bellows-forge-9',
            status: 'done',
            phase: 'bellows',
            log_path: undefined,
            completed_at: '2024-01-01T00:09:00Z',
          }),
        ]}
        maxTotalSmiths={2}
      />,
    )

    expect(screen.getByTestId('panel-w-live')).toBeInTheDocument()
    expect(screen.getByTestId('panel-w-done')).toBeInTheDocument()
    expect(screen.getByTestId('panel-w-failed')).toBeInTheDocument()
    expect(screen.queryByTestId('panel-bellows-forge-9')).not.toBeInTheDocument()
    // Finished panels do not consume smith slots: 2 cap − 1 active = 1 idle.
    expect(screen.getAllByTestId('worker-panel-idle-slot')).toHaveLength(1)
  })

  it('shows the empty state when there are no active workers and no slots', () => {
    render(<WorkerPanelGrid workers={[]} maxTotalSmiths={0} />)
    expect(screen.getByTestId('worker-panel-grid-empty')).toBeInTheDocument()
    expect(screen.queryByTestId('worker-panel-idle-slot')).not.toBeInTheDocument()
  })

  it('shows only idle slots (no empty state) when idle capacity exists with no workers', () => {
    render(<WorkerPanelGrid workers={[]} maxTotalSmiths={2} />)
    expect(screen.queryByTestId('worker-panel-grid-empty')).not.toBeInTheDocument()
    expect(screen.getAllByTestId('worker-panel-idle-slot')).toHaveLength(2)
  })
})

describe('isSlotWorker', () => {
  it('treats pending/running/reviewing/paused non-bellows workers as slot holders', () => {
    for (const status of ['pending', 'running', 'reviewing', 'paused']) {
      expect(isSlotWorker(worker({ status }))).toBe(true)
    }
  })

  it('rejects terminal statuses and bellows monitors', () => {
    expect(isSlotWorker(worker({ status: 'done' }))).toBe(false)
    expect(isSlotWorker(worker({ status: 'failed' }))).toBe(false)
    expect(
      isSlotWorker(
        worker({
          id: 'bellows-forge-1',
          status: 'running',
          phase: 'bellows',
          log_path: undefined,
        }),
      ),
    ).toBe(false)
  })
})
