// Shared types for the /prs tab. Kept colocated with PRsPage.tsx so the
// data-fetching and action sub-tasks (Forge-9ye8, Forge-x7dy) can extend a
// single source of truth without introducing a top-level API change.

export type PRSectionKind = 'forge_prs' | 'external_prs' | 'recently_merged'

// PRItem is a flat representation that covers all three sections. Forge-created
// PRs come from state.db (id present), external PRs come from `gh pr list`
// (repo present, is_external=true), recently-merged PRs may originate from
// either source.
export interface PRItem {
  id?: number
  number: number
  anvil: string
  repo?: string
  branch?: string
  base_branch?: string
  title: string
  url?: string
  author?: string
  status: string
  is_external: boolean
  is_conflicting?: boolean
  ci_passing?: boolean
  ci_failing?: boolean
  reviews_approved?: boolean
  bellows_assigned?: boolean
  ci_fix_count?: number
  review_fix_count?: number
  rebase_count?: number
  bead_id?: string
  created_at?: string
  updated_at?: string
  merged_at?: string
  closed_at?: string
}

export interface PRSection {
  kind: PRSectionKind
  title: string
  description?: string
  items: PRItem[]
}

// PRsResponse is the shape served by GET /api/prs/all (implemented in
// Forge-9ye8). Defined here so the page can be typed against it before the
// endpoint exists.
export interface PRsResponse {
  forge: PRItem[]
  external: PRItem[]
  recently_merged: PRItem[]
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
