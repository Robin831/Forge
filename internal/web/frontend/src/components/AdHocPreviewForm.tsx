import { Play, Sparkles } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { usePreview } from '../hooks/usePreview'
import { useToast } from '../hooks/useToast'

// validPreviewID mirrors internal/web/views.go's isValidBeadID: the {bead_id}
// path segment must start alphanumeric and carry nothing but letters, digits,
// dots, hyphens and underscores. Checking it here turns a 400 from the server
// into a message beside the field that says what to type instead.
const validPreviewID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/

export interface AdHocPreviewFormProps {
  /** The anvils a preview can be started for — `anvils` from the previews payload. */
  anvils: string[]
}

// AdHocPreviewForm is the browser half of `forge preview start <id> --anvil
// <name> [--branch <branch>]`: start a preview for a branch that no worker owns.
//
// It exists because a preview id is a *registry key*, not a bd lookup — the
// daemon never resolves it as an issue — so any branch can be previewed under
// any id. That is what makes it usable for smoke-testing a new manifest or
// looking at a branch that has no bead yet; such previews conventionally use
// ids like `kiln-smoke-1`.
//
// It adds no polling of its own: the start goes through the same usePreview
// state machine every other preview control uses, so the new environment
// appears in the fleet list below via the snapshot they all share.
export default function AdHocPreviewForm({ anvils }: AdHocPreviewFormProps) {
  const toast = useToast()
  const [beadId, setBeadId] = useState('')
  const [anvil, setAnvil] = useState('')
  const [branch, setBranch] = useState('')
  const [formError, setFormError] = useState<string | null>(null)
  // The daemon's refusal of *our* start, kept apart from the hook's `error`:
  // that one also reports a pre-existing failed preview filed under the typed
  // id, which is not something this form did.
  const [startError, setStartError] = useState<string | null>(null)

  // With a single previewable anvil there is nothing to choose, so choose it.
  // The placeholder option is dropped in that case too — re-selecting it would
  // only be undone by this effect.
  const onlyAnvil = anvils.length === 1 ? anvils[0] : ''
  useEffect(() => {
    if (onlyAnvil) setAnvil(onlyAnvil)
  }, [onlyAnvil])

  const trimmedId = beadId.trim()
  const { preview, isBusy, start } = usePreview(trimmedId, {
    anvil: anvil || undefined,
    branch,
    onError: (message) => {
      setStartError(message)
      toast.push(message, 'error')
    },
    onQueued: () => toast.push(`Starting preview for ${trimmedId}…`, 'info'),
  })

  // Clear the fields once a start settles without an error — the preview is now
  // a row in the list below, and the form is ready for the next one. A failed
  // start keeps what was typed so it can be corrected and retried.
  const wasBusy = useRef(false)
  useEffect(() => {
    const settled = wasBusy.current && !isBusy
    wasBusy.current = isBusy
    if (settled && !startError) {
      setBeadId('')
      setBranch('')
    }
  }, [isBusy, startError])

  // A preview is already filed under this id: starting again would hand back
  // that environment rather than the branch just typed, so say so instead. Our
  // own start is excluded — it has its own in-flight line.
  const taken = !isBusy && trimmedId !== '' && preview !== null
  const canSubmit = trimmedId !== '' && anvil !== '' && !taken && !isBusy

  const submit = () => {
    setFormError(null)
    setStartError(null)
    if (!trimmedId) {
      setFormError('A preview id is required.')
      return
    }
    if (!validPreviewID.test(trimmedId)) {
      setFormError(
        'A preview id must start with a letter or digit and use only letters, digits, dots, hyphens and underscores.',
      )
      return
    }
    if (!anvil) {
      setFormError('Choose an anvil to start the preview in.')
      return
    }
    void start()
  }

  const shownError = formError ?? startError

  return (
    <section
      aria-label="Ad-hoc preview"
      data-testid="adhoc-preview-form"
      className="rounded-xl border border-slate-800 bg-slate-900/60"
    >
      <header className="flex items-center gap-2 border-b border-slate-800 px-4 py-3">
        <Sparkles size={16} className="text-sky-400" aria-hidden />
        <h2 className="text-sm font-semibold text-slate-200">Ad-hoc preview</h2>
      </header>

      <form
        className="flex flex-col gap-3 px-4 py-3"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1">
            <span className="text-xs font-medium text-slate-400">Preview id</span>
            <input
              type="text"
              value={beadId}
              onChange={(e) => {
                setBeadId(e.target.value)
                setFormError(null)
                setStartError(null)
              }}
              disabled={isBusy}
              placeholder="kiln-smoke-1"
              aria-label="Preview id"
              data-testid="adhoc-preview-id"
              className="w-52 rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 font-mono text-sm text-slate-200 placeholder:text-slate-600 focus:border-sky-400 focus:outline-none focus-visible:ring-1 focus-visible:ring-sky-400 disabled:cursor-not-allowed disabled:opacity-50"
            />
          </label>

          <label className="flex flex-col gap-1">
            <span className="text-xs font-medium text-slate-400">Anvil</span>
            <select
              value={anvil}
              onChange={(e) => {
                setAnvil(e.target.value)
                setFormError(null)
              }}
              disabled={isBusy || anvils.length === 0}
              aria-label="Anvil"
              data-testid="adhoc-preview-anvil"
              className="rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 text-sm text-slate-200 focus:border-sky-400 focus:outline-none focus-visible:ring-1 focus-visible:ring-sky-400 disabled:cursor-not-allowed disabled:opacity-50"
            >
              {!onlyAnvil && <option value="">Choose an anvil…</option>}
              {anvils.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
          </label>

          <label className="flex flex-col gap-1">
            <span className="text-xs font-medium text-slate-400">Branch (optional)</span>
            <input
              type="text"
              value={branch}
              onChange={(e) => setBranch(e.target.value)}
              disabled={isBusy}
              placeholder={trimmedId ? `forge/${trimmedId}` : 'forge/<preview-id>'}
              aria-label="Branch"
              data-testid="adhoc-preview-branch"
              className="w-56 rounded-md border border-slate-700 bg-slate-800/60 px-2 py-1 font-mono text-sm text-slate-200 placeholder:text-slate-600 focus:border-sky-400 focus:outline-none focus-visible:ring-1 focus-visible:ring-sky-400 disabled:cursor-not-allowed disabled:opacity-50"
            />
          </label>

          <button
            type="submit"
            disabled={!canSubmit}
            data-testid="adhoc-preview-submit"
            title="Start a preview environment for this branch"
            className="inline-flex items-center gap-1 rounded-md border border-sky-600/30 bg-sky-600/15 px-3 py-1.5 text-xs font-medium text-sky-200 transition-colors hover:bg-sky-600/25 focus:outline-none focus-visible:ring-1 focus-visible:ring-sky-300 disabled:cursor-not-allowed disabled:opacity-50"
          >
            <Play size={13} aria-hidden />
            {isBusy ? 'Starting…' : 'Start preview'}
          </button>
        </div>

        <p className="text-xs text-slate-500">
          The id is a registry key, not a bd lookup — it names the preview, keys its logs and
          derives its hostname label, but it does not have to be an existing bead. Ad-hoc previews
          conventionally use ids like <span className="font-mono">kiln-smoke-1</span>. Leave the
          branch blank to preview <span className="font-mono">forge/&lt;preview-id&gt;</span>.
        </p>

        {anvils.length === 0 && (
          <p data-testid="adhoc-preview-no-anvils" className="text-xs text-amber-300">
            No anvil can host a preview yet. Give one a{' '}
            <span className="font-mono">.forge/preview.yaml</span> manifest in its main checkout.
          </p>
        )}

        {taken && (
          <p data-testid="adhoc-preview-taken" className="text-xs text-amber-300">
            A preview is already registered under <span className="font-mono">{trimmedId}</span>.
            Stop it below, or use another id.
          </p>
        )}

        {isBusy && (
          <p data-testid="adhoc-preview-pending" className="text-xs text-slate-400">
            Starting <span className="font-mono">{trimmedId}</span>… the checkout, setup command and
            health checks can take a few minutes.
          </p>
        )}

        {shownError && (
          <p data-testid="adhoc-preview-error" role="alert" className="text-xs text-rose-300">
            {shownError}
          </p>
        )}
      </form>
    </section>
  )
}
