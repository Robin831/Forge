import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Archive,
  ArchiveRestore,
  ArrowLeft,
  Hammer,
  MessageSquarePlus,
  Pencil,
  Plus,
  Send,
  Trash2,
} from 'lucide-react'
import { useApiPoll } from '../hooks/useApiPoll'
import { useToast } from '../hooks/useToast'
import {
  ApiError,
  forgeSessions,
  type ForgeMessage,
  type ForgeSession,
  type StatusResponse,
} from '../api'
import AppHeader from '../components/AppHeader'
import { relativeTime } from '../lib/format'

const STATUS_POLL_INTERVAL_MS = 10_000

// ForgePage is the Hearth 2.0 "Beads-Forge" page: an iterative, chat-style
// surface for designing beads through conversation. The foundation bead
// delivers persistence + draft input only — later beads add the claude
// integration and the grilling/plan stages.
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

  // Load the session list once on mount and refresh after mutations.
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

  // Load messages when the active session changes.
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

  // Auto-scroll to the bottom when new messages arrive.
  useEffect(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages])

  // Focus the draft textarea when switching to new-session mode. The effect
  // runs after the render so NewSessionView is mounted and composerRef points
  // at the draft input (not the old conversation textarea).
  useEffect(() => {
    if (activeId === null) {
      composerRef.current?.focus()
    }
  }, [activeId])

  // Focus the rename input when entering rename mode.
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
      if (data.message) {
        setMessages([data.message])
      } else {
        setMessages([])
      }
      // Re-fetch the list so the message_count is fresh for sidebar items
      // we already had in memory.
      void refreshSessions()
    } catch (err) {
      handleApiError(err, 'Failed to start session')
    } finally {
      setBusy(false)
    }
  }, [draft, handleApiError, refreshSessions])

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
    }
    setComposer('')
    setMessages((prev) => [...prev, optimistic])
    setBusy(true)
    try {
      const data = await forgeSessions.appendMessage(activeId, text)
      setMessages((prev) => prev.map((m) => (m.id === optimistic.id ? data.message : m)))
      setSessions((prev) =>
        prev.map((s) => (s.id === activeId ? data.session : s)),
      )
    } catch (err) {
      // Roll back the optimistic insert and restore the draft.
      setMessages((prev) => prev.filter((m) => m.id !== optimistic.id))
      setComposer(text)
      handleApiError(err, 'Failed to send message')
    } finally {
      setBusy(false)
      composerRef.current?.focus()
    }
  }, [activeId, composer, handleApiError])

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

  return (
    <div className="mx-auto flex min-h-full max-w-7xl flex-col gap-4 p-4 sm:p-6">
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
              <header className="flex items-center gap-3 border-b border-slate-800 px-4 py-3">
                <button
                  type="button"
                  onClick={() => setActiveId(null)}
                  className="rounded-md p-1 text-slate-400 hover:bg-slate-800/60 hover:text-slate-200 md:hidden"
                  aria-label="Back to sessions"
                >
                  <ArrowLeft size={16} />
                </button>
                <Hammer size={16} className="text-amber-400" aria-hidden />
                <div className="min-w-0 flex-1">
                  <h2 className="truncate text-sm font-semibold text-slate-100">
                    {activeSession.title || 'Untitled session'}
                  </h2>
                  <p className="truncate text-xs text-slate-500">
                    {activeSession.status} · updated {relativeTime(activeSession.updated_at)}
                  </p>
                </div>
              </header>

              <ConversationView
                messages={messages}
                loading={loadingMessages}
                endRef={messagesEndRef}
              />

              <Composer
                value={composer}
                onChange={setComposer}
                onSend={sendMessage}
                disabled={busy}
                placeholder="Reply… (no AI yet — your message is just persisted for now)"
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
        Beads-Forge · foundation bead — persistence only, AI integration arrives in the next bead.
      </footer>
    </div>
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
                      {s.message_count} msg · {relativeTime(s.updated_at)}
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
}

function ConversationView({ messages, loading, endRef }: ConversationViewProps) {
  return (
    <div className="flex-1 overflow-y-auto px-4 py-4" data-testid="forge-conversation">
      {loading && messages.length === 0 ? (
        <p className="text-center text-sm text-slate-500">Loading messages…</p>
      ) : messages.length === 0 ? (
        <EmptyConversation />
      ) : (
        <ul className="space-y-3">
          {messages.map((m) => (
            <MessageBubble key={m.id} message={m} />
          ))}
        </ul>
      )}
      <div ref={endRef} />
    </div>
  )
}

function EmptyConversation() {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-2 text-center text-sm text-slate-500">
      <MessageSquarePlus size={28} className="text-slate-600" aria-hidden />
      <p>No messages yet — send the first one to get started.</p>
    </div>
  )
}

function MessageBubble({ message }: { message: ForgeMessage }) {
  const isUser = message.role === 'user'
  const isSystem = message.role === 'system'
  const align = isUser ? 'items-end' : 'items-start'
  const bubble = isUser
    ? 'bg-amber-500/15 border-amber-500/30 text-amber-100'
    : isSystem
      ? 'bg-slate-800/40 border-slate-700 text-slate-400 italic'
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
          Describe a bead idea in your own words. Foundation bead: your input is just persisted
          today — claude grilling and bead emission arrive in the next two beads.
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
