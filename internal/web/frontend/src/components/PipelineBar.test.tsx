import '@testing-library/jest-dom/vitest'
import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import PipelineBar, { phaseToStage } from './PipelineBar'
import type { WorkerInfo } from '../api'

function worker(overrides: Partial<WorkerInfo>): WorkerInfo {
  return {
    id: 'w-1',
    bead_id: 'Forge-aaaa',
    anvil: 'forge',
    status: 'monitoring',
    phase: 'bellows',
    started_at: '2026-05-14T10:00:00Z',
    ...overrides,
  }
}

describe('phaseToStage', () => {
  it('maps the ready_to_merge phase to its own stage', () => {
    expect(phaseToStage('ready_to_merge')).toBe('ready_to_merge')
  })

  it('drops the legacy merged phase so it never lands on a stage', () => {
    expect(phaseToStage('merged')).toBeNull()
  })
})

describe('PipelineBar', () => {
  it('renders the Ready to merge pill and omits the Merged pill', () => {
    render(<PipelineBar workers={[]} />)

    expect(screen.queryByTestId('pipeline-stage-merged')).not.toBeInTheDocument()
    expect(screen.queryByText('Merged')).not.toBeInTheDocument()

    expect(screen.getByTestId('pipeline-stage-ready_to_merge')).toBeInTheDocument()
    expect(screen.getByText('Ready to merge')).toBeInTheDocument()
  })

  it('counts ready_to_merge workers in the Ready to merge pill, not the PR pill', () => {
    const workers: WorkerInfo[] = [
      // Two PRs flipped to ready_to_merge by the daemon.
      worker({
        id: 'bellows-forge-1',
        bead_id: 'Forge-aaaa',
        phase: 'ready_to_merge',
        pr_number: 1,
      }),
      worker({
        id: 'bellows-forge-2',
        bead_id: 'Forge-bbbb',
        phase: 'ready_to_merge',
        pr_number: 2,
      }),
      // One PR still being monitored by bellows (CI not yet green).
      worker({
        id: 'bellows-forge-3',
        bead_id: 'Forge-cccc',
        phase: 'bellows',
        pr_number: 3,
      }),
      // One PR being fix-iterated by bellows — quench worker counts in PR/Bellows.
      worker({
        id: 'quench-forge-4',
        bead_id: 'Forge-dddd',
        phase: 'quench',
        status: 'running',
      }),
    ]

    render(<PipelineBar workers={workers} />)

    expect(screen.getByTestId('pipeline-count-ready_to_merge')).toHaveTextContent('2')
    expect(screen.getByTestId('pipeline-count-pr')).toHaveTextContent('2')
  })

  it('shows zero in the Ready to merge pill when no PR meets every condition', () => {
    const workers: WorkerInfo[] = [
      worker({ id: 'bellows-forge-9', phase: 'bellows', pr_number: 9 }),
    ]
    render(<PipelineBar workers={workers} />)
    expect(screen.getByTestId('pipeline-count-ready_to_merge')).toHaveTextContent('0')
  })

  it('does not truncate long bead IDs', () => {
    render(
      <PipelineBar
        workers={[worker({ bead_id: 'Fhi.Metadata-maz1w', phase: 'bellows' })]}
      />,
    )
    expect(screen.getByText('Fhi.Metadata-maz1w')).toBeVisible()
  })
})
