// Smith's Claude CLI emits "stream-json" — one JSON object per line. This
// helper coerces a raw line into a structured LogEntry the UI can render
// nicely. When parsing fails (or the line isn't recognised), we fall back to
// a plain text entry so nothing is dropped.
//
// We intentionally keep the parser permissive: the Claude wire format evolves
// and we'd rather render an unrecognised event verbatim than throw.

export type LogEntryKind = 'tool_use' | 'tool_result' | 'text' | 'thinking' | 'system' | 'raw'

export interface LogEntry {
  kind: LogEntryKind
  // Display name for tool_use blocks (e.g. "Bash", "Read") and a level for
  // text/system entries. Optional for plain text.
  name?: string
  // Primary content rendered as a monospace block.
  content: string
  // Optional status badge for tool_result blocks.
  status?: 'success' | 'error'
}

interface ClaudeContentBlock {
  type?: string
  text?: string
  thinking?: string
  name?: string
  input?: unknown
  is_error?: boolean
  content?: unknown
}

interface ClaudeMessage {
  type?: string
  message?: { content?: ClaudeContentBlock[] | string }
  content?: ClaudeContentBlock[] | string
}

function stringifyToolInput(input: unknown): string {
  if (input === null || input === undefined) return ''
  if (typeof input === 'string') return input
  try {
    return JSON.stringify(input, null, 2)
  } catch {
    return String(input)
  }
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

// parseLogLine turns one stream-json line into 0..N LogEntry objects (a single
// assistant message can contain multiple content blocks).
export function parseLogLine(raw: string): LogEntry[] {
  const trimmed = raw.trim()
  if (!trimmed) return []
  if (!trimmed.startsWith('{') && !trimmed.startsWith('[')) {
    return [{ kind: 'raw', content: raw }]
  }
  let obj: ClaudeMessage
  try {
    obj = JSON.parse(trimmed) as ClaudeMessage
  } catch {
    return [{ kind: 'raw', content: raw }]
  }

  const blocks = extractBlocks(obj)
  if (blocks.length === 0) {
    // System events / lifecycle events with no renderable content — keep as
    // a one-line summary so the view still shows something.
    if (obj.type) {
      return [{ kind: 'system', name: obj.type, content: '' }]
    }
    return [{ kind: 'raw', content: raw }]
  }

  const entries: LogEntry[] = []
  for (const block of blocks) {
    switch (block.type) {
      case 'text':
        if (block.text) entries.push({ kind: 'text', content: block.text })
        break
      case 'thinking':
        if (block.thinking) entries.push({ kind: 'thinking', content: block.thinking })
        break
      case 'tool_use':
        entries.push({
          kind: 'tool_use',
          name: block.name ?? 'tool',
          content: stringifyToolInput(block.input),
        })
        break
      case 'tool_result':
        entries.push({
          kind: 'tool_result',
          content: stringifyToolResult(block.content),
          status: block.is_error ? 'error' : 'success',
        })
        break
      default:
        if (block.text) entries.push({ kind: 'text', content: block.text })
    }
  }
  if (entries.length === 0) {
    return [{ kind: 'raw', content: raw }]
  }
  return entries
}

function extractBlocks(obj: ClaudeMessage): ClaudeContentBlock[] {
  const candidates = [obj.message?.content, obj.content]
  for (const c of candidates) {
    if (Array.isArray(c)) return c
    if (typeof c === 'string') return [{ type: 'text', text: c }]
  }
  return []
}
