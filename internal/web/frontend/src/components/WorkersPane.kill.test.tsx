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

function renderPane(workers: WorkerInfo[]) {
  return render(
    <MemoryRouter>
      <WorkersPane workers={workers} loading={false} error={null} />
    </MemoryRouter>,
  )
}

afterEach(cleanup)

describe('WorkersPane kill affordance', () => {
  it('offers kill for a stalled worker, as it does for a running one', () => {
    // The two panes had a copy each of the kill gate and both omitted
    // 'stalled', whose process is still running and which the daemon's
    // killWorkerProcess accepts with no status gate at all.
    renderPane([
      worker({ id: 'w-1', status: 'running' }),
      worker({ id: 'w-2', bead_id: 'bd-2', status: 'stalled' }),
    ])

    expect(screen.getByRole('button', { name: 'Kill worker w-1' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Kill worker w-2' })).toBeInTheDocument()
  })

  it('offers no kill for paused or terminal workers', () => {
    renderPane([
      worker({ id: 'w-1', status: 'paused' }),
      worker({ id: 'w-2', bead_id: 'bd-2', status: 'done', completed_at: '2024-01-01T00:05:00Z' }),
    ])

    expect(screen.queryByRole('button', { name: 'Kill worker w-1' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Kill worker w-2' })).not.toBeInTheDocument()
  })
})
