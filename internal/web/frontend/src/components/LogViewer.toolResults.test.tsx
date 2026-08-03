import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import LogViewer from './LogViewer'

// Build a tool call plus its paired result, the way the CLI streams them.
function toolCall(name: string, input: unknown, result: string, isError = false): string[] {
  return [
    JSON.stringify({
      type: 'assistant',
      message: { content: [{ type: 'tool_use', id: 't1', name, input }] },
    }),
    JSON.stringify({
      type: 'user',
      message: {
        content: [{ type: 'tool_result', tool_use_id: 't1', content: result, is_error: isError }],
      },
    }),
  ]
}

const EDIT_ACK =
  'The file /home/forge/anvils/fhi.metadata/.workers/Fhi.Metadata-p05z4/changelog.d/x.md has been updated successfully. (file state is current in your context — no need to Read it back)'

afterEach(cleanup)

describe('LogViewer tool result noise', () => {
  it('drops the Edit success acknowledgement and its absolute path', () => {
    render(<LogViewer rawLines={toolCall('Edit', { file_path: '/w/x.md' }, EDIT_ACK)} />)

    expect(screen.getByText('Edit')).toBeInTheDocument()
    expect(screen.queryByText(/has been updated successfully/)).not.toBeInTheDocument()
    expect(screen.queryByText(/anvils\/fhi\.metadata/)).not.toBeInTheDocument()
  })

  it('keeps the acknowledgement reachable behind the expander', async () => {
    const user = userEvent.setup()
    render(<LogViewer rawLines={toolCall('Edit', { file_path: '/w/x.md' }, EDIT_ACK)} />)

    await user.click(screen.getByRole('button', { name: 'show more' }))
    expect(screen.getByText(/has been updated successfully/)).toBeInTheDocument()
  })

  it('collapses a file Read to its line count instead of dumping content', () => {
    const body = ['     1→category: Changed', '     2→- **A very long entry**', '     3→'].join('\n')
    render(<LogViewer rawLines={toolCall('Read', { file_path: '/w/x.md' }, body)} />)

    expect(screen.getByText('Read 3 lines')).toBeInTheDocument()
    expect(screen.queryByText(/category: Changed/)).not.toBeInTheDocument()
  })

  it('collapses the backgrounded-Bash boilerplate to the task id', () => {
    const body = [
      'Command running in background with ID: bb329k505',
      'Output is being written to: /tmp/claude-1000/x/tasks/bb329k505.output',
      'Session cwd remains /home/forge/anvils/x/.workers/y; directory changes do not apply.',
    ].join('\n')
    render(<LogViewer rawLines={toolCall('Bash', { command: 'dotnet test' }, body)} />)

    expect(screen.getByText('running in background (bb329k505)')).toBeInTheDocument()
    expect(screen.queryByText(/Output is being written to/)).not.toBeInTheDocument()
  })

  it('caps a single long line that the line cap cannot bound', () => {
    const oneLongLine = `PREFIX_${'lorem ipsum dolor sit amet '.repeat(200)}`
    render(<LogViewer rawLines={toolCall('Grep', { pattern: 'x' }, oneLongLine)} />)

    const rendered = screen.getByText(/^PREFIX_/)
    expect(rendered.textContent!.length).toBeLessThan(300)
    expect(rendered.textContent!.endsWith('…')).toBe(true)
  })

  it('does not put a "show input" link on an ordinary tool row', () => {
    render(<LogViewer rawLines={toolCall('Bash', { command: 'ls' }, 'a.txt\nb.txt')} />)

    expect(screen.queryByRole('button', { name: 'show input' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /show/ })).not.toBeInTheDocument()
  })

  it('reveals raw input when the tool name is clicked', async () => {
    const user = userEvent.setup()
    // With a description present the headline shows that, so the command
    // itself only appears once the raw input is revealed.
    const input = { command: 'ls -la /srv', description: 'List services' }
    render(<LogViewer rawLines={toolCall('Bash', input, 'a.txt')} />)

    expect(screen.getByText('(List services)')).toBeInTheDocument()
    expect(screen.queryByText(/ls -la \/srv/)).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /^Bash/ }))
    expect(screen.getByText(/ls -la \/srv/)).toBeInTheDocument()
  })

  it('still shows a failed tool result in full', () => {
    render(
      <LogViewer rawLines={toolCall('Edit', { file_path: '/w/x.md' }, 'String not found', true)} />,
    )

    expect(screen.getByText('String not found')).toBeInTheDocument()
  })
})
