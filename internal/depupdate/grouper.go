package depupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/Robin831/Forge/internal/executil"
)

// UpdateGroup represents a set of related package updates that should be
// installed together atomically.
type UpdateGroup struct {
	Name      string                  // e.g. "vite ecosystem", "@tailwindcss packages", "lodash"
	Updates   []depcheck.ModuleUpdate // packages in this group
	Kind      string                  // worst-case kind: "major" > "minor" > "patch"
	Ecosystem string                  // e.g. "Go", "npm", "NuGet"
	SourceDir string                  // directory containing the manifest (e.g. package.json); empty means repo root
}

// taggedUpdate pairs a ModuleUpdate with its ecosystem for internal processing.
type taggedUpdate struct {
	depcheck.ModuleUpdate
	Ecosystem string
}

// peerDepFetcher fetches peer dependencies for an npm package at a given version.
// It is a package-level variable so tests can replace it with a stub.
var peerDepFetcher = fetchPeerDeps

// GroupUpdates takes scan results for a single anvil and groups related
// packages so they can be installed atomically.
//
// Grouping strategies are applied in order:
//  1. Peer dep groups (npm only) — packages sharing peer dependencies
//  2. Scope groups — packages sharing an npm scope (@scope/*)
//  3. Standalone — each remaining package becomes its own group
func GroupUpdates(ctx context.Context, results []*depcheck.CheckResult) []UpdateGroup {
	// Flatten all updates with ecosystem tags.
	var all []taggedUpdate
	for _, cr := range results {
		if cr == nil || cr.Error != nil {
			continue
		}
		for _, u := range concat(cr.Patch, cr.Minor, cr.Major) {
			all = append(all, taggedUpdate{ModuleUpdate: u, Ecosystem: cr.Ecosystem})
		}
	}
	if len(all) == 0 {
		return nil
	}

	grouped := make(map[string]bool)
	var groups []UpdateGroup

	// --- 1. Peer dep groups (npm only) ---
	var npmPkgs []taggedUpdate
	for _, u := range all {
		if u.Ecosystem == "npm" {
			npmPkgs = append(npmPkgs, u)
		}
	}

	if len(npmPkgs) > 0 {
		peerGroups := buildPeerDepGroups(ctx, npmPkgs)
		for _, g := range peerGroups {
			groups = append(groups, g)
			for _, u := range g.Updates {
				grouped[u.Path] = true
			}
		}
	}

	// --- 2. Scope groups ---
	// scopeKey encodes both scope and ecosystem to avoid cross-ecosystem collisions.
	type scopeKey struct{ scope, ecosystem string }
	scopeMembers := make(map[scopeKey][]depcheck.ModuleUpdate)
	for _, u := range all {
		if grouped[u.Path] {
			continue
		}
		scope := extractScope(u.Path)
		if scope == "" {
			continue
		}
		k := scopeKey{scope, u.Ecosystem}
		scopeMembers[k] = append(scopeMembers[k], u.ModuleUpdate)
	}
	for k, members := range scopeMembers {
		if len(members) < 2 {
			continue // single scoped package — let standalone handle it
		}
		groups = append(groups, UpdateGroup{
			Name:      "@" + k.scope + " packages",
			Updates:   members,
			Kind:      worstKind(members),
			Ecosystem: k.ecosystem,
			SourceDir: members[0].SourceDir,
		})
		for _, m := range members {
			grouped[m.Path] = true
		}
	}

	// --- 3. Standalone ---
	for _, u := range all {
		if grouped[u.Path] {
			continue
		}
		groups = append(groups, UpdateGroup{
			Name:      u.Path,
			Updates:   []depcheck.ModuleUpdate{u.ModuleUpdate},
			Kind:      u.Kind,
			Ecosystem: u.Ecosystem,
			SourceDir: u.SourceDir,
		})
	}

	// Sort groups for deterministic output: by Name, then Kind.
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Name != groups[j].Name {
			return groups[i].Name < groups[j].Name
		}
		return groups[i].Kind < groups[j].Kind
	})
	// Sort each group's updates by Path.
	for i := range groups {
		sort.Slice(groups[i].Updates, func(a, b int) bool {
			return groups[i].Updates[a].Path < groups[i].Updates[b].Path
		})
	}

	return groups
}

// buildPeerDepGroups discovers peer dependency relationships among outdated npm
// packages and merges connected packages into groups via union-find.
func buildPeerDepGroups(ctx context.Context, npmPkgs []taggedUpdate) []UpdateGroup {
	// Build an npm-only lookup set to avoid matching peer deps against
	// non-npm packages that happen to share a name.
	npmSet := make(map[string]struct{}, len(npmPkgs))
	for _, u := range npmPkgs {
		npmSet[u.Path] = struct{}{}
	}

	// Fetch peer deps with caching to avoid duplicate npm view calls.
	peerCache := make(map[string]map[string]string) // "pkg@ver" → peer deps
	for _, u := range npmPkgs {
		key := u.Path + "@" + u.Latest
		if _, ok := peerCache[key]; ok {
			continue
		}
		peerCache[key] = peerDepFetcher(ctx, u.Path, u.Latest)
	}

	// Build union-find over npm package paths.
	uf := newUnionFind()
	for _, u := range npmPkgs {
		uf.add(u.Path)
	}

	// Track how many packages peer-depend on each package (to find roots)
	// and how many in-set peer deps each package declares (outgoing edges).
	dependedOn := make(map[string]int) // inbound: others peer-depend on this
	dependsOn := make(map[string]int)  // outbound: this package peers others in-set
	for _, u := range npmPkgs {
		key := u.Path + "@" + u.Latest
		for peerName := range peerCache[key] {
			if _, inSet := npmSet[peerName]; inSet {
				uf.union(u.Path, peerName)
				dependedOn[peerName]++
				dependsOn[u.Path]++
			}
		}
	}

	// Collect groups from union-find.
	groupMembers := make(map[string][]depcheck.ModuleUpdate)
	for _, u := range npmPkgs {
		root := uf.find(u.Path)
		groupMembers[root] = append(groupMembers[root], u.ModuleUpdate)
	}

	var groups []UpdateGroup
	for _, members := range groupMembers {
		if len(members) < 2 {
			continue // not a peer dep group
		}

		// Name after the "root" package: most depended on by others,
		// breaking ties by fewest outgoing peer deps (true roots don't
		// depend on others), then alphabetically for determinism.
		bestRoot := members[0].Path
		for _, m := range members[1:] {
			if dependedOn[m.Path] > dependedOn[bestRoot] ||
				(dependedOn[m.Path] == dependedOn[bestRoot] && dependsOn[m.Path] < dependsOn[bestRoot]) ||
				(dependedOn[m.Path] == dependedOn[bestRoot] && dependsOn[m.Path] == dependsOn[bestRoot] && m.Path < bestRoot) {
				bestRoot = m.Path
			}
		}

		groups = append(groups, UpdateGroup{
			Name:      bestRoot + " ecosystem",
			Updates:   members,
			Kind:      worstKind(members),
			Ecosystem: "npm",
			SourceDir: members[0].SourceDir,
		})
	}
	return groups
}

// fetchPeerDeps runs `npm view <pkg>@<version> peerDependencies --json` and
// returns the peer dependency map. Returns nil on error or timeout (5s).
func fetchPeerDeps(ctx context.Context, pkg, version string) map[string]string {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "npm", "view",
		fmt.Sprintf("%s@%s", pkg, version), "peerDependencies", "--json"))

	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	out = bytes.TrimSpace(out)
	if len(out) == 0 || string(out) == "undefined" {
		return nil
	}

	var peers map[string]string
	if err := json.Unmarshal(out, &peers); err != nil {
		return nil
	}
	return peers
}

// extractScope returns the scope from a scoped package name like @scope/name.
// Returns empty string for unscoped packages.
func extractScope(path string) string {
	if !strings.HasPrefix(path, "@") {
		return ""
	}
	parts := strings.SplitN(path[1:], "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0]
}

// worstKind returns the most severe kind among a set of updates.
// major > minor > patch.
func worstKind(updates []depcheck.ModuleUpdate) string {
	severity := map[string]int{"major": 2, "minor": 1, "patch": 0}
	worst := "patch"
	for _, u := range updates {
		if severity[u.Kind] > severity[worst] {
			worst = u.Kind
		}
	}
	return worst
}

// concat merges multiple ModuleUpdate slices into one.
func concat(slices ...[]depcheck.ModuleUpdate) []depcheck.ModuleUpdate {
	var out []depcheck.ModuleUpdate
	for _, s := range slices {
		out = append(out, s...)
	}
	return out
}

// unionFind implements a simple disjoint-set data structure with path
// compression and union by rank.
type unionFind struct {
	parent map[string]string
	rank   map[string]int
}

func newUnionFind() *unionFind {
	return &unionFind{
		parent: make(map[string]string),
		rank:   make(map[string]int),
	}
}

func (uf *unionFind) add(x string) {
	if _, ok := uf.parent[x]; !ok {
		uf.parent[x] = x
	}
}

func (uf *unionFind) find(x string) string {
	if uf.parent[x] != x {
		uf.parent[x] = uf.find(uf.parent[x])
	}
	return uf.parent[x]
}

func (uf *unionFind) union(x, y string) {
	uf.add(x)
	uf.add(y)
	rx, ry := uf.find(x), uf.find(y)
	if rx == ry {
		return
	}
	if uf.rank[rx] < uf.rank[ry] {
		rx, ry = ry, rx
	}
	uf.parent[ry] = rx
	if uf.rank[rx] == uf.rank[ry] {
		uf.rank[rx]++
	}
}
