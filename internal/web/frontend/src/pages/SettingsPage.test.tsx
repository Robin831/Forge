import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import type { ConfigKeyInfo, ConfigResponse } from '../api'

const { useApiPollMock, patchMock, patchAnvilMock } = vi.hoisted(() => ({
  useApiPollMock: vi.fn(),
  patchMock: vi.fn(),
  patchAnvilMock: vi.fn(),
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
      patchAnvil: (anvil: string, changes: Record<string, boolean | null>) =>
        patchAnvilMock(anvil, changes),
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
  anvils: {},
}

// CONFIG_WITH_ANVILS adds two registered anvils with a mix of inherit/explicit
// tri-state values and plain bools, exercising the per-anvil section.
const CONFIG_WITH_ANVILS: ConfigResponse = {
  keys: CONFIG.keys,
  anvils: {
    heimdall: {
      auto_merge: true,
      schematic_enabled: null,
      golangci_lint: true,
      go_race_detection: false,
      depcheck_enabled: null,
      questgiver_enabled: null,
      wicket_enabled: null,
      wicket_auto_dispatch: false,
    },
    metadata: {
      auto_merge: false,
      schematic_enabled: false,
      golangci_lint: null,
      go_race_detection: null,
      depcheck_enabled: null,
      questgiver_enabled: null,
      wicket_enabled: null,
      wicket_auto_dispatch: true,
    },
  },
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
  patchAnvilMock.mockReset()
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

  describe('per-anvil overrides', () => {
    it('renders one Pane per registered anvil', () => {
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      expect(screen.getByRole('region', { name: 'heimdall' })).toBeInTheDocument()
      expect(screen.getByRole('region', { name: 'metadata' })).toBeInTheDocument()
    })

    it('reflects tri-state values: inherit (null), on, and off', () => {
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const pane = screen.getByRole('region', { name: 'heimdall' })
      // schematic_enabled is null → Inherit selected.
      const schematic = within(pane).getByRole('radiogroup', {
        name: 'heimdall Schematic pre-analysis',
      })
      expect(within(schematic).getByRole('radio', { name: 'Inherit' })).toHaveAttribute(
        'aria-checked',
        'true',
      )
      // golangci_lint is true → On selected.
      const lint = within(pane).getByRole('radiogroup', { name: 'heimdall golangci-lint' })
      expect(within(lint).getByRole('radio', { name: 'On' })).toHaveAttribute(
        'aria-checked',
        'true',
      )
      // go_race_detection is false → Off selected.
      const race = within(pane).getByRole('radiogroup', {
        name: 'heimdall Go race detection',
      })
      expect(within(race).getByRole('radio', { name: 'Off' })).toHaveAttribute(
        'aria-checked',
        'true',
      )
    })

    it('renders plain bools as two-state switches', () => {
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const pane = screen.getByRole('region', { name: 'heimdall' })
      expect(
        within(pane).getByRole('switch', { name: 'heimdall Auto-merge PRs' }),
      ).toHaveAttribute('aria-checked', 'true')
      expect(
        within(pane).getByRole('switch', { name: 'heimdall Wicket auto-dispatch' }),
      ).toHaveAttribute('aria-checked', 'false')
    })

    it('PATCHes null when a tri-state control is set to Inherit', async () => {
      patchAnvilMock.mockResolvedValue({})
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const lint = within(
        screen.getByRole('region', { name: 'heimdall' }),
      ).getByRole('radiogroup', { name: 'heimdall golangci-lint' })
      await userEvent.click(within(lint).getByRole('radio', { name: 'Inherit' }))

      expect(patchAnvilMock).toHaveBeenCalledWith('heimdall', { golangci_lint: null })
    })

    it('PATCHes true/false when a tri-state control is set to On/Off', async () => {
      patchAnvilMock.mockResolvedValue({})
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const schematic = within(
        screen.getByRole('region', { name: 'heimdall' }),
      ).getByRole('radiogroup', { name: 'heimdall Schematic pre-analysis' })
      await userEvent.click(within(schematic).getByRole('radio', { name: 'On' }))

      expect(patchAnvilMock).toHaveBeenCalledWith('heimdall', {
        schematic_enabled: true,
      })
    })

    it('PATCHes a boolean when a plain-bool switch is toggled', async () => {
      patchAnvilMock.mockResolvedValue({})
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const sw = within(
        screen.getByRole('region', { name: 'heimdall' }),
      ).getByRole('switch', { name: 'heimdall Auto-merge PRs' })
      await userEvent.click(sw)

      expect(patchAnvilMock).toHaveBeenCalledWith('heimdall', { auto_merge: false })
    })

    it('shows "applies on next run" for next-run keys but not for auto_merge', () => {
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const pane = screen.getByRole('region', { name: 'heimdall' })
      // Several next-run keys are present, so the note appears at least once.
      expect(within(pane).getAllByText(/applies on next run/i).length).toBeGreaterThan(0)

      // auto_merge is hot-reloadable: its row must not carry the note.
      const autoMergeRow = within(pane)
        .getByText('Auto-merge PRs')
        .closest('li') as HTMLElement
      expect(
        within(autoMergeRow).queryByText(/applies on next run/i),
      ).not.toBeInTheDocument()
    })

    it('does not render a per-anvil section when no anvils are configured', () => {
      mockPoll({ data: CONFIG })
      renderPage()
      expect(screen.queryByText('Per-anvil overrides')).not.toBeInTheDocument()
    })
  })
})
