// Display helpers for the `created_by` value bd reports on every bead.
//
// The raw value is whatever the filing bd identity is called, and the same
// human can reach a shared anvil under more than one of them — a full display
// name from a work identity ("Anna Sophie Pettersen Sylta") and a compact
// handle from a personal one ("sophiesylta"). Rendering both verbatim shows one
// teammate as two people, which defeats the point of putting the creator on the
// row at all. Folding is done here, in the display layer, and never in the
// pipeline: the daemon passes bd's value through untouched, so a fold that gets
// it wrong is a rendering artefact rather than a corrupted record.
//
// Beads Forge files itself carry created_by "Forge". That is real signal about
// which work the machine raised, so it is displayed like any other creator and
// never blanked.

// Identity is one distinct created_by value, decomposed for comparison.
interface Identity {
  raw: string
  // key is the whole identity compacted to lowercase alphanumerics, so
  // "Anna Sylta", "anna.sylta" and "annasylta" all collapse to one string.
  key: string
  // tokens are the identity's name parts, lowercased, in order.
  tokens: string[]
}

// localPart strips an email domain, so "sophie@example.org" is compared and
// rendered as "sophie". bd emits bare handles far more often than addresses,
// but an address should not read as a different person from the handle in it.
function localPart(raw: string): string {
  const at = raw.indexOf('@')
  return at > 0 ? raw.slice(0, at) : raw
}

function identityOf(raw: string): Identity {
  const local = localPart(raw.trim())
  const tokens = local
    .split(/[^a-zA-Z0-9]+/)
    .filter(Boolean)
    .map((t) => t.toLowerCase())
  return { raw, key: tokens.join(''), tokens }
}

// concatOfTokenSubsequence reports whether `target` is exactly the
// concatenation of two or more of `tokens`, in order (gaps allowed). This is
// what folds a compact handle into a full name: "sophiesylta" is
// ["sophie", "sylta"] out of ["anna", "sophie", "pettersen", "sylta"].
//
// Two tokens is the floor deliberately. A single token would fold every
// "Robin" into any "Robin <surname>" present in the queue, which is a much
// likelier collision between two different people than a two-part match is.
function concatOfTokenSubsequence(target: string, tokens: string[]): boolean {
  if (!target || tokens.length < 2) return false
  // reachable maps a consumed-prefix length of `target` to the largest number
  // of tokens that can produce it.
  const reachable = new Map<number, number>([[0, 0]])
  for (const token of tokens) {
    for (const [offset, used] of Array.from(reachable.entries())) {
      if (!target.startsWith(token, offset)) continue
      const next = offset + token.length
      const prev = reachable.get(next)
      if (prev === undefined || prev < used + 1) {
        reachable.set(next, used + 1)
      }
    }
  }
  return (reachable.get(target.length) ?? 0) >= 2
}

// sameIdentity is the fold predicate: equal once punctuation and case are
// removed, or one is a compact handle spelled out of the other's name parts.
function sameIdentity(a: Identity, b: Identity): boolean {
  if (!a.key || !b.key) return false
  if (a.key === b.key) return true
  if (a.tokens.length === 1 && concatOfTokenSubsequence(a.key, b.tokens)) return true
  if (b.tokens.length === 1 && concatOfTokenSubsequence(b.key, a.tokens)) return true
  return false
}

// preferred picks which of two spellings of one person represents the pair:
// the more complete name first, then a spaced display name over a handle or an
// address, then the longer string, with a lexicographic tiebreak so the choice
// never depends on the order rows happen to arrive in.
function preferred(a: Identity, b: Identity): Identity {
  if (a.tokens.length !== b.tokens.length) return a.tokens.length > b.tokens.length ? a : b
  const aSpaced = /\s/.test(a.raw)
  const bSpaced = /\s/.test(b.raw)
  if (aSpaced !== bSpaced) return aSpaced ? a : b
  if (a.raw.length !== b.raw.length) return a.raw.length > b.raw.length ? a : b
  return a.raw <= b.raw ? a : b
}

/**
 * buildCreatorAliases folds the distinct created_by values in one set of rows
 * onto a canonical spelling per person, returning raw → canonical. Values with
 * no partner map to themselves, so callers can treat the map as total. The
 * result depends only on the set of values, not on their order.
 */
export function buildCreatorAliases(values: Iterable<string | undefined>): Map<string, string> {
  const identities: Identity[] = []
  const seen = new Set<string>()
  for (const value of values) {
    const raw = (value ?? '').trim()
    if (!raw || seen.has(raw)) continue
    seen.add(raw)
    const id = identityOf(raw)
    if (id.key) identities.push(id)
  }

  // Union-find over the fold predicate, so a chain of spellings (a full name,
  // a handle, and a punctuated variant) lands in one class rather than in two.
  const parent = identities.map((_, i) => i)
  const find = (i: number): number => {
    while (parent[i] !== i) {
      parent[i] = parent[parent[i]]
      i = parent[i]
    }
    return i
  }
  for (let i = 0; i < identities.length; i++) {
    for (let j = i + 1; j < identities.length; j++) {
      if (!sameIdentity(identities[i], identities[j])) continue
      const a = find(i)
      const b = find(j)
      if (a !== b) parent[a] = b
    }
  }

  const best = new Map<number, Identity>()
  for (let i = 0; i < identities.length; i++) {
    const root = find(i)
    const current = best.get(root)
    best.set(root, current ? preferred(current, identities[i]) : identities[i])
  }

  const aliases = new Map<string, string>()
  for (let i = 0; i < identities.length; i++) {
    aliases.set(identities[i].raw, best.get(find(i))!.raw)
  }
  return aliases
}

/**
 * canonicalCreator resolves one raw created_by through an alias map built by
 * buildCreatorAliases. Unknown or empty values pass through trimmed, so a row
 * rendered outside a built map still shows what bd said.
 */
export function canonicalCreator(raw: string | undefined, aliases?: Map<string, string>): string {
  const value = (raw ?? '').trim()
  if (!value) return ''
  return aliases?.get(value) ?? value
}

/**
 * creatorLabel shortens a creator for a byline: the email local part, and for a
 * multi-part display name the first and last part only ("Anna Sophie Pettersen
 * Sylta" → "Anna Sylta"). Callers put the full value in a title attribute, the
 * same way the byline's relative timestamp hides its ISO string. An empty
 * value yields an empty label — the row then renders no creator segment at all
 * rather than a placeholder.
 */
export function creatorLabel(raw: string | undefined): string {
  const value = localPart((raw ?? '').trim()).trim()
  if (!value) return ''
  const words = value.split(/\s+/).filter(Boolean)
  if (words.length <= 2) return words.join(' ')
  return `${words[0]} ${words[words.length - 1]}`
}

/**
 * creatorTitle is the tooltip for a rendered creator: the canonical spelling,
 * naming the row's own spelling as well when the two were folded together. A
 * fold that is wrong is then visible instead of silently rewriting who filed
 * the bead.
 */
export function creatorTitle(raw: string | undefined, canonical: string): string {
  const value = (raw ?? '').trim()
  if (!canonical) return value
  if (!value || value === canonical) return canonical
  return `${canonical} (filed as ${value})`
}

/**
 * creatorMatches reports whether a free-text query (already lowercased and
 * trimmed by the caller) selects a row by its creator. Every spelling the
 * operator might type is accepted: what bd reported, what the fold canonicalised
 * it to, and the shortened label actually on screen.
 */
export function creatorMatches(
  raw: string | undefined,
  query: string,
  aliases?: Map<string, string>,
): boolean {
  if (!query) return false
  const value = (raw ?? '').trim()
  if (!value) return false
  const canonical = canonicalCreator(value, aliases)
  return (
    value.toLowerCase().includes(query) ||
    canonical.toLowerCase().includes(query) ||
    creatorLabel(canonical).toLowerCase().includes(query)
  )
}

/**
 * creatorSortValue is the comparison key for the creator sort: the displayed
 * label of the canonical spelling, lowercased. Rows folded onto one person sort
 * together even when bd spelled them differently.
 */
export function creatorSortValue(raw: string | undefined, aliases?: Map<string, string>): string {
  return creatorLabel(canonicalCreator(raw, aliases)).toLowerCase()
}
