import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type {
  AnvilKeyInfo,
  ConfigKeyInfo,
  ConfigResponse,
  ConfigValue,
} from '../api'

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
      patch: (changes: Record<string, ConfigValue>) => patchMock(changes),
      patchAnvil: (anvil: string, changes: Record<string, ConfigValue | null>) =>
        patchAnvilMock(anvil, changes),
    },
  }
})

import SettingsPage from './SettingsPage'

function key(overrides: Partial<ConfigKeyInfo>): ConfigKeyInfo {
  return {
    key: 'schematic_enabled',
    type: 'bool',
    value: false,
    isDefault: true,
    hotReloadable: false,
    area: 'Pipeline',
    label: 'Schematic pre-analysis',
    description: 'Enable the Schematic pre-worker globally.',
    ...overrides,
  }
}

// ANVIL_KEYS mirrors a representative slice of the backend per-anvil schema:
// a hot-reloadable plain bool, several tri-state bools, and the non-bool
// scalars (number / enum / string).
const ANVIL_KEYS: AnvilKeyInfo[] = [
  {
    key: 'auto_merge',
    type: 'bool',
    triState: false,
    hotReloadable: true,
    label: 'Auto-merge PRs',
    description: "Automatically merge this anvil's PRs once checks pass.",
  },
  {
    key: 'schematic_enabled',
    type: 'bool',
    triState: true,
    hotReloadable: false,
    label: 'Schematic pre-analysis',
    description: 'Override the global Schematic pre-worker for this anvil.',
  },
  {
    key: 'golangci_lint',
    type: 'bool',
    triState: true,
    hotReloadable: false,
    label: 'golangci-lint',
    description: 'Run golangci-lint as a Temper step for this anvil.',
  },
  {
    key: 'go_race_detection',
    type: 'bool',
    triState: true,
    hotReloadable: false,
    label: 'Go race detection',
    description: 'Run the Go race detector for this anvil.',
  },
  {
    key: 'wicket_auto_dispatch',
    type: 'bool',
    triState: false,
    hotReloadable: false,
    label: 'Wicket auto-dispatch',
    description: 'Auto-dispatch beads created by Wicket triage.',
  },
  {
    key: 'max_smiths',
    type: 'int',
    triState: false,
    hotReloadable: false,
    label: 'Max smiths',
    description: 'Cap concurrent smiths for this anvil.',
    min: 0,
    max: 16,
  },
  {
    key: 'auto_dispatch',
    type: 'enum',
    triState: false,
    hotReloadable: false,
    label: 'Auto-dispatch mode',
    description: 'How beads are auto-dispatched for this anvil.',
    options: ['off', 'labeled', 'all'],
  },
  {
    key: 'platform',
    type: 'string',
    triState: false,
    hotReloadable: false,
    label: 'Platform',
    description: 'CI platform override for this anvil.',
  },
  {
    key: 'stage_providers',
    type: 'provider_map',
    triState: false,
    hotReloadable: false,
    label: 'Per-stage providers',
    description: 'Per-anvil override of the global per-stage provider chains.',
    options: ['smith', 'warden'],
  },
  {
    key: 'wicket_trusted_users',
    type: 'string_list',
    triState: false,
    hotReloadable: false,
    label: 'Wicket trusted users',
    description: 'GitHub logins whose issues are auto-dispatched.',
  },
  {
    key: 'wicket_ignore_users',
    type: 'string_list',
    triState: false,
    hotReloadable: false,
    label: 'Wicket ignored users',
    description: 'GitHub logins skipped entirely when triaging issues.',
  },
  {
    key: 'wicket_repos',
    type: 'string_list',
    triState: false,
    hotReloadable: false,
    label: 'Wicket repositories',
    description: '"owner/repo" strings Wicket scans for this anvil.',
  },
  {
    key: 'wicket_issue_labels',
    type: 'string_list',
    triState: false,
    hotReloadable: false,
    label: 'Wicket issue labels',
    description: 'Labels an issue must carry for Wicket to triage it.',
  },
  {
    key: 'wicket_triage_prompt',
    type: 'string',
    triState: false,
    hotReloadable: false,
    label: 'Wicket triage prompt',
    description: 'Optional prompt suffix appended to the triage system prompt.',
  },
]

const CONFIG: ConfigResponse = {
  keys: [
    key({ key: 'schematic_enabled', type: 'bool', area: 'Pipeline', label: 'Schematic pre-analysis', value: false, hotReloadable: false }),
    key({ key: 'auto_learn_rules', type: 'bool', area: 'Warden', label: 'Auto-learn Warden rules', value: true, hotReloadable: false }),
    key({ key: 'smelter_enabled', type: 'bool', area: 'Smelter', label: 'Smelter background process', value: true, hotReloadable: true }),
    key({ key: 'max_total_smiths', type: 'int', area: 'Pipeline', label: 'Max total smiths', value: 4, hotReloadable: true, min: 1, max: 32 }),
    key({ key: 'daily_cost_limit', type: 'float', area: 'Pipeline', label: 'Daily cost limit', value: 25, hotReloadable: true, unit: 'USD' }),
    key({ key: 'log_level', type: 'enum', area: 'Pipeline', label: 'Log level', value: 'info', hotReloadable: true, options: ['debug', 'info', 'warn'] }),
    key({ key: 'default_branch', type: 'string', area: 'Pipeline', label: 'Default branch', value: 'main', hotReloadable: true }),
    key({ key: 'providers', type: 'string_list', area: 'Providers', label: 'Provider chain', value: ['claude', 'gemini'], hotReloadable: true }),
    key({ key: 'stage_providers', type: 'provider_map', area: 'Providers', label: 'Per-stage providers', value: { smith: ['claude'] }, hotReloadable: true, options: ['smith', 'warden'] }),
    key({ key: 'poll_interval', type: 'duration', area: 'Scheduling', label: 'Poll interval', value: '5m0s', hotReloadable: false }),
    key({ key: 'smith_timeout', type: 'duration', area: 'Scheduling', label: 'Smith timeout', value: '30m0s', hotReloadable: false }),
  ],
  anvilKeys: ANVIL_KEYS,
  anvils: {},
}

// COMPOSITE_ANVIL_FIELDS is the per-anvil composite override projection (all
// inheriting by default) shared by the anvil fixtures below.
const COMPOSITE_ANVIL_FIELDS = {
  stage_providers: null,
  wicket_trusted_users: null,
  wicket_ignore_users: null,
  wicket_repos: null,
  wicket_issue_labels: null,
  wicket_triage_prompt: '',
}

// CONFIG_WITH_ANVILS adds two registered anvils with a mix of inherit/explicit
// tri-state values, plain bools, and the non-bool scalars.
const CONFIG_WITH_ANVILS: ConfigResponse = {
  keys: CONFIG.keys,
  anvilKeys: ANVIL_KEYS,
  anvils: {
    heimdall: {
      auto_merge: true,
      schematic_enabled: null,
      golangci_lint: true,
      go_race_detection: false,
      depcheck_enabled: null,
      questgiver_enabled: null,
      preview_enabled: true,
      preview_auto: 'ready_to_merge',
      preview_quests: true,
      wicket_enabled: null,
      wicket_auto_dispatch: false,
      max_smiths: 3,
      auto_dispatch: 'labeled',
      auto_dispatch_tag: 'forgeReady',
      auto_dispatch_min_priority: 2,
      platform: 'github',
      ...COMPOSITE_ANVIL_FIELDS,
    },
    metadata: {
      auto_merge: false,
      schematic_enabled: false,
      golangci_lint: null,
      go_race_detection: null,
      depcheck_enabled: null,
      questgiver_enabled: null,
      preview_enabled: null,
      preview_auto: '',
      preview_quests: false,
      wicket_enabled: null,
      wicket_auto_dispatch: true,
      max_smiths: 1,
      auto_dispatch: 'off',
      auto_dispatch_tag: '',
      auto_dispatch_min_priority: 0,
      platform: '',
      stage_providers: { smith: ['claude'] },
      wicket_trusted_users: ['octocat'],
      wicket_ignore_users: null,
      wicket_repos: null,
      wicket_issue_labels: null,
      wicket_triage_prompt: 'Prioritise security issues.',
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

  it('reflects the current value of each bool setting', () => {
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
    // schematic_enabled (in Pipeline) is non-hot-reloadable.
    expect(within(pipeline).getAllByText(/applies on next run/i).length).toBeGreaterThan(0)

    const smelter = screen.getByRole('region', { name: 'Smelter' })
    expect(within(smelter).queryByText(/applies on next run/i)).not.toBeInTheDocument()
  })

  it('PATCHes the toggled bool key', async () => {
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

  it('renders a global number field and PATCHes the typed number on blur', async () => {
    patchMock.mockResolvedValue(CONFIG)
    mockPoll({ data: CONFIG })
    renderPage()

    const input = screen.getByRole('spinbutton', { name: 'Max total smiths' })
    expect(input).toHaveValue(4)

    await userEvent.clear(input)
    await userEvent.type(input, '8')
    await userEvent.tab() // commit on blur

    expect(patchMock).toHaveBeenCalledWith({ max_total_smiths: 8 })
  })

  it('renders a global enum select and PATCHes the chosen option', async () => {
    patchMock.mockResolvedValue(CONFIG)
    mockPoll({ data: CONFIG })
    renderPage()

    const select = screen.getByRole('combobox', { name: 'Log level' })
    expect(select).toHaveValue('info')

    await userEvent.selectOptions(select, 'debug')

    expect(patchMock).toHaveBeenCalledWith({ log_level: 'debug' })
  })

  it('renders an error pane when the config fails to load', () => {
    mockPoll({ data: null, error: 'HTTP 500' })
    renderPage()
    expect(screen.getByText('HTTP 500')).toBeInTheDocument()
  })

  describe('composite & timing global controls', () => {
    it('renders a global string_list and PATCHes the appended item', async () => {
      patchMock.mockResolvedValue(CONFIG)
      mockPoll({ data: CONFIG })
      renderPage()

      const providers = screen.getByRole('group', { name: 'Provider chain' })
      const input = within(providers).getByRole('textbox', {
        name: /Add to Provider chain/i,
      })
      await userEvent.type(input, 'copilot{Enter}')

      expect(patchMock).toHaveBeenCalledWith({
        providers: ['claude', 'gemini', 'copilot'],
      })
    })

    it('renders a global provider_map grouped by stage', () => {
      mockPoll({ data: CONFIG })
      renderPage()

      const map = screen.getByRole('group', { name: 'Per-stage providers' })
      // Stages come from the key's options metadata.
      expect(within(map).getByText('smith')).toBeInTheDocument()
      expect(within(map).getByText('warden')).toBeInTheDocument()
    })

    it('collects duration settings into a collapsible Advanced / timing section', async () => {
      patchMock.mockResolvedValue(CONFIG)
      mockPoll({ data: CONFIG })
      renderPage()

      const section = screen.getByRole('region', { name: 'Advanced / timing' })
      // Collapsed by default: the duration inputs are not yet rendered.
      expect(
        screen.queryByRole('textbox', { name: 'Poll interval' }),
      ).not.toBeInTheDocument()

      // The section header is rendered outside the (hidden) body region.
      await userEvent.click(
        screen.getByRole('button', { name: /Advanced \/ timing/i }),
      )

      const input = within(section).getByRole('textbox', { name: 'Poll interval' })
      expect(input).toHaveValue('5m0s')

      await userEvent.clear(input)
      await userEvent.type(input, '10m')
      await userEvent.tab()

      expect(patchMock).toHaveBeenCalledWith({ poll_interval: '10m' })
    })

    it('rejects an invalid duration without PATCHing', async () => {
      patchMock.mockResolvedValue(CONFIG)
      mockPoll({ data: CONFIG })
      renderPage()

      await userEvent.click(
        screen.getByRole('button', { name: /Advanced \/ timing/i }),
      )
      const input = screen.getByRole('textbox', { name: 'Poll interval' })

      await userEvent.clear(input)
      await userEvent.type(input, 'not-a-duration')
      await userEvent.tab()

      expect(patchMock).not.toHaveBeenCalled()
      expect(input).toHaveAttribute('aria-invalid', 'true')
      expect(screen.getByText(/Invalid duration/i)).toBeInTheDocument()
    })
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

    it('renders a per-anvil scalar (max_smiths) and PATCHes the typed number', async () => {
      patchAnvilMock.mockResolvedValue({})
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const input = within(
        screen.getByRole('region', { name: 'heimdall' }),
      ).getByRole('spinbutton', { name: 'heimdall Max smiths' })
      expect(input).toHaveValue(3)

      await userEvent.clear(input)
      await userEvent.type(input, '6')
      await userEvent.tab()

      expect(patchAnvilMock).toHaveBeenCalledWith('heimdall', { max_smiths: 6 })
    })

    it('renders a per-anvil enum and PATCHes the chosen option', async () => {
      patchAnvilMock.mockResolvedValue({})
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const select = within(
        screen.getByRole('region', { name: 'heimdall' }),
      ).getByRole('combobox', { name: 'heimdall Auto-dispatch mode' })
      expect(select).toHaveValue('labeled')

      await userEvent.selectOptions(select, 'all')

      expect(patchAnvilMock).toHaveBeenCalledWith('heimdall', { auto_dispatch: 'all' })
    })

    it('PATCHes null when a per-anvil scalar Inherit reset is clicked', async () => {
      patchAnvilMock.mockResolvedValue({})
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const reset = within(
        screen.getByRole('region', { name: 'heimdall' }),
      ).getByRole('button', { name: /Reset heimdall Max smiths to inherit/i })
      await userEvent.click(reset)

      expect(patchAnvilMock).toHaveBeenCalledWith('heimdall', { max_smiths: null })
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

    it('renders a per-anvil string_list as inheriting when null and overrides it', async () => {
      patchAnvilMock.mockResolvedValue({})
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      // heimdall.wicket_trusted_users is null → ListField shows Override.
      const list = within(
        screen.getByRole('region', { name: 'heimdall' }),
      ).getByRole('group', { name: 'heimdall Wicket trusted users' })
      await userEvent.click(within(list).getByRole('button', { name: 'Override' }))

      expect(patchAnvilMock).toHaveBeenCalledWith('heimdall', {
        wicket_trusted_users: [],
      })
    })

    it('reflects an explicit per-anvil string_list and clears it to inherit', async () => {
      patchAnvilMock.mockResolvedValue({})
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const list = within(
        screen.getByRole('region', { name: 'metadata' }),
      ).getByRole('group', { name: 'metadata Wicket trusted users' })
      expect(within(list).getByText('octocat')).toBeInTheDocument()

      await userEvent.click(
        within(list).getByRole('button', {
          name: /Reset metadata Wicket trusted users to inherit/i,
        }),
      )
      expect(patchAnvilMock).toHaveBeenCalledWith('metadata', {
        wicket_trusted_users: null,
      })
    })

    it('renders a per-anvil multiline triage prompt and PATCHes the edited text', async () => {
      patchAnvilMock.mockResolvedValue({})
      mockPoll({ data: CONFIG_WITH_ANVILS })
      renderPage()

      const textarea = within(
        screen.getByRole('region', { name: 'metadata' }),
      ).getByRole('textbox', { name: 'metadata Wicket triage prompt' })
      expect(textarea).toHaveValue('Prioritise security issues.')

      await userEvent.clear(textarea)
      await userEvent.type(textarea, 'Focus on bugs.')
      await userEvent.tab()

      expect(patchAnvilMock).toHaveBeenCalledWith('metadata', {
        wicket_triage_prompt: 'Focus on bugs.',
      })
    })

    it('does not render a per-anvil section when no anvils are configured', () => {
      mockPoll({ data: CONFIG })
      renderPage()
      expect(screen.queryByText('Per-anvil overrides')).not.toBeInTheDocument()
    })
  })
})
