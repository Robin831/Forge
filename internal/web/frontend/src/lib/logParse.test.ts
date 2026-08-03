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

  // Tool acknowledgements are verbose but say nothing the headline doesn't.
  // Edit/Write acks are exactly one line, so the line-based preview cap never
  // trimmed them and they rendered in full on every call.
  describe('result summaries', () => {
    function summaryFor(name: string, result: string, isError = false): string | undefined {
      const entries = parseTranscript([
        assistant(toolUse('t1', name, { file_path: '/w/a.ts' })),
        user(toolResult('t1', result, isError)),
      ])
      return (entries[0] as ToolEntry).resultSummary
    }

    it('suppresses Edit and Write success acknowledgements', () => {
      expect(
        summaryFor('Edit', 'The file /home/forge/anvils/x/.workers/y/a.md has been updated successfully. (file state is current in your context — no need to Read it back)'),
      ).toBe('')
      expect(summaryFor('Write', 'File created successfully at: /home/forge/x/park.go')).toBe('')
      expect(summaryFor('NotebookEdit', 'The cell has been updated successfully.')).toBe('')
    })

    it('collapses a file Read to its line count', () => {
      expect(summaryFor('Read', '     1→alpha\n     2→beta\n     3→gamma')).toBe('Read 3 lines')
      expect(summaryFor('Read', '     1→only')).toBe('Read 1 line')
    })

    // Real worker logs carry both line-number shapes; the tab form is the one
    // the current CLI emits and is by far the more common of the two.
    it('collapses cat -n style Read output, including offset reads', () => {
      expect(summaryFor('Read', '1\tpackage daemon\n2\t\n3\timport "sync"')).toBe('Read 3 lines')
      expect(summaryFor('Read', '1140\trecheckUseCount := 0\n1141\t')).toBe('Read 2 lines')
    })

    it('collapses the backgrounded-Bash boilerplate to the task id', () => {
      const result = [
        'Command running in background with ID: bb329k505',
        'Output is being written to: /tmp/claude-1000/x/tasks/bb329k505.output',
        'Session cwd remains /home/forge/anvils/x/.workers/y',
      ].join('\n')
      expect(summaryFor('Bash', result)).toBe('running in background (bb329k505)')
    })

    it('leaves real output, non-file reads, and errors alone', () => {
      expect(summaryFor('Bash', 'Exit code 1\nCONFLICT in package.json')).toBeUndefined()
      expect(summaryFor('Read', 'Image dimensions 800x600')).toBeUndefined()
      expect(summaryFor('Grep', '23: match here')).toBeUndefined()
      // An Edit that failed must stay readable in full.
      expect(summaryFor('Edit', 'has been updated successfully', true)).toBeUndefined()
    })

    it('leaves the full result on the entry so the expander can reveal it', () => {
      const ack = 'File created successfully at: /home/forge/x/park.go'
      const entries = parseTranscript([
        assistant(toolUse('t1', 'Write', { file_path: '/w/park.go' })),
        user(toolResult('t1', ack)),
      ])
      expect(entries[0]).toMatchObject({ kind: 'tool', result: ack, resultSummary: '' })
    })
  })

  // Regression: these used to fall through to a 'raw' entry holding the whole
  // wire record, which the verbose toggle cannot filter — so a run's worth of
  // signature-only thinking blocks buried the transcript.
  it('hides assistant records whose blocks render nothing', () => {
    const entries = parseTranscript([
      // Thinking with the plaintext stripped, signature retained.
      assistant({ type: 'thinking', thinking: '', signature: 'ErQGCokBCBAYAipA' }),
      assistant({ type: 'text', text: '' }),
      assistant({ type: 'redacted_thinking', data: 'AAAAB' }),
    ])
    expect(entries.map((e) => e.kind)).toEqual(['hidden', 'hidden', 'hidden'])
    expect(entries.map((e) => (e.kind === 'hidden' ? e.label : ''))).toEqual([
      'thinking (empty)',
      'text (empty)',
      'redacted_thinking (empty)',
    ])
  })

  it('keeps the raw record on a hidden entry so verbose can reveal it', () => {
    const line = assistant({ type: 'thinking', thinking: '', signature: 'sig-abc' })
    const entries = parseTranscript([line])
    expect(entries[0]).toMatchObject({ kind: 'hidden', content: line })
  })

  it('still renders sibling blocks when only some are empty', () => {
    const entries = parseTranscript([
      assistant({ type: 'thinking', thinking: '', signature: 'sig' }, { type: 'text', text: 'hi' }),
    ])
    expect(entries.map((e) => e.kind)).toEqual(['assistant'])
  })

  it('never throws on malformed or non-JSON lines', () => {
    const entries = parseTranscript(['not json at all', '{"broken":', ''])
    // Empty line is dropped; the two others become raw entries.
    expect(entries.every((e) => e.kind === 'raw')).toBe(true)
    expect(entries).toHaveLength(2)
  })
})
