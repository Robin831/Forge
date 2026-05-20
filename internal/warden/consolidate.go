package warden

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ConsolidationRunner is the AI invocation hook for merging a cluster of
// rules into a canonical single rule. It receives the working directory and
// the prompt and returns the raw response bytes.
//
// In production this is satisfied by warden.DefaultConsolidationRunner.
// Tests inject a stub that returns a deterministic JSON response.
type ConsolidationRunner func(ctx context.Context, dir, prompt string) ([]byte, error)

// DefaultConsolidationRunner returns a ConsolidationRunner that invokes the
// standard warden-stage AI provider chain (Claude → Gemini fallback). Use
// this when wiring the Smelter from the daemon.
func DefaultConsolidationRunner() ConsolidationRunner {
	return func(ctx context.Context, dir, prompt string) ([]byte, error) {
		return aiRunner(ctx, dir, prompt)
	}
}

// MergeResult describes one cluster consolidation outcome.
type MergeResult struct {
	// Merged is the new canonical rule replacing the cluster.
	Merged Rule
	// ReplacedIDs lists the IDs of the rules that were superseded by Merged.
	ReplacedIDs []string
	// Category is the shared category across the cluster.
	Category string
	// MaxSimilarity is the highest pairwise Jaccard score observed in the
	// cluster (carried through from ClusterByJaccard).
	MaxSimilarity float64
}

// DistillMergedRule asks the warden-stage AI provider to consolidate a
// cluster of near-duplicate rules into one canonical rule. The cluster is
// passed verbatim; the response must be a single JSON object describing the
// merged rule's pattern and check. The caller is responsible for stitching
// in metadata (sources, added timestamp, ID).
//
// runner is the AI invocation function. When nil, aiRunner is used so this
// helper works with the standard provider fallback chain.
func DistillMergedRule(ctx context.Context, repoDir string, cluster []Rule, runner ConsolidationRunner) (pattern, check string, suggestedID string, err error) {
	if len(cluster) < 2 {
		return "", "", "", fmt.Errorf("DistillMergedRule: cluster must contain at least 2 rules (got %d)", len(cluster))
	}
	if runner == nil {
		runner = aiRunner
	}

	prompt := buildConsolidationPrompt(cluster)
	raw, err := runner(ctx, repoDir, prompt)
	if err != nil {
		return "", "", "", fmt.Errorf("ai consolidate: %w", err)
	}

	output := strings.TrimSpace(string(raw))
	jsonStr := extractJSON(output, "check")
	if jsonStr == "" {
		jsonStr = output
	}

	var parsed struct {
		ID      string `json:"id"`
		Pattern string `json:"pattern"`
		Check   string `json:"check"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return "", "", "", fmt.Errorf("parsing merged rule: %w (output: %s)", err, output)
	}
	if strings.TrimSpace(parsed.Check) == "" {
		return "", "", "", fmt.Errorf("merged rule missing required field: check")
	}

	return parsed.Pattern, parsed.Check, parsed.ID, nil
}

// buildConsolidationPrompt renders the warden-stage AI prompt for merging a
// cluster of similar rules into a single canonical rule.
func buildConsolidationPrompt(cluster []Rule) string {
	var sb strings.Builder
	sb.WriteString("You are consolidating a set of near-duplicate code review rules into a single canonical rule.\n\n")
	sb.WriteString("Each rule below targets a similar concern. Produce ONE merged rule whose `pattern` and `check` describe the shared intent clearly and concisely. Do not narrow the scope so much that any individual rule's concern is dropped.\n\n")
	sb.WriteString("## Rules to Consolidate\n\n")
	for i, r := range cluster {
		fmt.Fprintf(&sb, "### Rule %d (id: %s)\n- pattern: %s\n- check: %s\n\n", i+1, r.ID, r.Pattern, r.Check)
	}
	sb.WriteString("## Output Format\n\n")
	sb.WriteString("Respond with ONLY a JSON object (no markdown fences, no explanation) in this exact format:\n\n")
	sb.WriteString(`{"id": "short-kebab-case-id", "pattern": "shared pattern", "check": "what reviewers must verify"}`)
	sb.WriteString("\n")
	return sb.String()
}

// MergeMetadata combines the deduplicated source list and oldest Added
// timestamp from a cluster of rules. Returned Added is in YYYY-MM-DD form;
// if no cluster member carries a parseable Added date, the returned string
// is empty (callers can default to "today").
func MergeMetadata(cluster []Rule) (sources SourceList, oldestAdded string) {
	seen := make(map[string]struct{})
	var dedup []string
	for _, r := range cluster {
		for _, s := range r.Source {
			if s == "" {
				continue
			}
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			dedup = append(dedup, s)
		}
	}
	sort.Strings(dedup)
	sources = SourceList(dedup)

	const layout = "2006-01-02"
	var oldest time.Time
	for _, r := range cluster {
		if r.Added == "" {
			continue
		}
		t, err := time.Parse(layout, r.Added)
		if err != nil {
			continue
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	if !oldest.IsZero() {
		oldestAdded = oldest.Format(layout)
	}
	return sources, oldestAdded
}

// MergeRule builds the canonical merged Rule from a cluster, the AI-derived
// pattern/check, and a freshly minted ID. existingIDs is consulted so the
// generated ID does not collide with active rules in the file; when a
// collision is found, a numeric suffix is appended.
func MergeRule(cluster []Rule, category, pattern, check, suggestedID string, existingIDs map[string]struct{}) Rule {
	sources, oldestAdded := MergeMetadata(cluster)
	if oldestAdded == "" {
		oldestAdded = time.Now().UTC().Format("2006-01-02")
	}

	id := pickMergedID(suggestedID, cluster, existingIDs)

	// Union paths across cluster members; preserve first-seen order so the
	// merged rule still applies wherever its parents applied.
	var paths []string
	pathSeen := make(map[string]struct{})
	for _, r := range cluster {
		for _, p := range r.Paths {
			if _, ok := pathSeen[p]; ok {
				continue
			}
			pathSeen[p] = struct{}{}
			paths = append(paths, p)
		}
	}

	return Rule{
		ID:       id,
		Category: category,
		Pattern:  pattern,
		Check:    check,
		Source:   sources,
		Added:    oldestAdded,
		Paths:    paths,
	}
}

// pickMergedID chooses an ID for the merged rule. It prefers a non-empty
// AI-suggested ID, falls back to "merged-<first-id>" when none is given,
// and appends "-N" if the chosen ID collides with existing rules.
func pickMergedID(suggested string, cluster []Rule, existingIDs map[string]struct{}) string {
	base := strings.TrimSpace(suggested)
	if base == "" {
		if len(cluster) > 0 && cluster[0].ID != "" {
			base = "merged-" + cluster[0].ID
		} else {
			base = "merged-rule"
		}
	}
	if _, clash := existingIDs[base]; !clash {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, clash := existingIDs[candidate]; !clash {
			return candidate
		}
	}
}

// Consolidate runs the full consolidation pass over rf.Rules: it groups
// rules by category, clusters within each category at the given threshold,
// asks runner to merge each cluster, and replaces the cluster members in rf
// with their merged rule. Replaced rules are returned so the caller can
// archive them. The summary slice lists one MergeResult per consolidated
// cluster, suitable for inclusion in a commit message.
//
// Behaviour notes:
//   - Categories with fewer than 2 rules are skipped (no clustering possible).
//   - When threshold <= 0, the pass is a no-op (returns nil, nil, nil).
//   - When runner returns an error for a specific cluster the cluster is
//     skipped (logged via the returned error slice) so a single AI failure
//     does not block other consolidations from completing.
//
// runner may be nil; the default aiRunner is used in that case. Callers
// wanting a specific warden-stage provider chain should pass a runner built
// for that chain (e.g. via DefaultConsolidationRunner).
func Consolidate(ctx context.Context, repoDir string, rf *RulesFile, threshold float64, runner ConsolidationRunner) (replaced []Rule, summary []MergeResult, errs []error) {
	if rf == nil || threshold <= 0 || len(rf.Rules) < 2 {
		return nil, nil, nil
	}

	catOrder, byCat := GroupRulesByCategory(rf.Rules)

	// Track which rule IDs are removed so we can rebuild rf.Rules in
	// stable order after all clusters are processed.
	removed := make(map[string]struct{})
	var merged []Rule

	// Set of currently active IDs (excludes removed) for collision checking
	// when picking the merged ID.
	activeIDs := make(map[string]struct{}, len(rf.Rules))
	for _, r := range rf.Rules {
		if r.ID != "" {
			activeIDs[r.ID] = struct{}{}
		}
	}

	for _, cat := range catOrder {
		rules := byCat[cat]
		if len(rules) < 2 {
			continue
		}
		clusters := ClusterByJaccard(rules, threshold)
		for _, c := range clusters {
			pattern, check, suggestedID, err := DistillMergedRule(ctx, repoDir, c.Rules, runner)
			if err != nil {
				errs = append(errs, fmt.Errorf("consolidating cluster (category=%s, size=%d): %w", cat, len(c.Rules), err))
				continue
			}

			// Free the cluster members' IDs from activeIDs before picking
			// the merged ID — the merged rule may reasonably reuse one of
			// the cluster's existing IDs without colliding.
			for _, mem := range c.Rules {
				delete(activeIDs, mem.ID)
			}
			mergedRule := MergeRule(c.Rules, cat, pattern, check, suggestedID, activeIDs)
			activeIDs[mergedRule.ID] = struct{}{}

			ids := make([]string, 0, len(c.Rules))
			for _, mem := range c.Rules {
				removed[mem.ID] = struct{}{}
				replaced = append(replaced, mem)
				ids = append(ids, mem.ID)
			}
			merged = append(merged, mergedRule)

			summary = append(summary, MergeResult{
				Merged:        mergedRule,
				ReplacedIDs:   ids,
				Category:      cat,
				MaxSimilarity: c.MaxSimilarity,
			})
		}
	}

	if len(merged) == 0 {
		return nil, nil, errs
	}

	// Rebuild rf.Rules: keep original order minus removed, then append
	// merged rules at the end so the active file remains diff-readable.
	newRules := make([]Rule, 0, len(rf.Rules)-len(replaced)+len(merged))
	for _, r := range rf.Rules {
		if _, gone := removed[r.ID]; gone {
			continue
		}
		newRules = append(newRules, r)
	}
	newRules = append(newRules, merged...)
	rf.Rules = newRules

	return replaced, summary, errs
}

// FormatConsolidationSummary renders a human-readable bullet list of the
// consolidations performed in a smelter run. Used in the commit/PR body.
// Returns "" when summary is empty so callers can omit the section entirely.
func FormatConsolidationSummary(summary []MergeResult) string {
	if len(summary) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Consolidated near-duplicate rules:\n")
	for _, r := range summary {
		cat := r.Category
		if cat == "" {
			cat = "(no category)"
		}
		fmt.Fprintf(&sb, "- [%s] %s ← %s (sim=%.2f)\n",
			cat, r.Merged.ID, strings.Join(r.ReplacedIDs, ", "), r.MaxSimilarity)
	}
	return sb.String()
}
