import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { PRFindingsResponse, PRItem } from '../api'
import PRFindingsPanel from './PRFindingsPanel'

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

function pr(overrides: Partial<PRItem> = {}): PRItem {
  return {
    id: 7,
    number: 42,
    anvil: 'forge',
    status: 'open',
    is_external: false,
    title: 'Example PR',
    ...overrides,
  }
}

const SNAPSHOT: PRFindingsResponse = {
  pr: 42,
  anvil: 'forge',
  run: {
    status: 'complete',
    findings_count: 2,
    posted_count: 1,
    started_at: '2026-05-01T00:00:00Z',
  },
  findings: [
    {
      id: 1,
      pr: 42,
      anvil: 'forge',
      status: 'posted',
      severity: 'Important',
      message: 'Nil pointer dereference in handler',
      file: 'internal/web/prs.go',
      anchor: 'L120',
      category: 'correctness',
    },
    {
      id: 2,
      pr: 42,
      anvil: 'forge',
      status: 'open',
      severity: 'Nit',
      message: 'Prefer errors.Is over string compare',
    },
  ],
}

let fetchMock: ReturnType<typeof vi.fn>

beforeEach(() => {
  fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('PRFindingsPanel', () => {
  it('renders the run summary and findings from the initial fetch', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(SNAPSHOT))

    render(<PRFindingsPanel pr={pr()} />)

    await waitFor(() => {
      expect(screen.getByText('Nil pointer dereference in handler')).toBeInTheDocument()
    })
    expect(screen.getByText('Prefer errors.Is over string compare')).toBeInTheDocument()
    // Run summary surfaces the finding/posted counts.
    expect(screen.getByText(/2 findings/)).toBeInTheDocument()
    expect(screen.getByText(/1 posted/)).toBeInTheDocument()
    // Severity + status badges render.
    expect(screen.getByText('Important')).toBeInTheDocument()
    expect(screen.getByText('Nit')).toBeInTheDocument()
    // File location shows for the first finding.
    expect(screen.getByText('internal/web/prs.go:L120')).toBeInTheDocument()

    // getFindings hit the id-keyed endpoint.
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/prs/7/findings',
      expect.objectContaining({ credentials: 'include' }),
    )
  })

  it('gives a partial run its own chip and names the passes that did not review the head', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        ...SNAPSHOT,
        run: {
          status: 'partial',
          findings_count: 2,
          posted_count: 1,
          started_at: '2026-05-01T00:00:00Z',
          completed_passes: 4,
          total_passes: 5,
          failed_passes: [{ name: 'logic', reason: 'error_max_turns' }],
          status_text: 'partial: 4 of 5 passes completed (failed: logic — error_max_turns)',
        },
      } satisfies PRFindingsResponse),
    )

    render(<PRFindingsPanel pr={pr()} />)

    const chip = await screen.findByText('partial')
    // Neither the complete (emerald) nor the error (red) chip — a run that
    // half-reviewed the head is its own outcome.
    expect(chip.className).toContain('amber')
    expect(chip.className).not.toContain('emerald')
    expect(chip.className).not.toContain('red')
    expect(
      screen.getByText('partial: 4 of 5 passes completed (failed: logic — error_max_turns)'),
    ).toBeInTheDocument()
  })

  it('renders an empty state when there are no findings', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ pr: 42, anvil: 'forge', run: null, findings: [] }),
    )

    render(<PRFindingsPanel pr={pr()} />)

    await waitFor(() => {
      expect(screen.getByText('No findings for this PR.')).toBeInTheDocument()
    })
    expect(screen.getByText('No Assay run recorded yet.')).toBeInTheDocument()
  })

  it('POSTs to the rerun endpoint with the anvil body when Re-run is clicked', async () => {
    const user = userEvent.setup()
    fetchMock
      .mockResolvedValueOnce(jsonResponse(SNAPSHOT)) // initial getFindings
      .mockResolvedValueOnce(jsonResponse({ message: 'queued' }, { status: 202 })) // rerun
      .mockResolvedValueOnce(jsonResponse(SNAPSHOT)) // refetch on success

    render(<PRFindingsPanel pr={pr()} />)

    await waitFor(() => {
      expect(screen.getByText('Nil pointer dereference in handler')).toBeInTheDocument()
    })

    await user.click(screen.getByRole('button', { name: /Re-run/i }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([url]) => url === '/api/prs/7/rerun-assay')
      expect(call).toBeTruthy()
    })
    const [, init] = fetchMock.mock.calls.find(
      ([url]) => url === '/api/prs/7/rerun-assay',
    )!
    expect(init).toMatchObject({ method: 'POST' })
    expect(JSON.parse((init as RequestInit).body as string)).toEqual({ anvil: 'forge' })
    // The CSRF action header is set by apiPost.
    expect((init as RequestInit).headers).toMatchObject({ 'X-Forge-Action': '1' })
  })
})
