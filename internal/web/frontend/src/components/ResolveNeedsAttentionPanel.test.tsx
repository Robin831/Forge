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

describe('ResolveNeedsAttentionPanel — dispatch_blocked_stranded_branch', () => {
  it('renders the stranded-branch action set with relabelled verbs', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(DETAIL))

    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="dispatch_blocked_stranded_branch"
        />
      </Wrapper>,
    )

    await waitFor(() => {
      expect(screen.getByText('Open PR from branch')).toBeInTheDocument()
    })
    expect(screen.getByText('Reset branch & retry')).toBeInTheDocument()
    expect(screen.getByText('Accept & clear')).toBeInTheDocument()
    // The generic smith/dispatch verbs are not shown for this class.
    expect(screen.queryByText('Re-run Warden')).not.toBeInTheDocument()
    expect(screen.queryByText('Stop worker')).not.toBeInTheDocument()
    expect(screen.queryByText('Needs clarification')).not.toBeInTheDocument()
  })

  it('still POSTs the underlying verb when the relabelled action fires', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse(DETAIL))
      .mockResolvedValueOnce(jsonResponse({ status: 'ok' }))

    const user = userEvent.setup()
    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="dispatch_blocked_stranded_branch"
        />
      </Wrapper>,
    )

    const openPR = await screen.findByText('Open PR from branch')
    await user.click(openPR)
    // Open-PR maps to approve-as-is, which is destructive → confirm first.
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByText('Confirm'))

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.filter(([url]) => url === '/api/forge/resolve'),
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
})

describe('ResolveNeedsAttentionPanel — pr_create_failed', () => {
  it('renders the Create PR action set', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(DETAIL))

    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="pr_create_failed"
        />
      </Wrapper>,
    )

    await waitFor(() => {
      expect(screen.getByText('Create PR')).toBeInTheDocument()
    })
    expect(screen.getByText('Reset & retry')).toBeInTheDocument()
    expect(screen.getByText('Accept & clear')).toBeInTheDocument()
    // The generic smith/dispatch verbs are not shown for this class.
    expect(screen.queryByText('Re-run Warden')).not.toBeInTheDocument()
    expect(screen.queryByText('Stop worker')).not.toBeInTheDocument()
  })

  it('POSTs create-pr and renders the returned PR number + link on success', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse(DETAIL))
      .mockResolvedValueOnce(
        jsonResponse({
          message: 'opened PR #77 for Forge-aaaa',
          pr_number: 77,
          pr_url: 'https://github.com/example/repo/pull/77',
        }),
      )

    const user = userEvent.setup()
    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="pr_create_failed"
        />
      </Wrapper>,
    )

    const createPR = await screen.findByText('Create PR')
    await user.click(createPR)
    // create-pr is outward-facing → confirm before firing.
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByText('Confirm'))

    await waitFor(() => {
      expect(
        fetchMock.mock.calls.filter(([url]) => url === '/api/forge/resolve'),
      ).toHaveLength(1)
    })
    const [, init] = fetchMock.mock.calls.find(
      ([url]) => url === '/api/forge/resolve',
    ) as [string, RequestInit]
    expect(JSON.parse(init.body as string)).toMatchObject({
      bead_id: 'Forge-aaaa',
      action: 'create-pr',
      anvil_name: 'forge',
    })

    // Success surfaces the PR number and a clickable link to the PR.
    const link = await screen.findByRole('link', { name: /View on GitHub/i })
    expect(link).toHaveAttribute(
      'href',
      'https://github.com/example/repo/pull/77',
    )
    expect(screen.getByText(/Opened PR #77/)).toBeInTheDocument()
  })

  it('surfaces the gh error inline when PR creation fails', async () => {
    fetchMock
      .mockResolvedValueOnce(jsonResponse(DETAIL))
      .mockResolvedValueOnce(
        jsonResponse(
          { error: 'gh pr create failed: 422 protected branch' },
          { status: 500 },
        ),
      )

    const user = userEvent.setup()
    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="pr_create_failed"
        />
      </Wrapper>,
    )

    const createPR = await screen.findByText('Create PR')
    await user.click(createPR)
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByText('Confirm'))

    await waitFor(() => {
      expect(screen.getByText(/protected branch/)).toBeInTheDocument()
    })
    expect(
      screen.queryByRole('link', { name: /View on GitHub/i }),
    ).not.toBeInTheDocument()
  })
})

describe('ResolveNeedsAttentionPanel — clarification', () => {
  it('renders only the clarify / unclarify verbs', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(DETAIL))

    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="clarification"
        />
      </Wrapper>,
    )

    await waitFor(() => {
      expect(screen.getByText('Needs clarification')).toBeInTheDocument()
    })
    expect(screen.getByText('Clear clarification')).toBeInTheDocument()
    expect(screen.queryByText('Retry')).not.toBeInTheDocument()
    expect(screen.queryByText('Clear flag')).not.toBeInTheDocument()
  })
})

describe('ResolveNeedsAttentionPanel — anvil hint', () => {
  it('passes the anvil hint to the escalation fetch', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(DETAIL))

    render(
      <Wrapper>
        <ResolveNeedsAttentionPanel
          escalationId="Forge-aaaa"
          escalationType="smith_failed"
          anvil="forge"
        />
      </Wrapper>,
    )

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalled()
    })
    const [url] = fetchMock.mock.calls[0] as [string]
    expect(url).toBe('/api/forge/escalation/Forge-aaaa?anvil=forge')
  })
})
