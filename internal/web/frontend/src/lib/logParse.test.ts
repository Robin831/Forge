import { describe, expect, it } from 'vitest'
import {
  parseTranscript,
  relativizePath,
  toolHeadline,
  type SummaryEntry,
  type ToolEntry,
} from './logParse'

// Helpers to build stream-json lines the way Claude's CLI emits them.
function assistant(...blocks: unknown[]): string {
  return JSON.stringify({ type: 'assistant', message: { content: blocks } })
}
function user(...blocks: unknown[]): string {
  return JSON.stringify({ type: 'user', message: { content: blocks } })
}
function toolUse(id: string, name: string, input: unknown): unknown {
  return { type: 'tool_use', id, name, input }
}
function toolResult(toolUseId: string, content: unknown, isError = false): unknown {
  return { type: 'tool_result', tool_use_id: toolUseId, content, is_error: isError }
}

describe('parseTranscript pairing', () => {
  it('pairs a tool_result to its tool_use in order', () => {
    const entries = parseTranscript([
      assistant(toolUse('toolu_1', 'Bash', { command: 'ls', description: 'List files' })),
      user(toolResult('toolu_1', 'file1\nfile2')),
    ])
    const tools = entries.filter((e): e is ToolEntry => e.kind === 'tool')
    expect(tools).toHaveLength(1)
    expect(tools[0].name).toBe('Bash')
    expect(tools[0].result).toBe('file1\nfile2')
    expect(tools[0].isError).toBe(false)
    // The result is nested into the tool, not emitted standalone.
    expect(entries.filter((e) => e.kind === 'raw')).toHaveLength(0)
  })

  it('pairs a tool_result that arrives before its tool_use (out of order)', () => {
    const entries = parseTranscript([
      user(toolResult('toolu_9', 'done')),
      assistant(toolUse('toolu_9', 'Read', { file_path: '/repo/a.ts' })),
    ])
    const tools = entries.filter((e): e is ToolEntry => e.kind === 'tool')
    expect(tools).toHaveLength(1)
    expect(tools[0].result).toBe('done')
    expect(entries.filter((e) => e.kind === 'raw')).toHaveLength(0)
  })

  it('renders an unmatched tool_result as a standalone raw entry', () => {
    const entries = parseTranscript([user(toolResult('missing', 'orphan output', true))])
    const raw = entries.filter((e) => e.kind === 'raw')
    expect(raw).toHaveLength(1)
    expect(raw[0]).toMatchObject({ kind: 'raw', content: 'orphan output', status: 'error' })
  })

  it('propagates is_error onto the paired tool entry', () => {
    const entries = parseTranscript([
      assistant(toolUse('t1', 'Bash', { command: 'boom' })),
      user(toolResult('t1', 'failed', true)),
    ])
    const tool = entries.find((e): e is ToolEntry => e.kind === 'tool')
    expect(tool?.isError).toBe(true)
    expect(tool?.result).toBe('failed')
  })
})

describe('toolHeadline summarizers', () => {
  const cwd = '/home/robin/source/Forge/.workers/Forge-got3'

  it('Bash prefers description, falls back to command', () => {
    expect(toolHeadline('Bash', { command: 'ls -la', description: 'List files' })).toBe(
      'List files',
    )
    expect(toolHeadline('Bash', { command: 'ls -la' })).toBe('ls -la')
  })

  it('Read/Write/Edit/NotebookEdit relativize the file path', () => {
    for (const name of ['Read', 'Write', 'Edit', 'NotebookEdit']) {
      expect(toolHeadline(name, { file_path: `${cwd}/internal/web/x.ts` }, cwd)).toBe(
        'internal/web/x.ts',
      )
    }
  })

  it('Grep/Glob use the pattern', () => {
    expect(toolHeadline('Grep', { pattern: 'TODO' })).toBe('TODO')
    expect(toolHeadline('Glob', { pattern: '**/*.ts' })).toBe('**/*.ts')
  })

  it('Task/Agent use the description', () => {
    expect(toolHeadline('Task', { description: 'Explore code', prompt: 'long...' })).toBe(
      'Explore code',
    )
  })

  it('WebFetch uses url, WebSearch uses query', () => {
    expect(toolHeadline('WebFetch', { url: 'https://example.com' })).toBe('https://example.com')
    expect(toolHeadline('WebSearch', { query: 'react markdown' })).toBe('react markdown')
  })

  it('TodoWrite gets a fixed headline and parses todos into a checklist', () => {
    expect(toolHeadline('TodoWrite', { todos: [] })).toBe('Update Todos')
    const entries = parseTranscript([
      assistant(
        toolUse('td', 'TodoWrite', {
          todos: [
            { content: 'Do A', status: 'completed' },
            { content: 'Do B', status: 'in_progress' },
          ],
        }),
      ),
    ])
    const tool = entries.find((e): e is ToolEntry => e.kind === 'tool')
    expect(tool?.todos).toEqual([
      { content: 'Do A', status: 'completed' },
      { content: 'Do B', status: 'in_progress' },
    ])
  })

  it('falls back to the first short string field, then compact JSON', () => {
    expect(toolHeadline('MysteryTool', { note: 'hello there' })).toBe('hello there')
    const jsonHeadline = toolHeadline('MysteryTool', { nested: { a: 1 }, count: 5 })
    expect(jsonHeadline).toContain('nested')
  })
})

describe('relativizePath', () => {
  it('strips the cwd prefix', () => {
    expect(relativizePath('/repo/root/src/a.ts', '/repo/root')).toBe('src/a.ts')
    expect(relativizePath('/repo/root/src/a.ts', '/repo/root/')).toBe('src/a.ts')
  })

  it('strips through a .workers/<id>/ worktree marker when no cwd is known', () => {
    expect(relativizePath('/home/x/source/Forge/.workers/Forge-got3/internal/y.ts')).toBe(
      'internal/y.ts',
    )
  })

  it('does not strip a prefix that is not a path-boundary match', () => {
    expect(relativizePath('/repo/root2/file', '/repo/root')).toBe('/repo/root2/file')
  })

  it('leaves unrelated paths untouched', () => {
    expect(relativizePath('/etc/hosts')).toBe('/etc/hosts')
    expect(relativizePath('/etc/hosts', '/repo/root')).toBe('/etc/hosts')
  })
})

describe('event classification', () => {
  it('classifies system/init as a meta line with model and session', () => {
    const entries = parseTranscript([
      JSON.stringify({
        type: 'system',
        subtype: 'init',
        cwd: '/x',
        model: 'claude-opus',
        session_id: 'sess-123',
      }),
    ])
    expect(entries).toHaveLength(1)
    expect(entries[0].kind).toBe('meta')
    if (entries[0].kind === 'meta') {
      expect(entries[0].text).toContain('claude-opus')
      expect(entries[0].text).toContain('sess-123')
    }
  })

  it('classifies noise events as hidden', () => {
    const lines = [
      JSON.stringify({ type: 'system', subtype: 'thinking_tokens', tokens: 5 }),
      JSON.stringify({ type: 'system', subtype: 'hook_started', hook: 'x' }),
      JSON.stringify({ type: 'system', subtype: 'hook_response', hook: 'x' }),
      JSON.stringify({ type: 'rate_limit_event', retry_after: 3 }),
    ]
    const entries = parseTranscript(lines)
    expect(entries.every((e) => e.kind === 'hidden')).toBe(true)
    const labels = entries.map((e) => (e.kind === 'hidden' ? e.label : ''))
    expect(labels).toEqual([
      'system/thinking_tokens',
      'system/hook_started',
      'system/hook_response',
      'rate_limit_event',
    ])
  })

  it('maps the final result event to a summary entry', () => {
    const entries = parseTranscript([
      JSON.stringify({
        type: 'result',
        subtype: 'success',
        duration_ms: 4321,
        num_turns: 7,
        total_cost_usd: 0.1234,
        usage: { input_tokens: 100, output_tokens: 200 },
      }),
    ])
    expect(entries).toHaveLength(1)
    const s = entries[0] as SummaryEntry
    expect(s.kind).toBe('summary')
    expect(s.durationMs).toBe(4321)
    expect(s.numTurns).toBe(7)
    expect(s.totalCostUsd).toBeCloseTo(0.1234)
    expect(s.inputTokens).toBe(100)
    expect(s.outputTokens).toBe(200)
  })

  it('renders assistant text and thinking as their own entries', () => {
    const entries = parseTranscript([
      assistant({ type: 'text', text: 'Hello **world**' }),
      assistant({ type: 'thinking', thinking: 'pondering' }),
    ])
    expect(entries.map((e) => e.kind)).toEqual(['assistant', 'thinking'])
  })

  it('never throws on malformed or non-JSON lines', () => {
    const entries = parseTranscript(['not json at all', '{"broken":', ''])
    // Empty line is dropped; the two others become raw entries.
    expect(entries.every((e) => e.kind === 'raw')).toBe(true)
    expect(entries).toHaveLength(2)
  })
})
