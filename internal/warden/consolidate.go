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
// threshold is the Jaccard criterion; it is paired with the shipped
// DefaultOverlapThreshold so a terse restatement of a verbose rule — which
// Jaccard cannot score above |small|/|large| however complete the
// containment is — still clusters. See DedupParams.
//
// Behaviour notes:
//   - Categories with fewer than 2 rules are skipped (no clustering possible).
//   - When threshold <= 0, the WHOLE pass is a no-op (returns nil, nil, nil).
//     The overlap criterion is not applied on its own: threshold is the
//     configured off switch, and a zero that still merged rules by another
//     measure would be an off switch that does not switch anything off.
//   - When runner returns an error for a specific cluster the cluster is
//     skipped (logged via the returned error slice) so a single AI failure
//     does not block other consolidations from completing.
//
// runner may be nil; the default aiRunner is used in that case. Callers
// wanting a specific warden-stage provider chain should pass a runner built
// for that chain (e.g. via DefaultConsolidationRunner).
func Consolidate(ctx context.Context, repoDir string, rf *RulesFile, threshold float64, runner ConsolidationRunner) (replaced []Rule, summary []MergeResult, errs []error) {
	if threshold <= 0 {
		return nil, nil, nil
	}
	return ConsolidateWithParams(ctx, repoDir, rf, DedupParams{Jaccard: threshold, Overlap: DefaultOverlapThreshold}, runner)
}

// ConsolidateWithParams is Consolidate with both near-duplicate criteria
// supplied explicitly. Consolidate is the one-knob form that pairs the
// configured Jaccard threshold with the shipped overlap default.
func ConsolidateWithParams(ctx context.Context, repoDir string, rf *RulesFile, params DedupParams, runner ConsolidationRunner) (replaced []Rule, summary []MergeResult, errs []error) {
	if rf == nil || params.IsZero() || len(rf.Rules) < 2 {
		return nil, nil, nil
	}

	catOrder, byCat := GroupRulesByCategory(rf.Rules)

	var clusters []categorizedCluster
	for _, cat := range catOrder {
		rules := byCat[cat]
		if len(rules) < 2 {
			continue
		}
		for _, c := range ClusterNearDuplicates(rules, params) {
			clusters = append(clusters, categorizedCluster{Cluster: c, Category: cat})
		}
	}

	return applyClusters(ctx, repoDir, rf, clusters, runner)
}

// ConsolidateBatch is the intra-batch pass: it clusters ONLY the rules named
// by batchIDs — the ones this smelter run is adding — against each other, and
// it does so ACROSS categories.
//
// It exists because the whole-file pass cannot stand in for it. That pass
// partitions by category first, and a batch's near-duplicates routinely do
// not share one: PR #682 landed eight distillations of "the documented log
// filename must match what the code produces" spread over `style`,
// `documentation`, `testing` and `other`, five of "the atomicity comment must
// match reality", and four of "delete the handle only if you still own it" —
// 90 rules holding 16 clusters, every duplicate a copy in every Warden prompt
// and a slot spent out of the MaxRules cap. Two of those clusters carried
// directly contradictory guidance, so the Warden would have flagged an
// implementation whichever convention it followed (see DetectContradictions).
//
// Clustering across categories is safe here in a way it would not be for the
// whole file: the members are rules the same run is introducing, no reviewer
// has ever seen them, and merging them changes nothing that was already in
// effect. The merged rule takes the cluster's most common category (ties
// broken by first appearance) so it keeps landing in a category the category
// filter selects for.
//
// Rules in rf that are not named by batchIDs are neither clustered nor
// touched — deduping the batch against the existing file stays the whole-file
// pass's job, and doing both here would merge a brand-new rule into an
// established one behind the same log line.
func ConsolidateBatch(ctx context.Context, repoDir string, rf *RulesFile, batchIDs []string, params DedupParams, runner ConsolidationRunner) (replaced []Rule, summary []MergeResult, errs []error) {
	if rf == nil || params.IsZero() || len(batchIDs) < 2 {
		return nil, nil, nil
	}

	inBatch := make(map[string]struct{}, len(batchIDs))
	for _, id := range batchIDs {
		if id != "" {
			inBatch[id] = struct{}{}
		}
	}
	if len(inBatch) < 2 {
		return nil, nil, nil
	}

	// Walk rf.Rules rather than batchIDs so the clustering input is in the
	// file's own order: the merge is deterministic for a given file, and a
	// caller that hands over the same IDs in a different order gets the same
	// clusters and the same merged rule.
	var batch []Rule
	for _, r := range rf.Rules {
		if _, ok := inBatch[r.ID]; ok {
			batch = append(batch, r)
		}
	}
	if len(batch) < 2 {
		return nil, nil, nil
	}

	var clusters []categorizedCluster
	for _, c := range ClusterNearDuplicates(batch, params) {
		clusters = append(clusters, categorizedCluster{Cluster: c, Category: dominantCategory(c.Rules)})
	}

	return applyClusters(ctx, repoDir, rf, clusters, runner)
}

// categorizedCluster is a cluster plus the category its merged rule will
// carry. The whole-file pass takes it from the partition it clustered
// within; the batch pass derives it from the members.
type categorizedCluster struct {
	Cluster
	Category string
}

// dominantCategory returns the most frequent category among rules, breaking
// ties by first appearance so the result is stable for a given input order.
func dominantCategory(rules []Rule) string {
	counts := make(map[string]int, len(rules))
	var order []string
	for _, r := range rules {
		if _, seen := counts[r.Category]; !seen {
			order = append(order, r.Category)
		}
		counts[r.Category]++
	}
	best := ""
	bestN := -1
	for _, cat := range order {
		if counts[cat] > bestN {
			best, bestN = cat, counts[cat]
		}
	}
	return best
}

// applyClusters distills and merges each cluster, rewrites rf.Rules, and
// returns the superseded rules plus the per-cluster summary. It is the one
// implementation both consolidation passes share, so neither can pick a
// merged ID, order the rebuilt file or archive a replaced rule differently
// from the other.
func applyClusters(ctx context.Context, repoDir string, rf *RulesFile, clusters []categorizedCluster, runner ConsolidationRunner) (replaced []Rule, summary []MergeResult, errs []error) {
	if len(clusters) == 0 {
		return nil, nil, nil
	}

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

	for _, cc := range clusters {
		pattern, check, suggestedID, err := DistillMergedRule(ctx, repoDir, cc.Rules, runner)
		if err != nil {
			errs = append(errs, fmt.Errorf("consolidating cluster (category=%s, size=%d): %w", cc.Category, len(cc.Rules), err))
			continue
		}

		// Pick the merged ID while the cluster members' IDs are still in
		// activeIDs so the merged rule always gets an ID distinct from every
		// replaced rule. Reusing a replaced ID would create a self-referential
		// archive entry (superseded_by == own ID). Remove the replaced IDs
		// from activeIDs only after the merged rule's ID has been chosen.
		mergedRule := MergeRule(cc.Rules, cc.Category, pattern, check, suggestedID, activeIDs)
		activeIDs[mergedRule.ID] = struct{}{}
		for _, mem := range cc.Rules {
			delete(activeIDs, mem.ID)
		}

		ids := make([]string, 0, len(cc.Rules))
		for _, mem := range cc.Rules {
			removed[mem.ID] = struct{}{}
			replaced = append(replaced, mem)
			ids = append(ids, mem.ID)
		}
		merged = append(merged, mergedRule)

		summary = append(summary, MergeResult{
			Merged:        mergedRule,
			ReplacedIDs:   ids,
			Category:      cc.Category,
			MaxSimilarity: cc.MaxSimilarity,
		})
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
