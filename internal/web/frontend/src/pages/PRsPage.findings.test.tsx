import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import type { PRItem } from '../api'

const { useApiPollMock, usePRsDataMock } = vi.hoisted(() => ({
  useApiPollMock: vi.fn(),
  usePRsDataMock: vi.fn(),
}))

vi.mock('../hooks/useApiPoll', () => ({
  useApiPoll: (path: string, intervalMs?: number) => useApiPollMock(path, intervalMs),
}))

vi.mock('./usePRsData', () => ({
  usePRsData: () => usePRsDataMock(),
  PRS_CACHE_TTL_MS: 60_000,
}))

import PRsPage from './PRsPage'

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

function renderApp() {
  const router = createMemoryRouter([{ path: '/prs', element: <PRsPage /> }], {
    initialEntries: ['/prs'],
  })
  return render(<RouterProvider router={router} />)
}

const FORGE_PR: PRItem = {
  id: 7,
  number: 42,
  anvil: 'forge',
  status: 'open',
  is_external: false,
  title: 'alpha forge pr',
  bead_id: 'Forge-aaaa',
}

let fetchMock: ReturnType<typeof vi.fn>

beforeEach(() => {
  sessionStorage.clear()
  localStorage.clear()
  fetchMock = vi.fn().mockResolvedValue(
    jsonResponse({ pr: 42, anvil: 'forge', run: null, findings: [] }),
  )
  vi.stubGlobal('fetch', fetchMock)
  useApiPollMock.mockReturnValue({
    data: { running: true, pid: 1 },
    error: null,
    loading: false,
  })
  usePRsDataMock.mockReturnValue({
    forge_prs: [FORGE_PR],
    external_prs: [],
    recently_merged: [],
    loading: false,
    error: null,
    fetchedAt: Date.now(),
    refresh: vi.fn(),
  })
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('PRsPage Assay findings toggle', () => {
  it('reveals the findings panel and loads findings when the toggle is clicked', async () => {
    const user = userEvent.setup()
    renderApp()

    // Panel is hidden until the toggle is clicked.
    expect(
      screen.queryByRole('region', { name: /Assay findings for PR #42/ }),
    ).not.toBeInTheDocument()

    await user.click(screen.getByTestId('pr-findings-toggle-7'))

    await waitFor(() => {
      expect(
        screen.getByRole('region', { name: /Assay findings for PR #42/ }),
      ).toBeInTheDocument()
    })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/prs/7/findings',
      expect.objectContaining({ credentials: 'include' }),
    )
  })
})
