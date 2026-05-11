import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Archive,
  ArchiveRestore,
  ArrowLeft,
  CheckCircle2,
  ClipboardList,
  Hammer,
  Lightbulb,
  Loader2,
  MessageSquarePlus,
  Pencil,
  Plus,
  Rocket,
  Send,
  Sparkles,
  Trash2,
} from 'lucide-react'
import { useApiPoll } from '../hooks/useApiPoll'
import { useToast } from '../hooks/useToast'
import {
  ApiError,
  forgeSessions,
  type ForgeBeadsCreatedPayload,
  type ForgeMessage,
  type ForgeQuestionPayload,
  type ForgeSession,
  type ForgeTurnRequest,
  type StatusResponse,
} from '../api'
import AppHeader from '../components/AppHeader'
import { relativeTime } from '../lib/format'

const STATUS_POLL_INTERVAL_MS = 10_000

// ForgePage is the Hearth 2.0 "Beads-Forge" page: an iterative, chat-style
// surface for designing beads through conversation. The page steps through
// three AI stages — drafting (open chat), grilling (structured Q&A), ready
// (settled plan). Each stage has its own UI affordances; the actual bead
// emission lives in a follow-on bead.
export default function ForgePage() {
  const status = useApiPoll<StatusResponse>('/api/status', STATUS_POLL_INTERVAL_MS)
  const toast = useToast()

  const [sessions, setSessions] = useState<ForgeSession[]>([])
  const [activeId, setActiveId] = useState<number | null>(null)
  const [messages, setMessages] = useState<ForgeMessage[]>([])
  const [loadingSessions, setLoadingSessions] = useState(true)
  const [loadingMessages, setLoadingMessages] = useState(false)
  const [draft, setDraft] = useState('')
  const [composer, setComposer] = useState('')
  const [busy, setBusy] = useState(false)
  const [renamingId, setRenamingId] = useState<number | null>(null)
  const [renameValue, setRenameValue] = useState('')

  const messagesEndRef = useRef<HTMLDivElement>(null)
  const composerRef = useRef<HTMLTextAreaElement>(null)
  const renameInputRef = useRef<HTMLInputElement>(null)

  const handleApiError = useCallback(
    (err: unknown, fallback: string) => {
      const msg = err instanceof ApiError ? err.message : fallback
      toast.push(msg, 'error')
    },
    [toast],
  )

  const refreshSessions = useCallback(async () => {
    setLoadingSessions(true)
    try {
      const data = await forgeSessions.list()
      setSessions(data.sessions ?? [])
    } catch (err) {
      handleApiError(err, 'Failed to load sessions')
    } finally {
      setLoadingSessions(false)
    }
  }, [handleApiError])

  useEffect(() => {
    void refreshSessions()
  }, [refreshSessions])

  useEffect(() => {
    if (activeId === null) {
      setMessages([])
      return
    }
    let cancelled = false
    setLoadingMessages(true)
    ;(async () => {
      try {
        const data = await forgeSessions.get(activeId)
        if (cancelled) return
        setMessages(data.messages ?? [])
      } catch (err) {
        if (cancelled) return
        handleApiError(err, 'Failed to load messages')
      } finally {
        if (!cancelled) setLoadingMessages(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [activeId, handleApiError])

  useEffect(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages])

  useEffect(() => {
    if (activeId === null) {
      composerRef.current?.focus()
    }
  }, [activeId])

  useEffect(() => {
    if (renamingId !== null) {
      renameInputRef.current?.focus()
      renameInputRef.current?.select()
    }
  }, [renamingId])

  const activeSession = useMemo(
    () => sessions.find((s) => s.id === activeId) ?? null,
    [sessions, activeId],
  )

  // applyTurnResponse merges a /turn response into local state. It replaces
  // any optimistic placeholder messages with the canonical server records,
  // updates the session row in the sidebar, and bumps the active session
  // pointer if the stage changed.
  const applyTurnResponse = useCallback(
    (data: { session: ForgeSession; messages: ForgeMessage[] }, optimisticId?: number) => {
      setMessages((prev) => {
        let base = prev
        if (optimisticId !== undefined) {
          base = base.filter((m) => m.id !== optimisticId)
        }
        // Avoid duplicates if the server echoes a message we already have.
        const existing = new Set(base.map((m) => m.id))
        const additions = (data.messages ?? []).filter((m) => !existing.has(m.id))
        return [...base, ...additions]
      })
      setSessions((prev) => {
        const idx = prev.findIndex((s) => s.id === data.session.id)
        if (idx === -1) return [data.session, ...prev]
        const next = prev.slice()
        next[idx] = data.session
        return next
      })
    },
    [],
  )

  const startNewSession = useCallback(async () => {
    const text = draft.trim()
    setBusy(true)
    try {
      const data = await forgeSessions.create({
        initial_message: text || undefined,
      })
      setSessions((prev) => [data.session, ...prev])
      setActiveId(data.session.id)
      setDraft('')
      const initial = data.message ? [data.message] : []
      setMessages(initial)

      // If the user supplied an initial message, kick off the first AI turn
      // immediately so the conversation actually advances. Otherwise leave
      // the page idle so the user can compose.
      if (text) {
        try {
          const turn = await forgeSessions.turn(data.session.id, {})
          applyTurnResponse(turn)
        } catch (err) {
          handleApiError(err, 'Failed to run first AI turn')
        }
      }
      void refreshSessions()
    } catch (err) {
      handleApiError(err, 'Failed to start session')
    } finally {
      setBusy(false)
    }
  }, [applyTurnResponse, draft, handleApiError, refreshSessions])

  const sendMessage = useCallback(async () => {
    if (activeId === null) return
    const text = composer.trim()
    if (!text) return
    const optimistic: ForgeMessage = {
      id: -Date.now(),
      session_id: activeId,
      role: 'user',
      content: text,
      created_at: new Date().toISOString(),
      kind: 'text',
    }
    setComposer('')
    setMessages((prev) => [...prev, optimistic])
    setBusy(true)
    try {
      const data = await forgeSessions.turn(activeId, { content: text })
      applyTurnResponse(data, optimistic.id)
    } catch (err) {
      // Roll back the optimistic insert and restore the draft.
      setMessages((prev) => prev.filter((m) => m.id !== optimistic.id))
      setComposer(text)
      handleApiError(err, 'Failed to send message')
    } finally {
      setBusy(false)
      composerRef.current?.focus()
    }
  }, [activeId, applyTurnResponse, composer, handleApiError])

  const runTurn = useCallback(
    async (req: ForgeTurnRequest) => {
      if (activeId === null) return
      setBusy(true)
      try {
        const data = await forgeSessions.turn(activeId, req)
        applyTurnResponse(data)
      } catch (err) {
        handleApiError(err, 'Failed to run AI turn')
      } finally {
        setBusy(false)
      }
    },
    [activeId, applyTurnResponse, handleApiError],
  )

  const requestPlan = useCallback(() => {
    void runTurn({ request_plan: true })
  }, [runTurn])

  const startGrilling = useCallback(() => {
    void runTurn({ start_grilling: true })
  }, [runTurn])

  const markReady = useCallback(() => {
    void runTurn({ mark_ready: true })
  }, [runTurn])

  // createBeads is the final step of the Beads-Forge flow. It POSTs to a
  // dedicated endpoint (not /turn) that runs claude in emit mode, validates
  // the JSON envelope, and shells out to bd. We surface the resulting bead
  // IDs both as a structured chat bubble and as a toast for visibility.
  const createBeads = useCallback(async () => {
    if (activeId === null) return
    setBusy(true)
    try {
      const data = await forgeSessions.createBeads(activeId)
      // Append the new messages and refresh the session row in the sidebar.
      setMessages((prev) => {
        const existing = new Set(prev.map((m) => m.id))
        const additions = (data.messages ?? []).filter((m) => !existing.has(m.id))
        return [...prev, ...additions]
      })
      setSessions((prev) => {
        const idx = prev.findIndex((s) => s.id === data.session.id)
        if (idx === -1) return [data.session, ...prev]
        const next = prev.slice()
        next[idx] = data.session
        return next
      })
      const count = data.beads?.length ?? 0
      toast.push(
        count === 1
          ? `Created bead ${data.beads[0].bead_id}`
          : `Created ${count} beads from this session`,
        'success',
      )
    } catch (err) {
      handleApiError(err, 'Failed to create beads')
    } finally {
      setBusy(false)
    }
  }, [activeId, handleApiError, toast])

  const submitAnswer = useCallback(
    (questionId: number, optionId: string | null, freeText: string) => {
      if (activeId === null) return
      const trimmed = freeText.trim()
      if (!optionId && !trimmed) return
      void runTurn({
        answer_question_id: questionId,
        answer_option_id: optionId ?? '',
        content: trimmed || undefined,
      })
    },
    [activeId, runTurn],
  )

  const renameSession = useCallback(
    async (id: number, title: string) => {
      const trimmed = title.trim()
      if (!trimmed) {
        setRenamingId(null)
        return
      }
      try {
        const data = await forgeSessions.rename(id, trimmed)
        setSessions((prev) => prev.map((s) => (s.id === id ? data.session : s)))
        setRenamingId(null)
      } catch (err) {
        handleApiError(err, 'Failed to rename session')
      }
    },
    [handleApiError],
  )

  const archiveSession = useCallback(
    async (id: number, archive: boolean) => {
      try {
        const data = await forgeSessions.setStatus(id, archive ? 'archived' : 'draft')
        setSessions((prev) => prev.map((s) => (s.id === id ? data.session : s)))
        toast.push(archive ? 'Session archived' : 'Session restored', 'success')
      } catch (err) {
        handleApiError(err, 'Failed to update session')
      }
    },
    [handleApiError, toast],
  )

  const deleteSession = useCallback(
    async (id: number) => {
      try {
        await forgeSessions.delete(id)
        setSessions((prev) => prev.filter((s) => s.id !== id))
        if (activeId === id) {
          setActiveId(null)
          setMessages([])
        }
        toast.push('Session deleted', 'success')
      } catch (err) {
        handleApiError(err, 'Failed to delete session')
      }
    },
    [activeId, handleApiError, toast],
  )

  // Find the latest open question (one without a subsequent answer in the
  // chat history). The grilling-stage UI only allows answering this one.
  const latestOpenQuestion = useMemo(() => {
    if (!activeSession || activeSession.stage !== 'grilling') return null
    let last: ForgeMessage | null = null
    const answeredIds = new Set<number>()
    for (const m of messages) {
      if (m.kind === 'answer' && m.metadata) {
        try {
          const meta = JSON.parse(m.metadata) as { question_id?: number }
          if (typeof meta.question_id === 'number') {
            answeredIds.add(meta.question_id)
          }
        } catch {
          // ignore malformed metadata
        }
      }
      if (m.kind === 'question') {
        last = m
      }
    }
    if (last && !answeredIds.has(last.id)) return last
    return null
  }, [activeSession, messages])

  return (
    <div className="flex min-h-full flex-col gap-4 p-4 sm:p-6">
      <AppHeader daemonOnline={status.data?.running} daemonLoading={status.loading} />

      <div className="grid min-h-[36rem] grid-cols-1 gap-4 md:grid-cols-[18rem_minmax(0,1fr)]">
        <SessionSidebar
          sessions={sessions}
          loading={loadingSessions}
          activeId={activeId}
          renamingId={renamingId}
          renameValue={renameValue}
          renameInputRef={renameInputRef}
          onSelect={setActiveId}
          onNew={() => {
            setActiveId(null)
            setDraft('')
          }}
          onStartRename={(s) => {
            setRenamingId(s.id)
            setRenameValue(s.title || '')
          }}
          onCancelRename={() => setRenamingId(null)}
          onSubmitRename={renameSession}
          onRenameValueChange={setRenameValue}
          onArchive={archiveSession}
          onDelete={deleteSession}
        />

        <main className="flex min-h-[36rem] flex-col rounded-xl border border-slate-800 bg-slate-900/60">
          {activeSession ? (
            <>
              <SessionHeader
                session={activeSession}
                onBack={() => setActiveId(null)}
                busy={busy}
                onRequestPlan={requestPlan}
                onStartGrilling={startGrilling}
                onMarkReady={markReady}
                onCreateBeads={createBeads}
              />

              <ConversationView
                messages={messages}
                loading={loadingMessages}
                endRef={messagesEndRef}
                openQuestionId={latestOpenQuestion?.id ?? null}
                stage={activeSession.stage}
                busy={busy}
                onAnswer={submitAnswer}
              />

              <Composer
                value={composer}
                onChange={setComposer}
                onSend={sendMessage}
                disabled={busy || activeSession.stage === 'ready'}
                placeholder={composerPlaceholder(activeSession.stage, busy)}
                inputRef={composerRef}
              />
            </>
          ) : (
            <NewSessionView
              draft={draft}
              onDraftChange={setDraft}
              onStart={startNewSession}
              busy={busy}
              composerRef={composerRef}
            />
          )}
        </main>
      </div>

      <footer className="text-center text-xs text-slate-500">
        Beads-Forge · drafting → grilling → ready → create beads.
      </footer>
    </div>
  )
}

function composerPlaceholder(stage: ForgeSession['stage'], busy: boolean): string {
  if (busy) return 'Claude is working…'
  switch (stage) {
    case 'drafting':
      return 'Reply, push back, or ask claude to clarify…'
    case 'grilling':
      return 'Type a free-form answer (or pick an option above)…'
    case 'ready':
      return 'Click "Create bead(s)" to emit the plan as one or more beads.'
    default:
      return 'Type a message…'
  }
}

interface SessionHeaderProps {
  session: ForgeSession
  busy: boolean
  onBack: () => void
  onRequestPlan: () => void
  onStartGrilling: () => void
  onMarkReady: () => void
  onCreateBeads: () => void
}

function SessionHeader({
  session,
  busy,
  onBack,
  onRequestPlan,
  onStartGrilling,
  onMarkReady,
  onCreateBeads,
}: SessionHeaderProps) {
  const stage = session.stage ?? 'drafting'
  const hasPlan = (session.plan ?? '').trim().length > 0
  return (
    <header className="flex flex-col gap-2 border-b border-slate-800 px-4 py-3">
      <div className="flex items-center gap-3">
        <button
          type="button"
          onClick={onBack}
          className="rounded-md p-1 text-slate-400 hover:bg-slate-800/60 hover:text-slate-200 md:hidden"
          aria-label="Back to sessions"
        >
          <ArrowLeft size={16} />
        </button>
        <Hammer size={16} className="text-amber-400" aria-hidden />
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-sm font-semibold text-slate-100">
            {session.title || 'Untitled session'}
          </h2>
          <p className="truncate text-xs text-slate-500">
            updated {relativeTime(session.updated_at)}
            {session.status === 'archived' ? ' · archived' : ''}
          </p>
        </div>
        <StageBadge stage={stage} />
      </div>
      <div className="flex flex-wrap items-center gap-2">
        {stage === 'drafting' && (
          <StageButton
            onClick={onRequestPlan}
            disabled={busy}
            tone="amber"
            icon={<ClipboardList size={12} aria-hidden />}
          >
            {hasPlan ? 'Refresh plan' : 'Request plan'}
          </StageButton>
        )}
        {stage === 'drafting' && hasPlan && (
          <StageButton
            onClick={onStartGrilling}
            disabled={busy}
            tone="violet"
            icon={<Sparkles size={12} aria-hidden />}
          >
            Start grilling
          </StageButton>
        )}
        {stage === 'grilling' && (
          <StageButton
            onClick={onMarkReady}
            disabled={busy}
            tone="emerald"
            icon={<CheckCircle2 size={12} aria-hidden />}
          >
            Mark ready
          </StageButton>
        )}
        {stage === 'ready' && (
          <StageButton
            onClick={onCreateBeads}
            disabled={busy || !hasPlan}
            tone="emerald"
            icon={<Rocket size={12} aria-hidden />}
          >
            Create bead(s)
          </StageButton>
        )}
        {busy && (
          <span className="inline-flex items-center gap-1 text-xs text-slate-400">
            <Loader2 size={12} className="animate-spin" aria-hidden />
            claude is thinking…
          </span>
        )}
      </div>
      {hasPlan && <PlanPanel plan={session.plan ?? ''} />}
    </header>
  )
}

function PlanPanel({ plan }: { plan: string }) {
  return (
    <details className="rounded-md border border-amber-500/30 bg-amber-500/5">
      <summary className="cursor-pointer px-3 py-2 text-xs font-medium text-amber-200">
        <Lightbulb size={12} className="mr-1 inline" aria-hidden />
        Current plan ({plan.length} chars) — click to expand
      </summary>
      <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words border-t border-amber-500/20 px-3 py-2 text-xs text-amber-50">
        {plan}
      </pre>
    </details>
  )
}

function StageBadge({ stage }: { stage: ForgeSession['stage'] }) {
  const config = {
    drafting: { label: 'Drafting', tone: 'border-sky-500/40 bg-sky-500/10 text-sky-200' },
    grilling: { label: 'Grilling', tone: 'border-violet-500/40 bg-violet-500/10 text-violet-200' },
    ready: { label: 'Ready', tone: 'border-emerald-500/40 bg-emerald-500/10 text-emerald-200' },
  }[stage] ?? { label: stage, tone: 'border-slate-500/40 bg-slate-500/10 text-slate-200' }
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-full border px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${config.tone}`}
    >
      {config.label}
    </span>
  )
}

interface StageButtonProps {
  onClick: () => void
  disabled: boolean
  tone: 'amber' | 'violet' | 'emerald'
  icon: React.ReactNode
  children: React.ReactNode
}

function StageButton({ onClick, disabled, tone, icon, children }: StageButtonProps) {
  const tones = {
    amber: 'border-amber-500/40 bg-amber-500/15 text-amber-200 hover:bg-amber-500/25',
    violet: 'border-violet-500/40 bg-violet-500/15 text-violet-200 hover:bg-violet-500/25',
    emerald: 'border-emerald-500/40 bg-emerald-500/15 text-emerald-200 hover:bg-emerald-500/25',
  }[tone]
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className={`inline-flex items-center gap-1 rounded-md border px-2 py-1 text-xs font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${tones}`}
    >
      {icon}
      {children}
    </button>
  )
}

interface SessionSidebarProps {
  sessions: ForgeSession[]
  loading: boolean
  activeId: number | null
  renamingId: number | null
  renameValue: string
  renameInputRef: React.RefObject<HTMLInputElement | null>
  onSelect: (id: number) => void
  onNew: () => void
  onStartRename: (s: ForgeSession) => void
  onCancelRename: () => void
  onSubmitRename: (id: number, title: string) => Promise<void>
  onRenameValueChange: (v: string) => void
  onArchive: (id: number, archive: boolean) => Promise<void>
  onDelete: (id: number) => Promise<void>
}

function SessionSidebar({
  sessions,
  loading,
  activeId,
  renamingId,
  renameValue,
  renameInputRef,
  onSelect,
  onNew,
  onStartRename,
  onCancelRename,
  onSubmitRename,
  onRenameValueChange,
  onArchive,
  onDelete,
}: SessionSidebarProps) {
  return (
    <aside className="flex max-h-[32rem] min-h-[20rem] flex-col rounded-xl border border-slate-800 bg-slate-900/60 md:max-h-none">
      <header className="flex items-center justify-between gap-2 border-b border-slate-800 px-4 py-3">
        <h2 className="text-sm font-semibold text-slate-200">Design sessions</h2>
        <button
          type="button"
          onClick={onNew}
          className="inline-flex items-center gap-1 rounded-md border border-amber-600/40 bg-amber-600/15 px-2 py-1 text-xs font-medium text-amber-200 transition-colors hover:bg-amber-600/25"
          aria-label="Start a new session"
        >
          <Plus size={12} aria-hidden />
          New
        </button>
      </header>

      <div className="flex-1 overflow-y-auto">
        {loading ? (
          <p className="px-4 py-8 text-center text-sm text-slate-500">Loading…</p>
        ) : sessions.length === 0 ? (
          <p className="px-4 py-8 text-center text-sm text-slate-500">
            No sessions yet — start one on the right.
          </p>
        ) : (
          <ul className="divide-y divide-slate-800/60" data-testid="forge-sessions-list">
            {sessions.map((s) => (
              <li
                key={s.id}
                className={`group flex items-center gap-2 px-3 py-2 transition-colors ${
                  s.id === activeId ? 'bg-slate-800/60' : 'hover:bg-slate-800/40'
                }`}
              >
                {renamingId === s.id ? (
                  <form
                    className="flex flex-1 items-center gap-1"
                    onSubmit={(e) => {
                      e.preventDefault()
                      void onSubmitRename(s.id, renameValue)
                    }}
                  >
                    <input
                      ref={renameInputRef}
                      value={renameValue}
                      onChange={(e) => onRenameValueChange(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === 'Escape') onCancelRename()
                      }}
                      className="flex-1 rounded border border-slate-600 bg-slate-800 px-2 py-1 text-xs text-slate-100 focus:border-amber-400 focus:outline-none"
                      aria-label="Rename session"
                    />
                  </form>
                ) : (
                  <button
                    type="button"
                    onClick={() => onSelect(s.id)}
                    className="flex min-w-0 flex-1 flex-col items-start text-left"
                  >
                    <span
                      className={`truncate text-sm ${
                        s.id === activeId ? 'text-slate-100' : 'text-slate-200'
                      }`}
                    >
                      {s.title || 'Untitled session'}
                    </span>
                    <span className="truncate text-[11px] text-slate-500">
                      {s.message_count} msg · {s.stage ?? 'drafting'} · {relativeTime(s.updated_at)}
                      {s.status === 'archived' ? ' · archived' : ''}
                    </span>
                  </button>
                )}

                <SidebarRowActions
                  session={s}
                  isRenaming={renamingId === s.id}
                  onStartRename={() => onStartRename(s)}
                  onArchive={() => void onArchive(s.id, s.status !== 'archived')}
                  onDelete={() => {
                    if (
                      window.confirm(
                        `Delete "${s.title || 'Untitled session'}"? This cannot be undone.`,
                      )
                    ) {
                      void onDelete(s.id)
                    }
                  }}
                />
              </li>
            ))}
          </ul>
        )}
      </div>
    </aside>
  )
}

interface SidebarRowActionsProps {
  session: ForgeSession
  isRenaming: boolean
  onStartRename: () => void
  onArchive: () => void
  onDelete: () => void
}

function SidebarRowActions({
  session,
  isRenaming,
  onStartRename,
  onArchive,
  onDelete,
}: SidebarRowActionsProps) {
  if (isRenaming) return null
  const archived = session.status === 'archived'
  return (
    <div className="flex items-center gap-0.5 opacity-0 transition-opacity group-hover:opacity-100 focus-within:opacity-100">
      <IconButton onClick={onStartRename} label="Rename">
        <Pencil size={12} />
      </IconButton>
      <IconButton
        onClick={onArchive}
        label={archived ? 'Restore' : 'Archive'}
      >
        {archived ? <ArchiveRestore size={12} /> : <Archive size={12} />}
      </IconButton>
      <IconButton onClick={onDelete} label="Delete" tone="danger">
        <Trash2 size={12} />
      </IconButton>
    </div>
  )
}

interface IconButtonProps {
  onClick: () => void
  label: string
  tone?: 'default' | 'danger'
  children: React.ReactNode
}

function IconButton({ onClick, label, tone = 'default', children }: IconButtonProps) {
  const toneClass =
    tone === 'danger'
      ? 'text-slate-500 hover:text-red-300 hover:bg-red-500/10'
      : 'text-slate-500 hover:text-slate-200 hover:bg-slate-700/60'
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded p-1 ${toneClass}`}
      aria-label={label}
      title={label}
    >
      {children}
    </button>
  )
}

interface ConversationViewProps {
  messages: ForgeMessage[]
  loading: boolean
  endRef: React.RefObject<HTMLDivElement | null>
  openQuestionId: number | null
  stage: ForgeSession['stage']
  busy: boolean
  onAnswer: (questionId: number, optionId: string | null, freeText: string) => void
}

function ConversationView({
  messages,
  loading,
  endRef,
  openQuestionId,
  stage,
  busy,
  onAnswer,
}: ConversationViewProps) {
  return (
    <div className="flex-1 overflow-y-auto px-4 py-4" data-testid="forge-conversation">
      {loading && messages.length === 0 ? (
        <p className="text-center text-sm text-slate-500">Loading messages…</p>
      ) : messages.length === 0 ? (
        <EmptyConversation stage={stage} />
      ) : (
        <ul className="space-y-3">
          {messages.map((m) => (
            <MessageBubble
              key={m.id}
              message={m}
              isOpenQuestion={openQuestionId === m.id && !busy}
              onAnswer={onAnswer}
            />
          ))}
        </ul>
      )}
      <div ref={endRef} />
    </div>
  )
}

function EmptyConversation({ stage }: { stage: ForgeSession['stage'] }) {
  const stageHint =
    stage === 'grilling'
      ? 'Grilling started — claude will surface its first question on the next turn.'
      : stage === 'ready'
        ? 'Session is settled — click "Create bead(s)" to emit beads from the plan.'
        : 'No messages yet — send the first one to get started.'
  return (
    <div className="flex h-full flex-col items-center justify-center gap-2 text-center text-sm text-slate-500">
      <MessageSquarePlus size={28} className="text-slate-600" aria-hidden />
      <p>{stageHint}</p>
    </div>
  )
}

interface MessageBubbleProps {
  message: ForgeMessage
  isOpenQuestion: boolean
  onAnswer: (questionId: number, optionId: string | null, freeText: string) => void
}

function MessageBubble({ message, isOpenQuestion, onAnswer }: MessageBubbleProps) {
  if (message.kind === 'question') {
    return <QuestionBubble message={message} isOpen={isOpenQuestion} onAnswer={onAnswer} />
  }
  if (message.kind === 'plan') {
    return <PlanBubble message={message} />
  }
  if (message.kind === 'beads_created') {
    return <BeadsCreatedBubble message={message} />
  }
  if (message.kind === 'status' || message.role === 'system') {
    return <StatusBubble message={message} />
  }
  if (message.kind === 'answer') {
    return <AnswerBubble message={message} />
  }
  const isUser = message.role === 'user'
  const align = isUser ? 'items-end' : 'items-start'
  const bubble = isUser
    ? 'bg-amber-500/15 border-amber-500/30 text-amber-100'
    : 'bg-slate-800/60 border-slate-700 text-slate-100'
  return (
    <li className={`flex flex-col ${align}`}>
      <div
        className={`max-w-[85%] rounded-lg border px-3 py-2 text-sm whitespace-pre-wrap break-words ${bubble}`}
      >
        {message.content}
      </div>
      <span className="mt-1 text-[10px] text-slate-500">
        {message.role} · {relativeTime(message.created_at)}
      </span>
    </li>
  )
}

function PlanBubble({ message }: { message: ForgeMessage }) {
  return (
    <li className="flex flex-col items-start">
      <div className="max-w-[95%] rounded-lg border border-amber-500/40 bg-amber-500/5 px-3 py-2 text-sm text-amber-100">
        <div className="mb-1 flex items-center gap-1 text-[10px] font-semibold uppercase tracking-wide text-amber-300">
          <ClipboardList size={11} aria-hidden />
          Plan
        </div>
        <pre className="whitespace-pre-wrap break-words text-sm leading-relaxed">{message.content}</pre>
      </div>
      <span className="mt-1 text-[10px] text-slate-500">
        assistant · {relativeTime(message.created_at)}
      </span>
    </li>
  )
}

function BeadsCreatedBubble({ message }: { message: ForgeMessage }) {
  let payload: ForgeBeadsCreatedPayload | null = null
  try {
    if (message.metadata) payload = JSON.parse(message.metadata) as ForgeBeadsCreatedPayload
  } catch {
    payload = null
  }
  const beads = payload?.beads ?? []
  const summary = payload?.summary?.trim() ?? ''
  return (
    <li className="flex flex-col items-start">
      <div className="w-full max-w-[95%] rounded-lg border border-emerald-500/40 bg-emerald-500/5 px-3 py-3 text-sm text-emerald-50">
        <div className="mb-2 flex items-center gap-1 text-[10px] font-semibold uppercase tracking-wide text-emerald-300">
          <Rocket size={11} aria-hidden />
          Beads created
        </div>
        {summary && (
          <p className="mb-3 whitespace-pre-wrap break-words text-sm">{summary}</p>
        )}
        {beads.length === 0 ? (
          <pre className="whitespace-pre-wrap break-words text-sm leading-relaxed">
            {message.content}
          </pre>
        ) : (
          <ul className="space-y-1.5" data-testid="forge-beads-created">
            {beads.map((b) => (
              <li
                key={b.bead_id}
                className="flex items-center gap-2 rounded border border-emerald-500/30 bg-emerald-500/10 px-3 py-2 text-sm"
              >
                <code className="rounded bg-emerald-600/30 px-1.5 py-0.5 text-[11px] font-semibold text-emerald-100">
                  {b.bead_id}
                </code>
                <span className="text-[11px] uppercase tracking-wide text-emerald-300/80">
                  {b.anvil}
                </span>
                <span className="flex-1 truncate text-emerald-50">{b.title}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
      <span className="mt-1 text-[10px] text-slate-500">
        assistant · {relativeTime(message.created_at)}
      </span>
    </li>
  )
}

function StatusBubble({ message }: { message: ForgeMessage }) {
  return (
    <li className="flex flex-col items-center">
      <div className="rounded-md border border-slate-700 bg-slate-800/40 px-3 py-1.5 text-xs italic text-slate-400">
        {message.content}
      </div>
    </li>
  )
}

function AnswerBubble({ message }: { message: ForgeMessage }) {
  return (
    <li className="flex flex-col items-end">
      <div className="max-w-[85%] rounded-lg border border-amber-500/30 bg-amber-500/15 px-3 py-2 text-sm text-amber-100">
        <span className="mr-2 inline-block rounded bg-amber-600/30 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-amber-200">
          Answer
        </span>
        {message.content}
      </div>
      <span className="mt-1 text-[10px] text-slate-500">
        you · {relativeTime(message.created_at)}
      </span>
    </li>
  )
}

interface QuestionBubbleProps {
  message: ForgeMessage
  isOpen: boolean
  onAnswer: (questionId: number, optionId: string | null, freeText: string) => void
}

function QuestionBubble({ message, isOpen, onAnswer }: QuestionBubbleProps) {
  const [freeText, setFreeText] = useState('')
  let payload: ForgeQuestionPayload | null = null
  try {
    if (message.metadata) payload = JSON.parse(message.metadata) as ForgeQuestionPayload
  } catch {
    payload = null
  }
  const options = payload?.options ?? []
  const recommendation = payload?.recommendation ?? ''

  return (
    <li className="flex flex-col items-start">
      <div className="w-full max-w-[95%] rounded-lg border border-violet-500/40 bg-violet-500/5 px-3 py-3 text-sm text-violet-50">
        <div className="mb-2 flex items-center gap-1 text-[10px] font-semibold uppercase tracking-wide text-violet-300">
          <Sparkles size={11} aria-hidden />
          Question
        </div>
        <p className="mb-3 whitespace-pre-wrap break-words text-sm">{message.content}</p>

        {options.length > 0 && (
          <ul className="mb-3 space-y-1.5">
            {options.map((opt) => {
              const isRecommended = opt.id === recommendation
              return (
                <li key={opt.id}>
                  <button
                    type="button"
                    disabled={!isOpen}
                    onClick={() => onAnswer(message.id, opt.id, '')}
                    className={`flex w-full items-start gap-2 rounded border px-3 py-2 text-left text-sm transition-colors ${
                      isOpen
                        ? 'border-violet-500/40 bg-violet-500/10 text-violet-100 hover:bg-violet-500/20'
                        : 'border-slate-700 bg-slate-800/40 text-slate-400'
                    } disabled:cursor-not-allowed disabled:opacity-70`}
                  >
                    <span className="flex-1">
                      <span className="font-medium">{opt.label}</span>
                      {isRecommended && (
                        <span className="ml-2 rounded bg-violet-500/30 px-1.5 py-0.5 text-[9px] font-semibold uppercase tracking-wide text-violet-100">
                          Recommended
                        </span>
                      )}
                      {opt.description && (
                        <span className="mt-0.5 block text-xs text-violet-200/80">
                          {opt.description}
                        </span>
                      )}
                    </span>
                  </button>
                </li>
              )
            })}
          </ul>
        )}

        {payload?.rationale && (
          <p className="mb-3 rounded border border-violet-500/20 bg-violet-500/5 px-2 py-1 text-xs italic text-violet-200/80">
            Rationale: {payload.rationale}
          </p>
        )}

        {isOpen && (
          <form
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              const trimmed = freeText.trim()
              if (!trimmed) return
              onAnswer(message.id, null, trimmed)
              setFreeText('')
            }}
          >
            <input
              value={freeText}
              onChange={(e) => setFreeText(e.target.value)}
              placeholder="…or write a free-form answer"
              className="flex-1 rounded border border-violet-500/40 bg-slate-950/40 px-2 py-1.5 text-xs text-violet-50 placeholder:text-violet-300/40 focus:border-violet-300 focus:outline-none"
              aria-label="Free-form answer"
            />
            <button
              type="submit"
              disabled={freeText.trim().length === 0}
              className="rounded border border-violet-500/40 bg-violet-500/20 px-2 py-1.5 text-xs font-medium text-violet-100 transition-colors hover:bg-violet-500/30 disabled:cursor-not-allowed disabled:opacity-50"
            >
              Answer
            </button>
          </form>
        )}
      </div>
      <span className="mt-1 text-[10px] text-slate-500">
        assistant · {relativeTime(message.created_at)}
      </span>
    </li>
  )
}

interface ComposerProps {
  value: string
  onChange: (v: string) => void
  onSend: () => void
  disabled: boolean
  placeholder?: string
  inputRef: React.RefObject<HTMLTextAreaElement | null>
}

function Composer({ value, onChange, onSend, disabled, placeholder, inputRef }: ComposerProps) {
  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      onSend()
    }
  }
  return (
    <div className="border-t border-slate-800 px-4 py-3">
      <div className="flex items-end gap-2">
        <textarea
          ref={inputRef}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          onKeyDown={handleKeyDown}
          rows={2}
          disabled={disabled}
          placeholder={placeholder ?? 'Type a message…'}
          className="flex-1 resize-none rounded-md border border-slate-700 bg-slate-950/40 px-3 py-2 text-sm text-slate-100 placeholder:text-slate-500 focus:border-amber-400 focus:outline-none disabled:opacity-60"
          aria-label="Message input"
        />
        <button
          type="button"
          onClick={onSend}
          disabled={disabled || value.trim().length === 0}
          className="inline-flex items-center gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/20 px-3 py-2 text-sm font-medium text-amber-200 transition-colors hover:bg-amber-500/30 disabled:cursor-not-allowed disabled:opacity-50"
        >
          <Send size={14} aria-hidden />
          Send
        </button>
      </div>
    </div>
  )
}

interface NewSessionViewProps {
  draft: string
  onDraftChange: (v: string) => void
  onStart: () => void
  busy: boolean
  composerRef: React.RefObject<HTMLTextAreaElement | null>
}

function NewSessionView({ draft, onDraftChange, onStart, busy, composerRef }: NewSessionViewProps) {
  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      onStart()
    }
  }
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-4 px-4 py-10 text-center">
      <Hammer size={32} className="text-amber-400" aria-hidden />
      <div>
        <h2 className="text-lg font-semibold text-slate-100">Start a design session</h2>
        <p className="mx-auto mt-1 max-w-md text-sm text-slate-400">
          Describe an idea — claude will discuss it with you, draft a plan, then grill the design
          decisions until everything is settled.
        </p>
      </div>
      <textarea
        ref={composerRef}
        value={draft}
        onChange={(e) => onDraftChange(e.target.value)}
        onKeyDown={handleKeyDown}
        rows={5}
        placeholder="What would you like to forge?"
        className="w-full max-w-xl resize-none rounded-md border border-slate-700 bg-slate-950/40 px-3 py-2 text-sm text-slate-100 placeholder:text-slate-500 focus:border-amber-400 focus:outline-none"
        aria-label="Draft input"
      />
      <button
        type="button"
        onClick={onStart}
        disabled={busy}
        className="inline-flex items-center gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/20 px-4 py-2 text-sm font-medium text-amber-200 transition-colors hover:bg-amber-500/30 disabled:cursor-not-allowed disabled:opacity-50"
      >
        <Plus size={14} aria-hidden />
        {busy ? 'Starting…' : draft.trim() ? 'Start with this prompt' : 'Start empty session'}
      </button>
    </div>
  )
}
