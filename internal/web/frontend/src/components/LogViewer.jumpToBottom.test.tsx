import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import LogViewer from './LogViewer'

// Build a claude assistant-text line so the viewer renders a visible entry.
function textLine(text: string): string {
  return JSON.stringify({
    type: 'assistant',
    message: { content: [{ type: 'text', text }] },
  })
}

// jsdom has no layout engine, so scrollHeight/clientHeight/scrollTop all read 0.
// Shim them on the scroll container (role="log") so the auto-follow math in
// LogViewer's onScroll handler behaves as if the element were scrolled up.
function shimScroll(el: HTMLElement, opts: { scrollHeight: number; clientHeight: number; scrollTop: number }) {
  let top = opts.scrollTop
  Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => opts.scrollHeight })
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => opts.clientHeight })
  Object.defineProperty(el, 'scrollTop', {
    configurable: true,
    get: () => top,
    set: (v: number) => {
      top = v
    },
  })
}

afterEach(() => cleanup())

describe('LogViewer jump-to-bottom', () => {
  it('is hidden while following and appears after the user scrolls up', () => {
    render(<LogViewer rawLines={[textLine('line 1')]} jumpToBottom />)

    // Auto-follow is on at mount → no jump button.
    expect(screen.queryByTestId('log-jump-to-bottom')).not.toBeInTheDocument()

    const log = screen.getByRole('log')
    shimScroll(log, { scrollHeight: 1000, clientHeight: 100, scrollTop: 0 })
    fireEvent.scroll(log)

    // Scrolled up ⇒ auto-follow suspended ⇒ button visible.
    expect(screen.getByTestId('log-jump-to-bottom')).toBeInTheDocument()
  })

  it('re-follows and hides the button when clicked', async () => {
    const user = userEvent.setup()
    render(<LogViewer rawLines={[textLine('line 1')]} jumpToBottom />)

    const log = screen.getByRole('log')
    shimScroll(log, { scrollHeight: 1000, clientHeight: 100, scrollTop: 0 })
    fireEvent.scroll(log)

    const button = screen.getByTestId('log-jump-to-bottom')
    await user.click(button)

    // Clicking snaps to the tail and re-enables auto-follow, hiding the button.
    expect(screen.queryByTestId('log-jump-to-bottom')).not.toBeInTheDocument()
    expect(log.scrollTop).toBe(1000)
  })

  it('does not render the button at all when jumpToBottom is off (modal default)', () => {
    render(<LogViewer rawLines={[textLine('line 1')]} />)
    const log = screen.getByRole('log')
    shimScroll(log, { scrollHeight: 1000, clientHeight: 100, scrollTop: 0 })
    fireEvent.scroll(log)
    expect(screen.queryByTestId('log-jump-to-bottom')).not.toBeInTheDocument()
  })
})
