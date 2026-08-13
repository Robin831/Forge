import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import PipelineBar, { phaseToStage } from './PipelineBar'
import type { WorkerInfo } from '../api'
import { resetPreviewsStore } from '../hooks/usePreview'

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

// The bead rows in the PR half of the pipeline mount PreviewButton, whose
// shared store fetches /api/previews on first subscribe. Stub it so rows can
// decide their preview affordance deterministically (Kiln on, the forge anvil
// previewable, no live previews).
beforeEach(() => {
  resetPreviewsStore()
  vi.stubGlobal(
    'fetch',
    vi.fn(async () =>
      new Response(
        JSON.stringify({
          enabled: true,
          anvils: ['forge'],
          quest_anvils: [],
          previews: [],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    ),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
  resetPreviewsStore()
})

describe('phaseToStage', () => {
  it('maps the ready_to_merge phase to its own stage', () => {
    expect(phaseToStage('ready_to_merge')).toBe('ready_to_merge')
  })

  it('maps the assay phase to the assay stage', () => {
    expect(phaseToStage('assay')).toBe('assay')
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

  it('renders the Assay pill between the PR and Ready to merge pills', () => {
    render(<PipelineBar workers={[]} />)

    expect(screen.getByTestId('pipeline-stage-assay')).toBeInTheDocument()
    expect(screen.getByText('Assay')).toBeInTheDocument()
  })

  it('emits the seven stages in pipeline order', () => {
    const { container } = render(<PipelineBar workers={[]} />)
    const order = Array.from(
      container.querySelectorAll('[data-testid^="pipeline-stage-"]'),
    ).map((el) => el.getAttribute('data-testid'))
    expect(order).toEqual([
      'pipeline-stage-schematic',
      'pipeline-stage-smith',
      'pipeline-stage-temper',
      'pipeline-stage-warden',
      'pipeline-stage-pr',
      'pipeline-stage-assay',
      'pipeline-stage-ready_to_merge',
    ])
  })

  it('counts a running Assay worker in the Assay pill', () => {
    const workers: WorkerInfo[] = [
      worker({
        id: 'assay-forge-1',
        bead_id: 'Forge-eeee',
        phase: 'assay',
        status: 'running',
        pr_number: 5,
      }),
    ]
    render(<PipelineBar workers={workers} />)
    expect(screen.getByTestId('pipeline-count-assay')).toHaveTextContent('1')
  })

  it('marks the Assay stage on the bead row when an Assay and a bellows worker share a bead', () => {
    const workers: WorkerInfo[] = [
      // Synthetic bellows monitor for the open PR.
      worker({
        id: 'bellows-forge-6',
        bead_id: 'Forge-ffff',
        phase: 'bellows',
        pr_number: 6,
      }),
      // Real Assay review worker for the same bead — the more informative entry.
      worker({
        id: 'assay-forge-6',
        bead_id: 'Forge-ffff',
        phase: 'assay',
        status: 'running',
        pr_number: 6,
      }),
    ]
    render(<PipelineBar workers={workers} />)

    const row = screen.getByTestId('pipeline-bead-row')
    // The current stage is identifiable by its accessible label (not colour
    // alone) and carries a distinct icon — the colorblind-safety guarantee.
    const assayMarker = within(row).getByLabelText('Assay (current stage)')
    expect(assayMarker).toBeInTheDocument()
    const icon = assayMarker.querySelector('svg')
    expect(icon).not.toBeNull()
    // Colour remains a secondary cue on the active marker.
    expect(icon).toHaveClass('text-pink-400')
  })

  it('offers preview controls on a ready-to-merge bead row', async () => {
    render(
      <PipelineBar
        workers={[
          worker({
            id: 'bellows-forge-7',
            bead_id: 'Forge-gggg',
            phase: 'ready_to_merge',
            pr_number: 7,
          }),
        ]}
      />,
    )

    // findBy waits out the previews store's initial fetch, which is what
    // authorises the button to render at all.
    expect(await screen.findByTestId('preview-controls-Forge-gggg')).toBeInTheDocument()
    expect(screen.getByTestId('preview-start-Forge-gggg')).toBeInTheDocument()
  })

  it('offers no preview controls before the bead reaches the PR stage', async () => {
    render(
      <PipelineBar
        workers={[
          worker({
            id: 'smith-forge-8',
            bead_id: 'Forge-hhhh',
            phase: 'smith',
            status: 'running',
          }),
        ]}
      />,
    )

    // Give the previews store's fetch a chance to land so the absence is a
    // decision, not a not-loaded-yet artifact.
    await screen.findByTestId('pipeline-bead-row')
    expect(screen.queryByTestId('preview-controls-Forge-hhhh')).not.toBeInTheDocument()
  })

  it('does not truncate long bead IDs', () => {
    render(
      <PipelineBar
        workers={[
          worker({
            bead_id: 'Fhi.Metadata-maz1w',
            title: 'Add metadata export endpoint',
            phase: 'bellows',
          }),
        ]}
      />,
    )
    const beadIdEl = screen.getByText('Fhi.Metadata-maz1w')
    expect(beadIdEl).toBeVisible()
    expect(beadIdEl).toHaveClass('w-40')
  })
})
