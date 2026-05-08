// View-specific types and copy constants for the /prs tab.
// API-shaped types (PRItem, PRsResponse) live in src/api.ts.

import type { PRItem } from '../api'

export type PRSectionKind = 'forge_prs' | 'external_prs' | 'recently_merged'

export interface PRSection {
  kind: PRSectionKind
  title: string
  description?: string
  items: PRItem[]
}

export const PR_SECTION_TITLES: Record<PRSectionKind, string> = {
  forge_prs: 'Forge PRs',
  external_prs: 'External PRs',
  recently_merged: 'Recently merged',
}

export const PR_SECTION_DESCRIPTIONS: Record<PRSectionKind, string> = {
  forge_prs: 'Open PRs created by Forge — track CI, review, and merge state.',
  external_prs: 'Open PRs from other contributors across registered anvils.',
  recently_merged: 'PRs merged in the last 7 days.',
}

export const PR_SECTION_EMPTY_MESSAGES: Record<PRSectionKind, string> = {
  forge_prs: 'No open Forge PRs.',
  external_prs: 'No open external PRs.',
  recently_merged: 'No PRs merged recently.',
}
