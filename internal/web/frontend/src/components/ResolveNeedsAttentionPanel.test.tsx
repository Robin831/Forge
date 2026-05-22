import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ReactNode } from 'react'

import ResolveNeedsAttentionPanel from './ResolveNeedsAttentionPanel'
import { ResolveStoreProvider } from '../stores/resolveStore'

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

function Wrapper({ children }: { children: ReactNode }) {
  return <ResolveStoreProvider>{children}</ResolveStoreProvider>
}

const DETAIL = {
  bead_id: 'Forge-aaaa',
  anvil: 'forge',
  branch: 'forge/Forge-aaaa',
  worktree_path: '/tmp/Forge-aaaa',
  worktree_exists: true,
  escalation_message: 'warden rejected: claimed entities missing',
}

let fetchMock: ReturnType<typeof vi.fn>

beforeEach(() => {
  fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('ResolveNeedsAttentionPanel — dispatch_failed', () => {
  it('renders Approve as-is and Re-run Warden buttons alongside the existing verbs', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(DETAIL))

    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="dispatch_failed"
        />
      </Wrapper>,
    )

    await waitFor(() => {
      expect(screen.getByText('Approve as-is (skip Warden)')).toBeInTheDocument()
    })
    expect(screen.getByText('Re-run Warden')).toBeInTheDocument()
    // The five legacy verbs all still render too.
    expect(screen.getByText('Retry')).toBeInTheDocument()
    expect(screen.getByText('Needs clarification')).toBeInTheDocument()
    expect(screen.getByText('Clear clarification')).toBeInTheDocument()
    expect(screen.getByText('Clear flag')).toBeInTheDocument()
    // Stop is omitted for dispatch_failed (no worker exists).
    expect(screen.queryByText('Stop worker')).not.toBeInTheDocument()
  })

  it('does not include the deprecated clipboard helper', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(DETAIL))

    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="dispatch_failed"
        />
      </Wrapper>,
    )

    await waitFor(() => {
      expect(screen.getByText('Approve as-is (skip Warden)')).toBeInTheDocument()
    })
    expect(screen.queryByText('Open PR manually')).not.toBeInTheDocument()
    expect(screen.queryByText(/Draft PR title/i)).not.toBeInTheDocument()
  })

  it('confirms before invoking approve-as-is and POSTs the right action', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse(DETAIL))
      .mockResolvedValueOnce(jsonResponse({ status: 'ok' }))

    const user = userEvent.setup()
    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="dispatch_failed"
        />
      </Wrapper>,
    )

    const approveButton = await screen.findByText('Approve as-is (skip Warden)')
    await user.click(approveButton)

    // The destructive-verb confirm modal appears with the approve-as-is copy.
    const dialog = await screen.findByRole('dialog')
    expect(
      within(dialog).getByText(/skip the Warden review/i),
    ).toBeInTheDocument()
    // The resolve POST has not gone out yet — only the initial escalation fetch.
    expect(
      fetchMock.mock.calls.filter(
        ([url]) => url === '/api/forge/resolve',
      ),
    ).toHaveLength(0)

    await user.click(within(dialog).getByText('Confirm'))

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.filter(
          ([url]) => url === '/api/forge/resolve',
        ),
      ).toHaveLength(1)
    })
    const [, init] = fetchMock.mock.calls.find(
      ([url]) => url === '/api/forge/resolve',
    ) as [string, RequestInit]
    expect(JSON.parse(init.body as string)).toMatchObject({
      bead_id: 'Forge-aaaa',
      action: 'approve-as-is',
      anvil_name: 'forge',
    })
  })

  it('invokes warden-rerun without showing the confirmation modal', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse(DETAIL))
      .mockResolvedValueOnce(jsonResponse({ status: 'ok' }))

    const user = userEvent.setup()
    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="dispatch_failed"
        />
      </Wrapper>,
    )

    const rerun = await screen.findByText('Re-run Warden')
    await user.click(rerun)

    // No confirm dialog appears for the non-destructive verb.
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.filter(
          ([url]) => url === '/api/forge/resolve',
        ),
      ).toHaveLength(1)
    })
    const [, init] = fetchMock.mock.calls.find(
      ([url]) => url === '/api/forge/resolve',
    ) as [string, RequestInit]
    expect(JSON.parse(init.body as string)).toMatchObject({
      bead_id: 'Forge-aaaa',
      action: 'warden-rerun',
      anvil_name: 'forge',
    })
  })
})

describe('ResolveNeedsAttentionPanel — smith_failed', () => {
  it('renders Re-run Warden but hides Approve as-is for a still-running worker', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(DETAIL))

    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="smith_failed"
        />
      </Wrapper>,
    )

    await waitFor(() => {
      expect(screen.getByText('Re-run Warden')).toBeInTheDocument()
    })
    expect(screen.getByText('Stop worker')).toBeInTheDocument()
    expect(
      screen.queryByText('Approve as-is (skip Warden)'),
    ).not.toBeInTheDocument()
  })
})
