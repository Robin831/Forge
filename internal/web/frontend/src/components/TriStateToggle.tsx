import { useCallback, useEffect, useRef, useState } from 'react'

export type TriState = boolean | null

interface TriStateToggleProps {
  value: TriState
  onChange: (next: TriState) => void | Promise<void>
  disabled?: boolean
  'aria-label'?: string
  'aria-labelledby'?: string
}

const OPTIONS: { label: string; value: TriState }[] = [
  { label: 'Inherit', value: null },
  { label: 'On', value: true },
  { label: 'Off', value: false },
]

function optionKey(value: TriState): string {
  return value === null ? 'inherit' : value ? 'on' : 'off'
}

function activeIndex(value: TriState): number {
  return OPTIONS.findIndex((o) => o.value === value)
}

export default function TriStateToggle({
  value,
  onChange,
  disabled,
  'aria-label': ariaLabel,
  'aria-labelledby': ariaLabelledBy,
}: TriStateToggleProps) {
  const [optimistic, setOptimistic] = useState<TriState>(value)
  const [pending, setPending] = useState(false)
  const mounted = useRef(true)
  const pendingRef = useRef(false)
  const buttonRefs = useRef<(HTMLButtonElement | null)[]>([])

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  useEffect(() => {
    if (!pendingRef.current) setOptimistic(value)
  }, [value])

  const isDisabled = disabled || pending

  const select = useCallback(
    (next: TriState) => {
      if (disabled || pending || next === optimistic) return
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

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent, index: number) => {
      let nextIndex: number | null = null
      if (e.key === 'ArrowRight' || e.key === 'ArrowDown') {
        nextIndex = (index + 1) % OPTIONS.length
      } else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') {
        nextIndex = (index - 1 + OPTIONS.length) % OPTIONS.length
      } else if (e.key === 'Home') {
        nextIndex = 0
      } else if (e.key === 'End') {
        nextIndex = OPTIONS.length - 1
      }
      if (nextIndex !== null) {
        e.preventDefault()
        buttonRefs.current[nextIndex]?.focus()
        select(OPTIONS[nextIndex].value)
      }
    },
    [select],
  )

  const current = activeIndex(optimistic)

  return (
    <div
      role="radiogroup"
      aria-label={ariaLabel}
      aria-labelledby={ariaLabelledBy}
      className="inline-flex shrink-0 rounded-md border border-slate-700 bg-slate-800/60 p-0.5"
    >
      {OPTIONS.map((opt, i) => {
        const active = optimistic === opt.value
        return (
          <button
            key={optionKey(opt.value)}
            ref={(el) => { buttonRefs.current[i] = el }}
            type="button"
            role="radio"
            aria-checked={active}
            tabIndex={i === current ? 0 : -1}
            disabled={isDisabled}
            onClick={() => select(opt.value)}
            onKeyDown={(e) => handleKeyDown(e, i)}
            className={`rounded px-2.5 py-1 text-xs font-medium transition-colors duration-150 focus:outline-none focus-visible:ring-2 focus-visible:ring-emerald-400 focus-visible:ring-offset-1 focus-visible:ring-offset-slate-900 disabled:cursor-not-allowed disabled:opacity-50 ${
              active
                ? 'bg-emerald-500 text-white shadow-sm'
                : 'text-slate-300 hover:text-slate-100'
            }`}
          >
            {opt.label}
          </button>
        )
      })}
    </div>
  )
}
