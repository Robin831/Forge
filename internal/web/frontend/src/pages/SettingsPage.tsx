import { useEffect, useMemo, useState } from 'react'
import {
  Beaker,
  FlaskConical,
  GitBranch,
  Hammer,
  Settings as SettingsIcon,
  ShieldCheck,
  Sparkles,
  Workflow,
} from 'lucide-react'
import { useApiPoll } from '../hooks/useApiPoll'
import { useToast } from '../hooks/useToast'
import { forgeConfig } from '../api'
import type { ConfigKeyInfo, ConfigResponse, StatusResponse } from '../api'
import AppHeader from '../components/AppHeader'
import Pane, { EmptyState } from '../components/Pane'
import Switch from '../components/Switch'

const POLL_INTERVAL_MS = 30000

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

export default function SettingsPage() {
  const status = useApiPoll<StatusResponse>('/api/status', POLL_INTERVAL_MS)
  const config = useApiPoll<ConfigResponse>('/api/forge/config', POLL_INTERVAL_MS)
  const toast = useToast()

  // overrides holds optimistic values for keys mid-flight (and just after) so
  // the displayed state reflects a toggle immediately, before the next poll
  // confirms it. An entry is cleared once a poll reports the same value, or on
  // PATCH failure (revert).
  const [overrides, setOverrides] = useState<Record<string, boolean>>({})

  const configData = config.data
  useEffect(() => {
    if (!configData) return
    setOverrides((current) => {
      let changed = false
      const next = { ...current }
      for (const k of configData.keys) {
        if (k.key in next && next[k.key] === k.value) {
          delete next[k.key]
          changed = true
        }
      }
      return changed ? next : current
    })
  }, [configData])

  const groups = useMemo(
    () => groupByArea(configData?.keys ?? []),
    [configData],
  )

  const handleToggle = async (key: string, next: boolean) => {
    setOverrides((current) => ({ ...current, [key]: next }))
    try {
      await forgeConfig.patch({ [key]: next })
      // Leave the override in place; the next poll reconciles it once the
      // re-read config reflects the change.
    } catch (err) {
      // Revert the optimistic override and surface the daemon's message. Re-throw
      // so the Switch also rolls its own optimistic state back.
      setOverrides((current) => {
        const reverted = { ...current }
        delete reverted[key]
        return reverted
      })
      const message = err instanceof Error ? err.message : 'failed to update setting'
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
          Toggle global pipeline behaviour. Changes are written to your Forge
          config file. Per-anvil overrides are managed in <code>forge.yaml</code>.
        </p>
      </div>

      {config.error && totalKeys === 0 ? (
        <Pane
          title="Configuration"
          icon={<SettingsIcon size={16} className="text-amber-400" aria-hidden />}
          loading={config.loading}
          error={config.error}
        >
          <EmptyState message="Configuration unavailable." />
        </Pane>
      ) : groups.length === 0 ? (
        <Pane
          title="Configuration"
          icon={<SettingsIcon size={16} className="text-amber-400" aria-hidden />}
          loading={config.loading}
        >
          <EmptyState message="No configurable settings available." />
        </Pane>
      ) : (
        groups.map((group) => {
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
                {group.keys.map((k) => {
                  const value = overrides[k.key] ?? k.value
                  const switchId = `setting-${k.key}`
                  return (
                    <li
                      key={k.key}
                      className="flex items-start justify-between gap-4 px-4 py-3"
                    >
                      <div className="min-w-0">
                        <label
                          htmlFor={switchId}
                          className="block text-sm font-medium text-slate-200"
                        >
                          {k.label}
                        </label>
                        <p className="mt-0.5 text-xs text-slate-500">{k.description}</p>
                        {!k.hotReloadable && (
                          <p className="mt-1 inline-flex items-center gap-1 text-[11px] text-amber-400/70">
                            <GitBranch size={11} aria-hidden />
                            Applies on next run
                          </p>
                        )}
                      </div>
                      <div className="shrink-0 pt-0.5">
                        <Switch
                          id={switchId}
                          checked={value}
                          onChange={(nextVal) => handleToggle(k.key, nextVal)}
                          aria-label={k.label}
                        />
                      </div>
                    </li>
                  )
                })}
              </ul>
            </Pane>
          )
        })
      )}

      <footer className="text-center text-xs text-slate-500">
        Polled every {POLL_INTERVAL_MS / 1000}s · Hearth 2.0
      </footer>
    </div>
  )
}
