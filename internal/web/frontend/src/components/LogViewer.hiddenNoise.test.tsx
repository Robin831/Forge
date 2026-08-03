import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import LogViewer from './LogViewer'

// A thinking block whose plaintext was stripped, leaving only the signature.
// Claude emits these in bulk during a normal run; the signature alone runs to
// roughly 1.5KB on the wire.
const LONG_SIGNATURE = `ErQGCokBCBAYAipA${'Zm9vYmFy'.repeat(180)}`

function signatureOnlyThinking(): string {
  return JSON.stringify({
    type: 'assistant',
    message: {
      model: 'claude-opus-4-8',
      content: [{ type: 'thinking', thinking: '', signature: LONG_SIGNATURE }],
    },
  })
}

function textLine(text: string): string {
  return JSON.stringify({ type: 'assistant', message: { content: [{ type: 'text', text }] } })
}

afterEach(cleanup)

describe('LogViewer signature-only thinking noise', () => {
  it('keeps signature blobs out of the transcript by default', () => {
    render(<LogViewer rawLines={[signatureOnlyThinking(), textLine('real output')]} />)

    expect(screen.getByText('real output')).toBeInTheDocument()
    expect(screen.queryByText(/ErQGCokBCBAYAipA/)).not.toBeInTheDocument()
  })

  it('counts the suppressed records next to the verbose toggle', () => {
    render(<LogViewer rawLines={[signatureOnlyThinking(), signatureOnlyThinking()]} />)

    expect(screen.getByText('verbose (2)')).toBeInTheDocument()
  })

  it('reveals them clamped under verbose, expandable to the full record', async () => {
    const user = userEvent.setup()
    render(<LogViewer rawLines={[signatureOnlyThinking()]} />)

    await user.click(screen.getByRole('checkbox'))

    expect(screen.getByText('thinking (empty)')).toBeInTheDocument()
    // Clamped: the preview ends in an ellipsis rather than the whole record.
    const clamped = screen.getByText(/^\{"type":"assistant".*…$/)
    expect(clamped.textContent!.length).toBeLessThan(250)

    await user.click(screen.getByRole('button', { name: 'show more' }))
    expect(screen.getByText(new RegExp(LONG_SIGNATURE.slice(0, 40)))).toBeInTheDocument()
  })
})
