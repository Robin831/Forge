import { useCallback, useEffect, useRef, useState } from 'react'
import { RotateCcw } from 'lucide-react'
import ChainList, { reorder } from './ChainList'

// PROVIDER_STAGES is the default, ordered stage set for a provider_map value:
// the pipeline stages that accept their own provider chain. It mirrors the
// backend's providerStages (internal/web/forge_config.go) and is the fallback
// when the caller does not pass `stages` from the schema's Options metadata.
export const PROVIDER_STAGES = [
  'smith',
  'warden',
  'schematic',
  'cifix',
  'reviewfix',
]

// ProviderMap is the value shape of a `provider_map` setting (stage_providers):
// each stage maps to an ordered provider chain. A stage absent from the map has
// no override; an empty chain is never persisted (the backend rejects it), so
// clearing a stage removes its key.
export type ProviderMap = Record<string, string[]>

interface ProviderMapFieldProps {
  // value is the source-of-truth map from the parent. `null` means "inherit"
  // (the per-anvil override is unset); when `inheritable` is false it is treated
  // the same as an empty map.
  value: ProviderMap | null
  // onChange receives the full committed map, or `null` to clear a per-anvil
  // override (inherit). Stages with an empty chain are omitted so the payload
  // satisfies the backend's "at least one provider per stage" rule. It may
  // return a promise; while pending every control is disabled, and the
  // optimistic map reverts to `value` if it rejects.
  onChange: (next: ProviderMap | null) => void | Promise<void>
  // stages is the allowed, ordered stage set. Defaults to PROVIDER_STAGES; pass
  // the schema's Options so the editor renders exactly the backend's stages.
  stages?: string[]
  // inheritable enables the null=inherit semantics used by per-anvil overrides.
  inheritable?: boolean
  disabled?: boolean
  'aria-label'?: string
}

// ProviderMapField edits a `provider_map` value (stage_providers): one
// reorderable ChainList per pipeline stage. It commits the whole map on every
// mutation through the optimistic, disable-while-pending, revert-on-reject
// contract shared with the other Settings controls, omits emptied stages, and
// supports the per-anvil null=inherit semantics.
export default function ProviderMapField({
  value,
  onChange,
  stages = PROVIDER_STAGES,
  inheritable,
  disabled,
  'aria-label': ariaLabel,
}: ProviderMapFieldProps) {
  const [optimistic, setOptimistic] = useState<ProviderMap | null>(value)
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  const pendingRef = useRef(false)

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  // Re-sync from props only while idle so a poll can't clobber an in-flight edit.
  useEffect(() => {
    if (!pendingRef.current) setOptimistic(value)
  }, [value])

  const isDisabled = disabled || pending

  const commit = useCallback(
    (next: ProviderMap | null) => {
      if (disabled || pending) return
      const previous = optimistic
      setOptimistic(next)
      pendingRef.current = true
      setPending(true)
      Promise.resolve()
        .then(() => onChange(next))
        .catch(() => {
          if (mounted.current) setOptimistic(previous)
        })
        .finally(() => {
          if (mounted.current) {
            pendingRef.current = false
            setPending(false)
          }
        })
    },
    [disabled, pending, optimistic, onChange],
  )

  // setStage rebuilds the map with `chain` for one stage, dropping the key when
  // the chain is empty so an empty stage is never sent to the backend.
  const setStage = useCallback(
    (stage: string, chain: string[]) => {
      const base = optimistic ?? {}
      const next: ProviderMap = { ...base }
      if (chain.length === 0) delete next[stage]
      else next[stage] = chain
      commit(next)
    },
    [optimistic, commit],
  )

  const map = optimistic ?? {}
  const inheriting = inheritable === true && optimistic === null

  if (inheriting) {
    return (
      <div role="group" aria-label={ariaLabel} className="flex items-center gap-2">
        <span className="text-xs italic text-slate-500">
          Inherits global / default
        </span>
        <button
          type="button"
          disabled={isDisabled}
          onClick={() => commit({})}
          className="rounded-md border border-slate-700 px-2 py-1 text-xs font-medium text-slate-300 transition-colors hover:border-emerald-400/60 hover:text-slate-100 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
        >
          Override
        </button>
      </div>
    )
  }

  return (
    <div role="group" aria-label={ariaLabel} className="flex flex-col gap-3">
      {stages.map((stage) => {
        const chain = map[stage] ?? []
        return (
          <div key={stage} className="flex flex-col gap-1.5">
            <span className="text-xs font-medium uppercase tracking-wide text-slate-400">
              {stage}
            </span>
            <ChainList
              items={chain}
              disabled={isDisabled}
              placeholder="provider (e.g. claude)"
              addLabel={`Add provider for ${stage}`}
              idPrefix={`provider-${stage}`}
              onAdd={(v) => setStage(stage, [...chain, v])}
              onRemove={(i) => setStage(stage, chain.filter((_, j) => j !== i))}
              onMove={(i, dir) => setStage(stage, reorder(chain, i, dir))}
            />
          </div>
        )
      })}
      {inheritable && (
        <button
          type="button"
          disabled={isDisabled}
          onClick={() => commit(null)}
          aria-label={`Reset ${ariaLabel ?? 'stage providers'} to inherit`}
          title="Reset to inherit the global / default value"
          className="inline-flex w-fit items-center gap-1 rounded-md border border-slate-700 px-2 py-1 text-xs font-medium text-slate-400 transition-colors hover:border-slate-600 hover:text-slate-200 focus:outline-none focus-visible:ring-1 focus-visible:ring-emerald-400 disabled:cursor-not-allowed disabled:opacity-50"
        >
          <RotateCcw size={12} aria-hidden />
          Inherit
        </button>
      )}
    </div>
  )
}
