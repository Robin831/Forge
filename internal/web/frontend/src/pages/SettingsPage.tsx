import { useMemo, useState } from 'react'
import {
  Beaker,
  Clock,
  FlaskConical,
  GitBranch,
  Hammer,
  RotateCcw,
  Settings as SettingsIcon,
  ShieldCheck,
  Sparkles,
  Workflow,
} from 'lucide-react'
import { useApiPoll } from '../hooks/useApiPoll'
import { useToast } from '../hooks/useToast'
import { forgeConfig } from '../api'
import type {
  AnvilKeyInfo,
  AnvilSettings,
  ConfigKeyInfo,
  ConfigResponse,
  ConfigValue,
  ConfigValueType,
  StatusResponse,
} from '../api'
import AppHeader from '../components/AppHeader'
import Pane, { EmptyState } from '../components/Pane'
import Switch from '../components/Switch'
import TriStateToggle from '../components/TriStateToggle'
import type { TriState } from '../components/TriStateToggle'
import NumberField from '../components/NumberField'
import TextField from '../components/TextField'
import TextAreaField from '../components/TextAreaField'
import SelectField from '../components/SelectField'
import ListField from '../components/ListField'
import ProviderMapField from '../components/ProviderMapField'
import DurationField from '../components/DurationField'

const POLL_INTERVAL_MS = 30000

// ADVANCED_TIMING_AREA is the synthetic section that collects every duration
// setting, regardless of the backend `area` it is tagged with, into a single
// collapsible "Advanced / timing" group so the interval/timeout knobs don't
// clutter the primary per-feature sections.
const ADVANCED_TIMING_AREA = 'Advanced / timing'

// LABELLABLE_TYPES are the value types whose control is a single focusable input
// that can be associated with a <label htmlFor>. Composite editors (string_list,
// provider_map) render a group of inputs instead, so their row uses a plain
// <span> caption rather than a label.
const LABELLABLE_TYPES = new Set<ConfigValueType>([
  'int',
  'float',
  'enum',
  'string',
  'duration',
])

// MULTILINE_STRING_KEYS marks `string` settings that should render as a textarea
// rather than a single-line input. The backend exposes these as plain strings;
// this is the frontend's render-time hint for long-form values (e.g. the Wicket
// triage prompt suffix).
const MULTILINE_STRING_KEYS = new Set<string>(['wicket_triage_prompt'])

function isMultilineString(type: ConfigValueType, key: string): boolean {
  return type === 'string' && MULTILINE_STRING_KEYS.has(key)
}

function isLabellable(type: ConfigValueType): boolean {
  return LABELLABLE_TYPES.has(type)
}

// AREA_ICON maps the backend `area` strings (see internal/web/forge_config.go)
// to a lucide icon for the section header. Unknown areas fall back to the
// generic settings icon so a newly-added area still renders cleanly.
const AREA_ICON: Record<string, typeof SettingsIcon> = {
  Pipeline: Workflow,
  Temper: Hammer,
  Warden: ShieldCheck,
  Crucible: FlaskConical,
  Copilot: Sparkles,
  Vulncheck: ShieldCheck,
  Smelter: Beaker,
}

function areaIcon(area: string): typeof SettingsIcon {
  return AREA_ICON[area] ?? SettingsIcon
}

interface AreaGroup {
  area: string
  keys: ConfigKeyInfo[]
}

// groupByArea buckets keys into sections, preserving the server's key order and
// the first-seen order of areas so rendering is deterministic.
function groupByArea(keys: ConfigKeyInfo[]): AreaGroup[] {
  const groups: AreaGroup[] = []
  const index = new Map<string, AreaGroup>()
  for (const k of keys) {
    let group = index.get(k.area)
    if (!group) {
      group = { area: k.area, keys: [] }
      index.set(k.area, group)
      groups.push(group)
    }
    group.keys.push(k)
  }
  return groups
}

// AppliesNote renders the "applies on next run" hint shown for non-hot-reloadable
// keys, matching the styling established for the global settings list.
function AppliesNote() {
  return (
    <p className="mt-1 inline-flex items-center gap-1 text-[11px] text-amber-400/70">
      <GitBranch size={11} aria-hidden />
      Applies on next run
    </p>
  )
}

// InheritButton is the small reset affordance shown next to per-anvil scalar
// controls (number / enum / string). Sending `null` clears the override so the
// anvil falls back to the global/default. Tri-state bools already expose
// "Inherit" in their toggle, so this is only rendered for non-bool scalars.
function InheritButton({
  onClick,
  label,
  disabled,
}: {
  onClick: () => void
  label: string
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      aria-label={label}
      title="Reset to inherit the global / default value"
      className="inline-flex items-center gap-1 rounded-md border border-slate-700 px-2 py-1 text-xs font-medium text-slate-400 transition-colors hover:border-slate-600 hover:text-slate-200 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
    >
      <RotateCcw size={12} aria-hidden />
      Inherit
    </button>
  )
}

export default function SettingsPage() {
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const config = useApiPoll<ConfigResponse>('/api/forge/config', POLL_INTERVAL_MS)
  const toast = useToast()

  const configData = config.data

  // Duration settings are pulled out of their per-feature areas and collected
  // into a single "Advanced / timing" section; everything else groups by area.
  const { groups, durationKeys } = useMemo(() => {
    const keys = configData?.keys ?? []
    const durations = keys.filter((k) => k.type === 'duration')
    const rest = keys.filter((k) => k.type !== 'duration')
    return { groups: groupByArea(rest), durationKeys: durations }
  }, [configData])

  // The timing section is collapsed by default — these are advanced knobs most
  // users never touch.
  const [timingExpanded, setTimingExpanded] = useState(false)

  const anvilKeys = configData?.anvilKeys ?? []

  // anvilEntries is the configured anvils sorted by name so the per-anvil
  // sections render in a stable, predictable order.
  const anvilEntries = useMemo(
    () =>
      Object.entries(configData?.anvils ?? {}).sort(([a], [b]) =>
        a.localeCompare(b),
      ),
    [configData],
  )

  // handleGlobalChange writes a single global key. The control owns its own
  // optimistic state and reverts on a rejected promise, so this just surfaces
  // the daemon's message on failure and re-throws to trigger that revert. The
  // next poll reconciles the displayed value with the persisted config.
  const handleGlobalChange = async (key: string, next: ConfigValue) => {
    try {
      await forgeConfig.patch({ [key]: next })
    } catch (err) {
      const message =
        err instanceof Error ? err.message : 'failed to update setting'
      toast.push(message, 'error')
      throw err
    }
  }

  // handleAnvilChange writes a single per-anvil key. `next` is the typed value,
  // or `null` to clear the override (inherit the global/default) for tri-state
  // bools and every non-bool scalar. The control reverts its own optimistic
  // state on rejection; we surface the daemon message and re-throw.
  const handleAnvilChange = async (
    anvil: string,
    key: string,
    next: ConfigValue | null,
  ) => {
    try {
      await forgeConfig.patchAnvil(anvil, { [key]: next })
    } catch (err) {
      const message =
        err instanceof Error ? err.message : 'failed to update setting'
      toast.push(message, 'error')
      throw err
    }
  }

  const totalKeys = config.data?.keys.length ?? 0

  return (
    <div className="flex min-h-full flex-col gap-6 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <div>
        <h2 className="text-lg font-semibold text-slate-100">Settings</h2>
        <p className="mt-1 text-sm text-slate-400">
          Tune global pipeline behaviour, then override individual settings per
          anvil below. Changes are written to your Forge config file
          (<code>forge.yaml</code>).
        </p>
      </div>

      {config.error && totalKeys === 0 ? (
        <Pane
          title="Configuration"
          icon={<SettingsIcon size={16} className="text-amber-400" aria-hidden />}
          loading={config.loading}
          error={config.error}
          children={null}
        />
      ) : groups.length === 0 && durationKeys.length === 0 ? (
        <Pane
          title="Configuration"
          icon={<SettingsIcon size={16} className="text-amber-400" aria-hidden />}
          loading={config.loading}
        >
          <EmptyState message="No configurable settings available." />
        </Pane>
      ) : (
        <>
          {groups.map((group) => {
            const Icon = areaIcon(group.area)
            return (
              <Pane
                key={group.area}
                title={group.area}
                icon={<Icon size={16} className="text-amber-400" aria-hidden />}
                count={group.keys.length}
                loading={config.loading}
              >
                <ul className="divide-y divide-slate-800">
                  {group.keys.map((k) => (
                    <GlobalRow
                      key={k.key}
                      info={k}
                      onChange={(next) => handleGlobalChange(k.key, next)}
                    />
                  ))}
                </ul>
              </Pane>
            )
          })}

          {durationKeys.length > 0 && (
            <Pane
              title={ADVANCED_TIMING_AREA}
              icon={<Clock size={16} className="text-amber-400" aria-hidden />}
              count={durationKeys.length}
              loading={config.loading}
              collapsible
              expanded={timingExpanded}
              onToggle={() => setTimingExpanded((v) => !v)}
            >
              <ul className="divide-y divide-slate-800">
                {durationKeys.map((k) => (
                  <GlobalRow
                    key={k.key}
                    info={k}
                    onChange={(next) => handleGlobalChange(k.key, next)}
                  />
                ))}
              </ul>
            </Pane>
          )}
        </>
      )}

      {anvilEntries.length > 0 && (
        <div>
          <h3 className="text-base font-semibold text-slate-100">
            Per-anvil overrides
          </h3>
          <p className="mt-1 text-sm text-slate-400">
            Override individual settings per repository. Tri-state controls show{' '}
            <span className="font-medium text-slate-300">Inherit</span> when the
            anvil follows the global value; scalar controls expose an{' '}
            <span className="font-medium text-slate-300">Inherit</span> reset.
          </p>
        </div>
      )}

      {anvilEntries.map(([name, settings]) => (
        <Pane
          key={name}
          title={name}
          icon={<Hammer size={16} className="text-amber-400" aria-hidden />}
          count={anvilKeys.length}
          loading={config.loading}
        >
          <ul className="divide-y divide-slate-800">
            {anvilKeys.map((meta) => (
              <AnvilRow
                key={meta.key}
                anvil={name}
                meta={meta}
                settings={settings}
                onChange={(next) => handleAnvilChange(name, meta.key, next)}
              />
            ))}
          </ul>
        </Pane>
      ))}

      <footer className="text-center text-xs text-slate-500">
        Polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>
    </div>
  )
}

// GlobalRow renders one global key: label + description (+ applies-on-next-run
// note for non-hot-reloadable keys) and a control chosen by `type`.
function GlobalRow({
  info,
  onChange,
}: {
  info: ConfigKeyInfo
  onChange: (next: ConfigValue) => void | Promise<void>
}) {
  const controlId = `setting-${info.key}`
  // bool and composite (string_list / provider_map) controls bind via aria-label
  // rather than htmlFor, so only the single-input types get a <label> association.
  const labellable = isLabellable(info.type)

  return (
    <li className="flex items-start justify-between gap-4 px-4 py-3">
      <div className="min-w-0">
        {labellable ? (
          <label
            htmlFor={controlId}
            className="block text-sm font-medium text-slate-200"
          >
            {info.label}
          </label>
        ) : (
          <span className="block text-sm font-medium text-slate-200">
            {info.label}
          </span>
        )}
        <p className="mt-0.5 text-xs text-slate-500">{info.description}</p>
        {!info.hotReloadable && <AppliesNote />}
      </div>
      <div className="shrink-0 pt-0.5">
        <GlobalControl info={info} controlId={controlId} onChange={onChange} />
      </div>
    </li>
  )
}

function GlobalControl({
  info,
  controlId,
  onChange,
}: {
  info: ConfigKeyInfo
  controlId: string
  onChange: (next: ConfigValue) => void | Promise<void>
}) {
  switch (info.type) {
    case 'bool':
      return (
        <Switch
          id={controlId}
          checked={info.value === true}
          onChange={(next) => onChange(next)}
          aria-label={info.label}
        />
      )
    case 'int':
    case 'float':
      return (
        <NumberField
          id={controlId}
          value={typeof info.value === 'number' ? info.value : Number(info.value)}
          min={info.min}
          max={info.max}
          step={info.type === 'int' ? 1 : 'any'}
          unit={info.unit}
          onChange={(next) => onChange(next)}
          aria-label={info.label}
        />
      )
    case 'enum':
      return (
        <SelectField
          id={controlId}
          value={String(info.value)}
          options={info.options ?? []}
          onChange={(next) => onChange(next)}
          aria-label={info.label}
        />
      )
    case 'duration':
      return (
        <DurationField
          id={controlId}
          value={typeof info.value === 'string' ? info.value : String(info.value ?? '')}
          onChange={(next) => onChange(next)}
          aria-label={info.label}
        />
      )
    case 'string_list':
      return (
        <ListField
          value={Array.isArray(info.value) ? (info.value as string[]) : []}
          onChange={(next) => onChange(next ?? [])}
          addLabel={`Add to ${info.label}`}
          idPrefix={info.key}
          aria-label={info.label}
        />
      )
    case 'provider_map':
      return (
        <ProviderMapField
          value={
            info.value && typeof info.value === 'object' && !Array.isArray(info.value)
              ? (info.value as Record<string, string[]>)
              : {}
          }
          stages={info.options}
          onChange={(next) => onChange(next ?? {})}
          aria-label={info.label}
        />
      )
    case 'string':
      if (isMultilineString(info.type, info.key)) {
        return (
          <TextAreaField
            id={controlId}
            value={String(info.value ?? '')}
            onChange={(next) => onChange(next)}
            aria-label={info.label}
          />
        )
      }
      return (
        <TextField
          id={controlId}
          value={String(info.value)}
          onChange={(next) => onChange(next)}
          aria-label={info.label}
        />
      )
    default:
      return (
        <TextField
          id={controlId}
          value={String(info.value)}
          onChange={(next) => onChange(next)}
          aria-label={info.label}
        />
      )
  }
}

// AnvilRow renders one per-anvil key for one anvil, choosing the control by the
// schema's `type` and reading the current value from the anvil's settings.
function AnvilRow({
  anvil,
  meta,
  settings,
  onChange,
}: {
  anvil: string
  meta: AnvilKeyInfo
  settings: AnvilSettings
  onChange: (next: ConfigValue | null) => void | Promise<void>
}) {
  const controlId = `anvil-${anvil}-${meta.key}`
  const raw = settings[meta.key as keyof AnvilSettings]
  // bool and composite (string_list / provider_map) controls bind via aria-label.
  const labellable = isLabellable(meta.type)

  return (
    <li className="flex items-start justify-between gap-4 px-4 py-3">
      <div className="min-w-0">
        {labellable ? (
          <label
            htmlFor={controlId}
            className="block text-sm font-medium text-slate-200"
          >
            {meta.label}
          </label>
        ) : (
          <span className="block text-sm font-medium text-slate-200">
            {meta.label}
          </span>
        )}
        <p className="mt-0.5 text-xs text-slate-500">{meta.description}</p>
        {!meta.hotReloadable && <AppliesNote />}
      </div>
      <div className="flex shrink-0 items-center gap-2 pt-0.5">
        <AnvilControl
          anvil={anvil}
          meta={meta}
          controlId={controlId}
          raw={raw}
          onChange={onChange}
        />
      </div>
    </li>
  )
}

function AnvilControl({
  anvil,
  meta,
  controlId,
  raw,
  onChange,
}: {
  anvil: string
  meta: AnvilKeyInfo
  controlId: string
  raw: AnvilSettings[keyof AnvilSettings]
  onChange: (next: ConfigValue | null) => void | Promise<void>
}) {
  const ariaLabel = `${anvil} ${meta.label}`

  if (meta.type === 'bool') {
    if (meta.triState) {
      const value = (raw ?? null) as TriState
      return (
        <TriStateToggle
          value={value}
          onChange={(next) => onChange(next)}
          aria-label={ariaLabel}
        />
      )
    }
    return (
      <Switch
        id={controlId}
        checked={raw === true}
        onChange={(next) => onChange(next)}
        aria-label={ariaLabel}
      />
    )
  }

  if (meta.type === 'int' || meta.type === 'float') {
    const value = typeof raw === 'number' ? raw : Number(raw ?? 0)
    return (
      <>
        <NumberField
          id={controlId}
          value={value}
          min={meta.min}
          max={meta.max}
          step={meta.type === 'int' ? 1 : 'any'}
          onChange={(next) => onChange(next)}
          aria-label={ariaLabel}
        />
        <InheritButton
          onClick={() => onChange(null)}
          label={`Reset ${ariaLabel} to inherit`}
        />
      </>
    )
  }

  if (meta.type === 'enum') {
    return (
      <>
        <SelectField
          id={controlId}
          value={String(raw ?? '')}
          options={meta.options ?? []}
          onChange={(next) => onChange(next)}
          aria-label={ariaLabel}
        />
        <InheritButton
          onClick={() => onChange(null)}
          label={`Reset ${ariaLabel} to inherit`}
        />
      </>
    )
  }

  if (meta.type === 'string_list') {
    // ListField owns its own null=inherit affordance (an "Override" placeholder
    // when null, an "Inherit" reset when set), so no separate InheritButton.
    const value = (raw ?? null) as string[] | null
    return (
      <ListField
        value={value}
        inheritable
        onChange={(next) => onChange(next)}
        addLabel={`Add to ${meta.label}`}
        idPrefix={`${anvil}-${meta.key}`}
        aria-label={ariaLabel}
      />
    )
  }

  if (meta.type === 'provider_map') {
    const value = (raw ?? null) as Record<string, string[]> | null
    return (
      <ProviderMapField
        value={value}
        stages={meta.options}
        inheritable
        onChange={(next) => onChange(next)}
        aria-label={ariaLabel}
      />
    )
  }

  // string. Long-form keys (e.g. the Wicket triage prompt) render as a textarea;
  // the rest use a single-line input. Both clear the override via InheritButton.
  if (isMultilineString(meta.type, meta.key)) {
    return (
      <>
        <TextAreaField
          id={controlId}
          value={String(raw ?? '')}
          onChange={(next) => onChange(next)}
          aria-label={ariaLabel}
        />
        <InheritButton
          onClick={() => onChange(null)}
          label={`Reset ${ariaLabel} to inherit`}
        />
      </>
    )
  }

  return (
    <>
      <TextField
        id={controlId}
        value={String(raw ?? '')}
        onChange={(next) => onChange(next)}
        aria-label={ariaLabel}
      />
      <InheritButton
        onClick={() => onChange(null)}
        label={`Reset ${ariaLabel} to inherit`}
      />
    </>
  )
}
