import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import type { ConfigKeyInfo, ConfigResponse } from '../api'

const { useApiPollMock, patchMock } = vi.hoisted(() => ({
  useApiPollMock: vi.fn(),
  patchMock: vi.fn(),
}))

vi.mock('../hooks/useApiPoll', () => ({
  useApiPoll: (path: string, intervalMs?: number) => useApiPollMock(path, intervalMs),
}))

vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return {
    ...actual,
    forgeConfig: {
      get: vi.fn(),
      patch: (changes: Record<string, boolean>) => patchMock(changes),
    },
  }
})

import SettingsPage from './SettingsPage'

function key(overrides: Partial<ConfigKeyInfo>): ConfigKeyInfo {
  return {
    key: 'schematic_enabled',
    value: false,
    isDefault: true,
    hotReloadable: false,
    area: 'Pipeline',
    label: 'Schematic pre-analysis',
    description: 'Enable the Schematic pre-worker globally.',
    ...overrides,
  }
}

const CONFIG: ConfigResponse = {
  keys: [
    key({ key: 'schematic_enabled', area: 'Pipeline', label: 'Schematic pre-analysis', value: false, hotReloadable: false }),
    key({ key: 'auto_learn_rules', area: 'Warden', label: 'Auto-learn Warden rules', value: true, hotReloadable: false }),
    key({ key: 'smelter_enabled', area: 'Smelter', label: 'Smelter background process', value: true, hotReloadable: true }),
  ],
}

function mockPoll(config: { data: ConfigResponse | null; loading?: boolean; error?: string | null }) {
  useApiPollMock.mockImplementation((path: string) => {
    if (path === '/api/forge/config') {
      return { data: config.data, loading: config.loading ?? false, error: config.error ?? null }
    }
    // /api/status
    return { data: { running: true }, loading: false, error: null }
  })
}

function renderPage() {
  return render(
    <MemoryRouter>
      <SettingsPage />
    </MemoryRouter>,
  )
}

afterEach(() => {
  cleanup()
  useApiPollMock.mockReset()
  patchMock.mockReset()
})

describe('SettingsPage', () => {
  it('renders settings grouped by area', () => {
    mockPoll({ data: CONFIG })
    renderPage()

    // One section (Pane) per area.
    expect(screen.getByRole('region', { name: 'Pipeline' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Warden' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Smelter' })).toBeInTheDocument()

    expect(screen.getByText('Schematic pre-analysis')).toBeInTheDocument()
    expect(screen.getByText('Auto-learn Warden rules')).toBeInTheDocument()
  })

  it('reflects the current value of each setting', () => {
    mockPoll({ data: CONFIG })
    renderPage()

    expect(screen.getByRole('switch', { name: 'Schematic pre-analysis' })).toHaveAttribute(
      'aria-checked',
      'false',
    )
    expect(screen.getByRole('switch', { name: 'Auto-learn Warden rules' })).toHaveAttribute(
      'aria-checked',
      'true',
    )
  })

  it('shows the "applies on next run" note only for non-hot-reloadable keys', () => {
    mockPoll({ data: CONFIG })
    renderPage()

    const pipeline = screen.getByRole('region', { name: 'Pipeline' })
    expect(within(pipeline).getByText(/applies on next run/i)).toBeInTheDocument()

    const smelter = screen.getByRole('region', { name: 'Smelter' })
    expect(within(smelter).queryByText(/applies on next run/i)).not.toBeInTheDocument()
  })

  it('PATCHes the toggled key and optimistically updates', async () => {
    patchMock.mockResolvedValue(CONFIG)
    mockPoll({ data: CONFIG })
    renderPage()

    const sw = screen.getByRole('switch', { name: 'Schematic pre-analysis' })
    await userEvent.click(sw)

    expect(patchMock).toHaveBeenCalledWith({ schematic_enabled: true })
    expect(sw).toHaveAttribute('aria-checked', 'true')
  })

  it('reverts the toggle when the PATCH fails', async () => {
    let reject!: (e: unknown) => void
    patchMock.mockReturnValue(
      new Promise((_res, rej) => {
        reject = rej
      }),
    )
    mockPoll({ data: CONFIG })
    renderPage()

    const sw = screen.getByRole('switch', { name: 'Schematic pre-analysis' })
    await userEvent.click(sw)
    expect(sw).toHaveAttribute('aria-checked', 'true')

    await act(async () => {
      reject(new Error('boom'))
      await Promise.resolve()
    })

    await waitFor(() => expect(sw).toHaveAttribute('aria-checked', 'false'))
  })

  it('renders an error pane when the config fails to load', () => {
    mockPoll({ data: null, error: 'HTTP 500' })
    renderPage()
    expect(screen.getByText('HTTP 500')).toBeInTheDocument()
  })
})
