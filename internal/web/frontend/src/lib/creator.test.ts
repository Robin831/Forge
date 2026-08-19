import { describe, expect, it } from 'vitest'
import {
  buildCreatorAliases,
  canonicalCreator,
  creatorLabel,
  creatorMatches,
  creatorSortValue,
  creatorTitle,
} from './creator'

describe('creatorLabel', () => {
  it('returns an empty label for a missing creator so no segment renders', () => {
    expect(creatorLabel(undefined)).toBe('')
    expect(creatorLabel('')).toBe('')
    expect(creatorLabel('   ')).toBe('')
  })

  it('keeps short names and machine creators verbatim', () => {
    expect(creatorLabel('Forge')).toBe('Forge')
    expect(creatorLabel('sophiesylta')).toBe('sophiesylta')
    expect(creatorLabel('Robin Smith')).toBe('Robin Smith')
  })

  it('shortens a long display name to its first and last part', () => {
    expect(creatorLabel('Anna Sophie Pettersen Sylta')).toBe('Anna Sylta')
  })

  it('drops an email domain', () => {
    expect(creatorLabel('sophie.sylta@example.org')).toBe('sophie.sylta')
  })
})

describe('buildCreatorAliases', () => {
  it('folds a compact handle into the full name it spells', () => {
    const aliases = buildCreatorAliases(['Anna Sophie Pettersen Sylta', 'sophiesylta'])
    expect(aliases.get('sophiesylta')).toBe('Anna Sophie Pettersen Sylta')
    expect(aliases.get('Anna Sophie Pettersen Sylta')).toBe('Anna Sophie Pettersen Sylta')
  })

  it('does not depend on the order the values arrive in', () => {
    const forward = buildCreatorAliases(['Anna Sophie Pettersen Sylta', 'sophiesylta'])
    const reverse = buildCreatorAliases(['sophiesylta', 'Anna Sophie Pettersen Sylta'])
    expect(reverse.get('sophiesylta')).toBe(forward.get('sophiesylta'))
    expect(reverse.get('Anna Sophie Pettersen Sylta')).toBe(
      forward.get('Anna Sophie Pettersen Sylta'),
    )
  })

  it('folds punctuation and case variants of one name', () => {
    const aliases = buildCreatorAliases(['Robin Smith', 'robin.smith', 'robin.smith@example.org'])
    const canonical = aliases.get('robin.smith')
    expect(canonical).toBeDefined()
    expect(aliases.get('Robin Smith')).toBe(canonical)
    expect(aliases.get('robin.smith@example.org')).toBe(canonical)
  })

  it('leaves distinct people alone', () => {
    const aliases = buildCreatorAliases(['Forge', 'Robin Smith', 'sophiesylta'])
    expect(aliases.get('Forge')).toBe('Forge')
    expect(aliases.get('Robin Smith')).toBe('Robin Smith')
    expect(aliases.get('sophiesylta')).toBe('sophiesylta')
  })

  it('does not fold on a single shared name part', () => {
    // "Robin" is a plausible handle for someone who is not "Robin Smith";
    // one matching part is too weak to merge two identities on.
    const aliases = buildCreatorAliases(['Robin Smith', 'robin'])
    expect(aliases.get('robin')).toBe('robin')
    expect(aliases.get('Robin Smith')).toBe('Robin Smith')
  })

  it('ignores blank values', () => {
    const aliases = buildCreatorAliases([undefined, '', '   ', 'Forge'])
    expect(aliases.size).toBe(1)
    expect(aliases.get('Forge')).toBe('Forge')
  })
})

describe('canonicalCreator', () => {
  it('passes an unknown value through trimmed', () => {
    expect(canonicalCreator('  Forge  ', new Map())).toBe('Forge')
    expect(canonicalCreator(undefined)).toBe('')
  })
})

describe('creatorTitle', () => {
  it('names the row’s own spelling when it was folded into another', () => {
    expect(creatorTitle('sophiesylta', 'Anna Sophie Pettersen Sylta')).toBe(
      'Anna Sophie Pettersen Sylta (filed as sophiesylta)',
    )
  })

  it('is just the value when nothing was folded', () => {
    expect(creatorTitle('Forge', 'Forge')).toBe('Forge')
  })
})

describe('creatorMatches', () => {
  const aliases = buildCreatorAliases(['Anna Sophie Pettersen Sylta', 'sophiesylta'])

  it('matches the raw value bd reported', () => {
    expect(creatorMatches('sophiesylta', 'sophiesylta', aliases)).toBe(true)
  })

  it('matches a folded identity by the canonical name', () => {
    expect(creatorMatches('sophiesylta', 'pettersen', aliases)).toBe(true)
  })

  it('matches the shortened label shown on the row', () => {
    expect(creatorMatches('Anna Sophie Pettersen Sylta', 'anna sylta', aliases)).toBe(true)
  })

  it('rejects a non-match and an absent creator', () => {
    expect(creatorMatches('Forge', 'sylta', aliases)).toBe(false)
    expect(creatorMatches(undefined, 'sylta', aliases)).toBe(false)
    expect(creatorMatches('Forge', '', aliases)).toBe(false)
  })
})

describe('creatorSortValue', () => {
  it('gives two spellings of one person the same sort key', () => {
    const aliases = buildCreatorAliases(['Anna Sophie Pettersen Sylta', 'sophiesylta'])
    expect(creatorSortValue('sophiesylta', aliases)).toBe(
      creatorSortValue('Anna Sophie Pettersen Sylta', aliases),
    )
  })

  it('is empty for a bead with no creator yet', () => {
    expect(creatorSortValue(undefined)).toBe('')
  })
})
