import '@testing-library/jest-dom/vitest'
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'

import WedgedAnvilsBanner from './WedgedAnvilsBanner'

afterEach(cleanup)

describe('WedgedAnvilsBanner', () => {
  it('renders nothing when no anvil is wedged', () => {
    const { container } = render(<WedgedAnvilsBanner anvils={[]} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('names the anvil, its conflicted tables and the divergence', () => {
    render(
      <WedgedAnvilsBanner
        anvils={[
          {
            anvil: 'munin',
            conflict_tables: 'issues (3)',
            conflict_count: 3,
            branch: 'beads-sync',
            ahead: 1,
            behind: 10,
            divergence_known: true,
            detail: 'Beads database is mid-merge with unresolved conflicts.',
          },
        ]}
      />,
    )
    const banner = screen.getByRole('region', { name: 'Wedged anvils' })
    expect(banner).toHaveTextContent('1 anvil is wedged')
    expect(banner).toHaveTextContent('munin')
    expect(banner).toHaveTextContent('issues (3)')
    expect(banner).toHaveTextContent('beads-sync ahead 1 / behind 10')
    expect(banner).toHaveTextContent(
      'Beads database is mid-merge with unresolved conflicts.',
    )
  })

  it('omits a zero detected_at rather than reporting a millennia-old wedge', () => {
    // Go marshals a zero time.Time as year 0001 — `omitempty` does not apply to
    // structs — so the banner must not render it as a relative age.
    render(
      <WedgedAnvilsBanner
        anvils={[{ anvil: 'munin', detected_at: '0001-01-01T00:00:00Z' }]}
      />,
    )
    expect(
      screen.getByRole('region', { name: 'Wedged anvils' }),
    ).not.toHaveTextContent('detected')
  })

  it('says the banner clears itself so the operator does not hunt for a dismiss action', () => {
    render(<WedgedAnvilsBanner anvils={[{ anvil: 'munin' }]} />)
    const banner = screen.getByRole('region', { name: 'Wedged anvils' })
    expect(banner).toHaveTextContent('there is nothing to dismiss here')
    expect(banner).toHaveTextContent(
      'this banner clears itself on the next poll once dolt_conflicts is empty',
    )
  })

  it('pluralises the headline for multiple wedged anvils', () => {
    render(
      <WedgedAnvilsBanner
        anvils={[{ anvil: 'munin' }, { anvil: 'hugin' }]}
      />,
    )
    expect(
      screen.getByRole('region', { name: 'Wedged anvils' }),
    ).toHaveTextContent('2 anvils are wedged')
  })
})
