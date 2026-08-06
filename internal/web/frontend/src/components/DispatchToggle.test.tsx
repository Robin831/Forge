import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'

import DispatchToggle, { describePauseCause } from './DispatchToggle'

afterEach(cleanup)

describe('describePauseCause', () => {
  it('labels an operator pause as manual', () => {
    expect(describePauseCause('manual')).toBe('manual')
  })

  it('treats a missing reason as manual (older daemon)', () => {
    expect(describePauseCause(undefined)).toBe('manual')
    expect(describePauseCause('')).toBe('manual')
  })

  it('names a self-deploy drain and appends its detail', () => {
    expect(describePauseCause('self-deploy')).toBe('self-deploy drain')
    expect(describePauseCause('self-deploy', 'waiting on 2 workers, max 30m')).toBe(
      'self-deploy drain, waiting on 2 workers, max 30m',
    )
  })

  it('renders an unknown reason verbatim rather than mislabelling it', () => {
    expect(describePauseCause('hot-reload')).toBe('hot-reload')
  })
})

describe('DispatchToggle', () => {
  it('shows no pause badge while dispatch runs', () => {
    render(<DispatchToggle paused={false} />)
    expect(screen.queryByText(/dispatch paused/)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /pause/i })).toBeInTheDocument()
  })

  it('names the cause of a self-deploy pause instead of calling it manual', () => {
    render(
      <DispatchToggle
        paused
        reason="self-deploy"
        detail="waiting on 2 workers, max 30m"
      />,
    )
    expect(
      screen.getByText(/dispatch paused \(self-deploy drain, waiting on 2 workers, max 30m\)/),
    ).toBeInTheDocument()
  })

  it('keeps the paused-since suffix on a manual pause', () => {
    render(<DispatchToggle paused reason="manual" pausedSince="2026-08-06T19:50:00Z" />)
    expect(screen.getByText(/dispatch paused \(manual\)/)).toBeInTheDocument()
    expect(screen.getByText(/ since /)).toBeInTheDocument()
  })
})
