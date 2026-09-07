import '@testing-library/jest-dom/vitest'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import type { WorkerInfo } from '../api'

const { apiGetMock, useEventSourceMock } = vi.hoisted(() => ({
  apiGetMock: vi.fn(),
  useEventSourceMock: vi.fn(),
}))

vi.mock('../api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../api')>()),
  apiGet: (path: string) => apiGetMock(path),
}))

vi.mock('../hooks/useEventSource', () => ({
  useEventSource: (url: string | null, opts?: unknown) => useEventSourceMock(url, opts),
}))

vi.mock('../auth', () => ({
  useAuth: () => ({ logout: vi.fn() }),
}))

import WorkerLogModal from './WorkerLogModal'
import { isSteerTargetStatus } from '../lib/workerStatus'

function worker(status: string): WorkerInfo {
  return {
    id: 'w1',
    bead_id: 'Forge-abc1',
    anvil: 'forge',
    title: 'Test worker',
    status,
    started_at: '2024-01-01T00:00:00Z',
    // A recorded Claude session, so steerDisabledReason has no second reason to
    // disable the composer and the status is the only thing under test.
    session_id: 'sess-1',
    model: 'claude-opus-5',
  }
}

beforeEach(() => {
  apiGetMock.mockResolvedValue({ lines: [] })
  useEventSourceMock.mockReturnValue({ items: [], status: 'closed', error: null, clear: () => {} })
})

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

// The expanded log view is the surface that kept a fourth private copy of the
// steer matrix after WorkerPanel, WorkersPane and BeadDetailPage had been moved
// onto the shared one — so a stalled worker offered a composer in its panel and
// none at all once its log was opened. These cases assert the modal against the
// shared predicate itself rather than against a hand-written list, which is the
// only form that cannot drift away from it again.
describe('WorkerLogModal steer composer', () => {
  for (const status of ['running', 'pending', 'reviewing', 'paused', 'stalled']) {
    it(`renders the composer for a ${status} worker`, async () => {
      expect(isSteerTargetStatus(status)).toBe(true)
      render(<WorkerLogModal worker={worker(status)} onClose={() => {}} />)

      const input = (await screen.findByLabelText('Steer message')) as HTMLInputElement
      expect(input).toBeEnabled()
      expect(
        screen.queryByText(/No active pipeline — steering requires an active Smith worker\./),
      ).not.toBeInTheDocument()
    })
  }

  for (const status of ['done', 'failed', 'killed', 'timeout']) {
    it(`renders no composer for a ${status} worker`, () => {
      expect(isSteerTargetStatus(status)).toBe(false)
      render(<WorkerLogModal worker={worker(status)} onClose={() => {}} />)

      expect(screen.queryByLabelText('Steer message')).not.toBeInTheDocument()
    })
  }
})
