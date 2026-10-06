import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'

// Drive the four polled endpoints with static data so the page renders
// deterministically without a live daemon. A test that needs workers sets
// mockWorkers before rendering.
let mockWorkers: unknown[] = []
vi.mock('../hooks/useApiPoll', () => ({
  useApiPoll: (path: string) => {
    if (path.startsWith('/api/workers')) {
      return { data: { workers: mockWorkers }, loading: false, error: null }
    }
    switch (path) {
      case '/api/status':
        return { data: { running: true, max_total_smiths: 2 }, loading: false, error: null }
      case '/api/queue':
        return { data: { items: [] }, loading: false, error: null }
      case '/api/crucibles':
        return { data: { crucibles: [] }, loading: false, error: null }
      default:
        return { data: null, loading: false, error: null }
    }
  },
}))

// Stub the heavy children so we can assert layout placement by test id.
vi.mock('../components/AppHeader', () => ({ default: () => <div data-testid="app-header" /> }))
vi.mock('../components/DispatchToggle', () => ({ default: () => <div data-testid="dispatch-toggle" /> }))
vi.mock('../components/PipelineBar', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../components/PipelineBar')>()),
  default: () => <div data-testid="pipeline-bar" />,
}))
vi.mock('../components/CruciblesPane', () => ({ default: () => <div data-testid="crucibles-pane" /> }))
vi.mock('../components/QueuePane', () => ({ default: () => <div data-testid="queue-pane" /> }))
vi.mock('../components/LiveActivity', () => ({ default: () => <div data-testid="live-activity" /> }))
vi.mock('../components/WorkerLogModal', () => ({ default: () => null }))
vi.mock('../components/NeedsAttentionPane', () => ({
  default: () => <div data-testid="needs-attention-pane" />,
}))
vi.mock('../components/WorkerPanelGrid', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../components/WorkerPanelGrid')>()),
  default: () => <div data-testid="worker-panel-grid" />,
}))

import DashboardPage from './DashboardPage'

function renderDashboard() {
  return render(
    <MemoryRouter>
      <DashboardPage />
    </MemoryRouter>,
  )
}

afterEach(() => {
  cleanup()
  mockWorkers = []
  vi.clearAllMocks()
})

describe('DashboardPage layout swap (Forge-f0iz)', () => {
  it('renders the WorkerPanelGrid full-width, outside the main 3-col grid', () => {
    renderDashboard()
    const grid = screen.getByTestId('worker-panel-grid')
    expect(grid).toBeInTheDocument()
    const main = screen.getByRole('main')
    expect(main).not.toContainElement(grid)
  })

  it('moves NeedsAttentionPane into the main grid alongside Queue and LiveActivity', () => {
    renderDashboard()
    const main = screen.getByRole('main')
    expect(within(main).getByTestId('queue-pane')).toBeInTheDocument()
    expect(within(main).getByTestId('needs-attention-pane')).toBeInTheDocument()
    expect(within(main).getByTestId('live-activity')).toBeInTheDocument()
  })

  it('no longer renders the standalone WorkersPane', () => {
    renderDashboard()
    // WorkersPane exposes a "Workers" pane landmark; the grid replaced it.
    expect(screen.queryByRole('region', { name: 'Workers' })).not.toBeInTheDocument()
  })
})

describe('DashboardPage active workers stat (Forge-ypsa)', () => {
  function statValue(label: string): string | null {
    const card = screen.getByText(label).parentElement?.parentElement
    return card?.lastElementChild?.textContent ?? null
  }

  it('counts every slot status, not just pending/running', () => {
    mockWorkers = [
      { id: 'w-pending', bead_id: 'b1', status: 'pending', log_path: '/l/1' },
      { id: 'w-running', bead_id: 'b2', status: 'running', log_path: '/l/2' },
      { id: 'w-reviewing', bead_id: 'b3', status: 'reviewing', log_path: '/l/3' },
      { id: 'w-paused', bead_id: 'b4', status: 'paused', log_path: '/l/4' },
      { id: 'w-stalled', bead_id: 'b5', status: 'stalled', log_path: '/l/5' },
      // Terminal rows linger in the ?recent= payload but are not active.
      { id: 'w-done', bead_id: 'b6', status: 'done', log_path: '/l/6', completed_at: '2026-10-06T00:00:00Z' },
      { id: 'w-failed', bead_id: 'b7', status: 'failed', log_path: '/l/7', completed_at: '2026-10-06T00:00:00Z' },
      // A bellows PR monitor in a slot status is a synthetic row, not a worker.
      { id: 'bellows-forge-12', bead_id: 'b8', status: 'running', phase: 'bellows' },
    ]
    renderDashboard()
    expect(statValue('Active workers')).toBe('5')
  })
})
