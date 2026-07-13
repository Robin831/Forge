// Smith's Claude CLI emits "stream-json" — one JSON object per line. This
// module coerces those raw lines into a *transcript model* that the log
// viewer renders like a Claude Code CLI session: one-line tool headlines with
// their results nested underneath, markdown assistant prose, collapsed
// thinking, and a summary footer — with system/hook/rate-limit noise
// classified as hidden so it stays out of the way by default.
//
// The parser is intentionally permissive: the Claude wire format evolves and
// we would rather render an unrecognised line verbatim (kind 'raw') than
// throw and lose the log.

export type TranscriptKind =
  | 'tool'
  | 'assistant'
  | 'thinking'
  | 'meta'
  | 'summary'
  | 'hidden'
  | 'raw'

export interface TodoItem {
  content: string
  status: string
}

// A tool call, with its paired result nested in (results arrive on later
// "user" lines and are matched back by tool_use_id).
export interface ToolEntry {
  kind: 'tool'
  id?: string
  name: string
  // One-line CLI-style summary, e.g. the description for Bash or the
  // relativized file path for Read/Write/Edit.
  headline: string
  // Raw tool input, kept so the full JSON can be revealed behind an expander.
  input: unknown
  // Paired tool_result content, when a matching result was found.
  result?: string
  isError?: boolean
  // Populated for TodoWrite so the UI can render a checklist instead of JSON.
  todos?: TodoItem[]
}

export interface AssistantEntry {
  kind: 'assistant'
  text: string
}

export interface ThinkingEntry {
  kind: 'thinking'
  text: string
}

// A single meta line (e.g. the system/init model + session id).
export interface MetaEntry {
  kind: 'meta'
  text: string
}

// The final type=result event, rendered as a summary footer.
export interface SummaryEntry {
  kind: 'summary'
  durationMs?: number
  numTurns?: number
  totalCostUsd?: number
  inputTokens?: number
  outputTokens?: number
}

// Classified noise (thinking_tokens, hook_started/response, rate_limit_event)
// and other system chatter — not rendered unless the viewer is in verbose mode.
export interface HiddenEntry {
  kind: 'hidden'
  label: string
  content: string
}

// Anything we couldn't classify (unparseable lines, unmatched tool_results).
// Rendered by default so no information is silently dropped.
export interface RawEntry {
  kind: 'raw'
  content: string
  // Set for unmatched tool_result blocks so they still show a status badge.
  name?: string
  status?: 'success' | 'error'
}

export type TranscriptEntry =
  | ToolEntry
  | AssistantEntry
  | ThinkingEntry
  | MetaEntry
  | SummaryEntry
  | HiddenEntry
  | RawEntry

interface ClaudeContentBlock {
  type?: string
  text?: string
  thinking?: string
  id?: string
  name?: string
  input?: unknown
  is_error?: boolean
  content?: unknown
  tool_use_id?: string
}

interface ClaudeUsage {
  input_tokens?: number
  output_tokens?: number
}

interface ClaudeMessage {
  type?: string
  subtype?: string
  cwd?: string
  model?: string
  session_id?: string
  duration_ms?: number
  num_turns?: number
  total_cost_usd?: number
  usage?: ClaudeUsage
  message?: { content?: ClaudeContentBlock[] | string; usage?: ClaudeUsage }
  content?: ClaudeContentBlock[] | string
}

function truncate(s: string, max = 90): string {
  const oneLine = s.replace(/\s+/g, ' ').trim()
  if (oneLine.length <= max) return oneLine
  return oneLine.slice(0, max - 1) + '…'
}

// relativizePath strips the worktree prefix from an absolute path so tool
// headlines read like the CLI ("internal/web/…" not the full worktree path).
// It prefers the cwd reported by the system/init event; failing that it strips
// through a ".workers/<id>/" worktree marker (Forge's worktree layout).
export function relativizePath(p: string, cwd?: string): string {
  if (!p) return p
  if (cwd) {
    const norm = cwd.replace(/[/\\]+$/, '')
    if (p === norm) return p
    if (p.startsWith(norm)) {
      const rest = p.slice(norm.length).replace(/^[/\\]+/, '')
      if (rest) return rest
    }
  }
  const marker = p.match(/[/\\]\.workers[/\\][^/\\]+[/\\](.+)$/)
  if (marker) return marker[1]
  return p
}

function asRecord(input: unknown): Record<string, unknown> {
  return input && typeof input === 'object' && !Array.isArray(input)
    ? (input as Record<string, unknown>)
    : {}
}

function strField(obj: Record<string, unknown>, key: string): string | undefined {
  const v = obj[key]
  return typeof v === 'string' ? v : undefined
}

// firstStringField finds the first short string value in the input, used as a
// last-resort headline before falling back to compact JSON.
function firstStringField(obj: Record<string, unknown>): string | undefined {
  for (const k of Object.keys(obj)) {
    const v = obj[k]
    if (typeof v === 'string' && v.length > 0 && v.length <= 200) return v
  }
  return undefined
}

function compactJSON(input: unknown): string {
  try {
    return truncate(JSON.stringify(input) ?? '', 90)
  } catch {
    return truncate(String(input), 90)
  }
}

// toolHeadline mirrors the Claude Code CLI's one-line tool summaries.
export function toolHeadline(name: string, input: unknown, cwd?: string): string {
  const obj = asRecord(input)
  const fallback = () => firstStringField(obj) ?? compactJSON(input)
  switch (name) {
    case 'Bash':
      return truncate(strField(obj, 'description') || strField(obj, 'command') || fallback())
    case 'Read':
    case 'Write':
    case 'Edit':
    case 'MultiEdit':
    case 'NotebookEdit': {
      const fp = strField(obj, 'file_path') || strField(obj, 'notebook_path')
      return fp ? relativizePath(fp, cwd) : fallback()
    }
    case 'Grep':
    case 'Glob':
      return strField(obj, 'pattern') || fallback()
    case 'Task':
    case 'Agent':
      return truncate(strField(obj, 'description') || strField(obj, 'prompt') || fallback())
    case 'WebFetch':
      return strField(obj, 'url') || fallback()
    case 'WebSearch':
      return truncate(strField(obj, 'query') || fallback())
    case 'TodoWrite':
      return 'Update Todos'
    default:
      return fallback()
  }
}

function parseTodos(input: unknown): TodoItem[] | undefined {
  const obj = asRecord(input)
  const todos = obj['todos']
  if (!Array.isArray(todos)) return undefined
  const out: TodoItem[] = []
  for (const t of todos) {
    const rec = asRecord(t)
    const content = strField(rec, 'content') ?? strField(rec, 'activeForm') ?? ''
    if (!content) continue
    out.push({ content, status: strField(rec, 'status') ?? 'pending' })
  }
  return out.length ? out : undefined
}

function stringifyToolResult(content: unknown): string {
  if (content === null || content === undefined) return ''
  if (typeof content === 'string') return content
  if (Array.isArray(content)) {
    return content
      .map((c) => (typeof c === 'string' ? c : (c as { text?: string }).text ?? JSON.stringify(c)))
      .join('\n')
  }
  return JSON.stringify(content, null, 2)
}

function extractBlocks(obj: ClaudeMessage): ClaudeContentBlock[] {
  const candidates = [obj.message?.content, obj.content]
  for (const c of candidates) {
    if (Array.isArray(c)) return c
    if (typeof c === 'string') return [{ type: 'text', text: c }]
  }
  return []
}

// Intermediate blocks emitted during the first pass, before tool_result
// pairing. tool_result blocks are collected separately (indexed by
// tool_use_id) and merged into their matching tool entry in a second pass.
type ProtoBlock =
  | { proto: 'entry'; entry: TranscriptEntry }
  | { proto: 'tool'; entry: ToolEntry }
  | { proto: 'result'; toolUseId?: string; content: string; isError: boolean }

// parseLine turns a single stream-json line into 0..N proto-blocks. Exported
// for unit testing; the component uses parseTranscript.
function parseLine(raw: string, cwd?: string): ProtoBlock[] {
  const trimmed = raw.trim()
  if (!trimmed) return []
  if (!trimmed.startsWith('{') && !trimmed.startsWith('[')) {
    return [{ proto: 'entry', entry: { kind: 'raw', content: raw } }]
  }
  let obj: ClaudeMessage
  try {
    obj = JSON.parse(trimmed) as ClaudeMessage
  } catch {
    return [{ proto: 'entry', entry: { kind: 'raw', content: raw } }]
  }

  const blocks = extractBlocks(obj)
  if (blocks.length === 0) {
    return [classifyEvent(obj, raw)]
  }

  const out: ProtoBlock[] = []
  for (const block of blocks) {
    switch (block.type) {
      case 'text':
        if (block.text) out.push({ proto: 'entry', entry: { kind: 'assistant', text: block.text } })
        break
      case 'thinking':
        if (block.thinking)
          out.push({ proto: 'entry', entry: { kind: 'thinking', text: block.thinking } })
        break
      case 'tool_use': {
        const name = block.name ?? 'tool'
        out.push({
          proto: 'tool',
          entry: {
            kind: 'tool',
            id: block.id,
            name,
            headline: toolHeadline(name, block.input, cwd),
            input: block.input,
            todos: name === 'TodoWrite' ? parseTodos(block.input) : undefined,
          },
        })
        break
      }
      case 'tool_result':
        out.push({
          proto: 'result',
          toolUseId: block.tool_use_id,
          content: stringifyToolResult(block.content),
          isError: !!block.is_error,
        })
        break
      default:
        if (block.text) out.push({ proto: 'entry', entry: { kind: 'assistant', text: block.text } })
    }
  }
  if (out.length === 0) {
    return [{ proto: 'entry', entry: { kind: 'raw', content: raw } }]
  }
  return out
}

// classifyEvent maps a content-less top-level event to a transcript entry:
// system/init → meta; thinking_tokens/hook_*/rate_limit → hidden; result →
// summary; anything else with a type → hidden (labelled, revealed in verbose).
function classifyEvent(obj: ClaudeMessage, raw: string): ProtoBlock {
  const type = obj.type
  if (type === 'result') {
    const usage = obj.usage ?? obj.message?.usage
    return {
      proto: 'entry',
      entry: {
        kind: 'summary',
        durationMs: obj.duration_ms,
        numTurns: obj.num_turns,
        totalCostUsd: obj.total_cost_usd,
        inputTokens: usage?.input_tokens,
        outputTokens: usage?.output_tokens,
      },
    }
  }
  if (type === 'rate_limit_event') {
    return { proto: 'entry', entry: { kind: 'hidden', label: 'rate_limit_event', content: raw } }
  }
  if (type === 'system') {
    const subtype = obj.subtype ?? ''
    if (subtype === 'init') {
      const bits: string[] = []
      if (obj.model) bits.push(`model ${obj.model}`)
      if (obj.session_id) bits.push(`session ${obj.session_id}`)
      return {
        proto: 'entry',
        entry: { kind: 'meta', text: bits.length ? bits.join(' · ') : 'session started' },
      }
    }
    // All non-init system events (thinking_tokens, hook_started/response, and
    // any future subtypes) are noise; keep the label so verbose can surface them.
    const label = subtype ? `system/${subtype}` : 'system'
    return { proto: 'entry', entry: { kind: 'hidden', label, content: raw } }
  }
  if (type) {
    return { proto: 'entry', entry: { kind: 'hidden', label: type, content: raw } }
  }
  return { proto: 'entry', entry: { kind: 'raw', content: raw } }
}

// parseTranscript parses all raw log lines into an ordered transcript, pairing
// each tool_result back to its tool_use by id. Unmatched results fall back to
// standalone 'raw' entries so nothing is lost.
export function parseTranscript(rawLines: string[]): TranscriptEntry[] {
  // Discover the worktree cwd from system/init (if present) before computing
  // tool headlines, so file paths relativize correctly regardless of ordering.
  let cwd: string | undefined
  for (const line of rawLines) {
    const t = line.trim()
    if (!t.startsWith('{')) continue
    if (!t.includes('"cwd"')) continue
    try {
      const obj = JSON.parse(t) as ClaudeMessage
      if (obj.type === 'system' && obj.subtype === 'init' && typeof obj.cwd === 'string') {
        cwd = obj.cwd
        break
      }
    } catch {
      // ignore — permissive parse
    }
  }

  const protos: ProtoBlock[] = []
  for (const line of rawLines) {
    for (const p of parseLine(line, cwd)) protos.push(p)
  }

  // Index results by tool_use_id and record which tool_use ids exist, so
  // pairing works regardless of whether the result arrives before or after its
  // call in the stream (last-wins on duplicate ids is fine — ids are unique).
  const resultsById = new Map<string, { content: string; isError: boolean }>()
  const toolIds = new Set<string>()
  for (const p of protos) {
    if (p.proto === 'result' && p.toolUseId) {
      resultsById.set(p.toolUseId, { content: p.content, isError: p.isError })
    } else if (p.proto === 'tool' && p.entry.id) {
      toolIds.add(p.entry.id)
    }
  }

  const entries: TranscriptEntry[] = []
  for (const p of protos) {
    if (p.proto === 'entry') {
      entries.push(p.entry)
      continue
    }
    if (p.proto === 'tool') {
      const tool = p.entry
      if (tool.id) {
        const res = resultsById.get(tool.id)
        if (res) {
          tool.result = res.content
          tool.isError = res.isError
        }
      }
      entries.push(tool)
      continue
    }
    // proto === 'result': nested under its call when one exists; otherwise
    // render standalone so nothing is lost.
    if (p.toolUseId && toolIds.has(p.toolUseId)) continue
    entries.push({
      kind: 'raw',
      name: 'tool result',
      content: p.content,
      status: p.isError ? 'error' : 'success',
    })
  }
  return entries
}
