import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'

// Drive the four polled endpoints with static data so the page renders
// deterministically without a live daemon.
vi.mock('../hooks/useApiPoll', () => ({
  useApiPoll: (path: string) => {
    switch (path) {
      case '/api/status':
        return { data: { running: true, max_total_smiths: 2 }, loading: false, error: null }
      case '/api/queue':
        return { data: { items: [] }, loading: false, error: null }
      case '/api/workers':
        return { data: { workers: [] }, loading: false, error: null }
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
vi.mock('../components/PipelineBar', () => ({
  default: () => <div data-testid="pipeline-bar" />,
  isBellowsMonitor: () => false,
}))
vi.mock('../components/CruciblesPane', () => ({ default: () => <div data-testid="crucibles-pane" /> }))
vi.mock('../components/QueuePane', () => ({ default: () => <div data-testid="queue-pane" /> }))
vi.mock('../components/LiveActivity', () => ({ default: () => <div data-testid="live-activity" /> }))
vi.mock('../components/WorkerLogModal', () => ({ default: () => null }))
vi.mock('../components/NeedsAttentionPane', () => ({
  default: () => <div data-testid="needs-attention-pane" />,
}))
vi.mock('../components/WorkerPanelGrid', () => ({
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
