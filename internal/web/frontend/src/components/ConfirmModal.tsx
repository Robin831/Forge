import { useEffect, useRef, useState } from 'react'

export type ConfirmTone = 'danger' | 'primary'

export interface ConfirmModalProps {
  open: boolean
  title: string
  message: string
  confirmLabel?: string
  cancelLabel?: string
  tone?: ConfirmTone
  // When set, renders a single-line input field whose value is passed back to
  // onConfirm. Used by clarify / label / note flows where the user supplies a
  // short string before confirming.
  inputLabel?: string
  inputPlaceholder?: string
  inputDefault?: string
  busy?: boolean
  onConfirm: (input: string) => void | Promise<void>
  onCancel: () => void
}

const TONE_BUTTON: Record<ConfirmTone, string> = {
  danger:
    'bg-red-600 hover:bg-red-500 focus-visible:ring-red-400 text-white',
  primary:
    'bg-amber-500 hover:bg-amber-400 focus-visible:ring-amber-300 text-slate-900',
}

export default function ConfirmModal({
  open,
  title,
  message,
  confirmLabel = 'Confirm',
  cancelLabel = 'Cancel',
  tone = 'primary',
  inputLabel,
  inputPlaceholder,
  inputDefault = '',
  busy = false,
  onConfirm,
  onCancel,
}: ConfirmModalProps) {
  const [input, setInput] = useState(inputDefault)
  const inputRef = useRef<HTMLInputElement | null>(null)
  const cancelRef = useRef<HTMLButtonElement | null>(null)

  useEffect(() => {
    if (open) {
      setInput(inputDefault)
    }
  }, [open, inputDefault])

  useEffect(() => {
    if (!open) return
    // Focus the text input when present, otherwise the cancel button so
    // keyboard users can dismiss with Enter.
    const t = window.setTimeout(() => {
      if (inputLabel && inputRef.current) {
        inputRef.current.focus()
        inputRef.current.select()
      } else if (cancelRef.current) {
        cancelRef.current.focus()
      }
    }, 10)
    return () => window.clearTimeout(t)
  }, [open, inputLabel])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) {
        onCancel()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, busy, onCancel])

  if (!open) return null

  const handleConfirm = () => {
    if (busy) return
    void onConfirm(input)
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="confirm-modal-title"
      className="fixed inset-0 z-50 flex items-center justify-center bg-slate-950/70 p-4"
    >
      <div className="w-full max-w-md rounded-xl border border-slate-800 bg-slate-900 p-5 shadow-xl">
        <h2
          id="confirm-modal-title"
          className="text-base font-semibold text-slate-100"
        >
          {title}
        </h2>
        <p className="mt-2 whitespace-pre-wrap text-sm text-slate-300">{message}</p>
        {inputLabel && (
          <label className="mt-4 block text-xs font-medium text-slate-400">
            {inputLabel}
            <input
              ref={inputRef}
              type="text"
              value={input}
              onChange={(e) => setInput(e.target.value)}
              placeholder={inputPlaceholder}
              disabled={busy}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  handleConfirm()
                }
              }}
              className="mt-1 w-full rounded-md border border-slate-700 bg-slate-950 px-3 py-2 text-sm text-slate-100 focus:border-amber-400 focus:outline-none focus:ring-1 focus:ring-amber-400/50 disabled:opacity-50"
            />
          </label>
        )}
        <div className="mt-5 flex justify-end gap-2">
          <button
            ref={cancelRef}
            type="button"
            onClick={onCancel}
            disabled={busy}
            className="rounded-md border border-slate-700 bg-slate-800 px-3 py-1.5 text-sm text-slate-200 hover:bg-slate-700 focus:outline-none focus-visible:ring-2 focus-visible:ring-slate-400 disabled:opacity-50"
          >
            {cancelLabel}
          </button>
          <button
            type="button"
            onClick={handleConfirm}
            disabled={busy}
            className={`rounded-md px-3 py-1.5 text-sm font-medium focus:outline-none focus-visible:ring-2 disabled:opacity-50 ${TONE_BUTTON[tone]}`}
          >
            {busy ? 'Working…' : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}
