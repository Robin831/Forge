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
// and defers to distinctRuleID for the collision suffix.
//
// The suffix loop is distinctRuleID's and not a second copy of it: a rule
// added from the pending queue and a rule created by a merge are named by
// one rule, so a change to the scheme (padding, separator, a cap on
// attempts) cannot be applied to one path and missed on the other.
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
	return distinctRuleID(base, existingIDs)
}

// Consolidate runs the full consolidation pass over rf.Rules: it groups
// rules by category, clusters within each category at the given threshold,
// asks runner to merge each cluster, and replaces the cluster members in rf
// with their merged rule. Replaced rules are returned so the caller can
// archive them. The summary slice lists one MergeResult per consolidated
// cluster, suitable for inclusion in a commit message.
//
// threshold is the Jaccard criterion and the ONLY criterion this form
// applies. It deliberately does not pair it with DefaultOverlapThreshold:
// every caller of this function passes a single tuned number and has no way
// to say "and also merge by containment at 0.55", so silently adding the
// second criterion would merge rules on a measure the caller never opted
// into — including for an operator who raised the number precisely to
// suppress merging. A caller wanting both criteria says so through
// ConsolidateWithParams, which is what every production path does.
//
// Behaviour notes:
//   - Categories with fewer than 2 rules are skipped (no clustering possible).
//   - When threshold <= 0, the WHOLE pass is a no-op (returns nil, nil, nil).
//   - When runner returns an error for a specific cluster the cluster is
//     skipped (logged via the returned error slice) so a single AI failure
//     does not block other consolidations from completing.
//
// runner may be nil; the default aiRunner is used in that case. Callers
// wanting a specific warden-stage provider chain should pass a runner built
// for that chain (e.g. via DefaultConsolidationRunner).
//
// No production code calls this: the smelter flush and `forge warden
// consolidate` both go through ConsolidateWithParams so the overlap
// criterion is explicit at the call site. It is retained as the one-knob
// form for tests and for callers that genuinely want Jaccard alone.
func Consolidate(ctx context.Context, repoDir string, rf *RulesFile, threshold float64, runner ConsolidationRunner) (replaced []Rule, summary []MergeResult, errs []error) {
	if threshold <= 0 {
		return nil, nil, nil
	}
	return ConsolidateWithParams(ctx, repoDir, rf, DedupParams{Jaccard: threshold}, runner)
}

// ConsolidateWithParams is Consolidate with both near-duplicate criteria
// supplied explicitly, and is the form every production path uses.
//
// A zero DedupParams (neither criterion positive) is a no-op: with no
// criterion active no pair of rules can be judged a near-duplicate, so the
// O(n²) walk cannot return anything. The Smelter's own off switch is
// stricter than that — a non-positive Jaccard threshold skips the pass
// outright, so the overlap criterion is never applied on its own from
// configuration.
func ConsolidateWithParams(ctx context.Context, repoDir string, rf *RulesFile, params DedupParams, runner ConsolidationRunner) (replaced []Rule, summary []MergeResult, errs []error) {
	rep := ConsolidateWithParamsReport(ctx, repoDir, rf, params, runner)
	return rep.Replaced, rep.Summary, rep.Errors
}

// ConsolidationReport is what a consolidation pass DID, as opposed to what it
// achieved. Replaced, Summary and Errors are the three values
// ConsolidateWithParams has always returned; ClustersAttempted is the one a
// caller cannot derive from them, and the one a summary needs in order to be
// honest — "no clusters merged" and "every cluster the pass found failed"
// are the same empty Summary, and reporting the second as the first is how a
// consolidation that has merged nothing since May kept reporting the rules
// file as already at steady state.
type ConsolidationReport struct {
	// Replaced lists the rules superseded by the merges in Summary.
	Replaced []Rule
	// Summary holds one entry per cluster that merged.
	Summary []MergeResult
	// Errors holds one entry per cluster that did not, in cluster order.
	Errors []error
	// ClustersAttempted is the number of near-duplicate clusters the pass
	// found and tried to merge. It is the denominator: len(Summary) +
	// len(Errors) == ClustersAttempted for every pass, so a caller can say
	// "0/56 merged, 56 errored" rather than printing nothing at all.
	ClustersAttempted int
}

// ConsolidateWithParamsReport is ConsolidateWithParams with the attempted
// cluster count kept alongside the outcomes. The three-value form is a thin
// adapter over it, so neither can count a cluster the other does not.
func ConsolidateWithParamsReport(ctx context.Context, repoDir string, rf *RulesFile, params DedupParams, runner ConsolidationRunner) ConsolidationReport {
	if rf == nil || params.IsZero() || len(rf.Rules) < 2 {
		return ConsolidationReport{}
	}

	catOrder, byCat, posByCat := groupRulePositionsByCategory(rf.Rules)

	var clusters []categorizedCluster
	for _, cat := range catOrder {
		rules := byCat[cat]
		if len(rules) < 2 {
			continue
		}
		positions := posByCat[cat]
		for _, c := range ClusterNearDuplicates(rules, params) {
			clusters = append(clusters, categorizedCluster{
				Cluster:   c,
				Category:  cat,
				Positions: mapPositions(positions, c.Indices),
			})
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
// broken by first appearance), which keeps it landing in a category the
// category filter selects for on the majority members' file types — but not
// necessarily on the minority's, which is why splitSecurityBoundary refuses
// the one crossing where that trade loses coverage outright.
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
	//
	// An ID is only a usable handle on a rule while it names exactly one, and
	// nothing guarantees that: a rule's ID is written by whichever
	// distillation session produced it, and the rules file is ordinary
	// tracked YAML that a merge or a hand edit can leave holding two rules
	// under one ID. Selecting by ID would then pull a rule this flush never
	// added into the batch and — because the removal was by ID too — delete
	// both of them from the file while archiving one, which is one distinct
	// rule silently lost per collision. So a colliding ID is excluded from
	// the batch entirely and reported: this pass leaves the pair exactly as
	// it found them rather than guessing which one it was handed.
	counts := make(map[string]int, len(rf.Rules))
	for _, r := range rf.Rules {
		if _, ok := inBatch[r.ID]; ok {
			counts[r.ID]++
		}
	}
	ambiguous := make([]string, 0)
	for id, n := range counts {
		if n > 1 {
			ambiguous = append(ambiguous, id)
		}
	}
	if len(ambiguous) > 0 {
		sort.Strings(ambiguous)
		for _, id := range ambiguous {
			errs = append(errs, fmt.Errorf("intra-batch consolidation: rule id %q names %d rules in the file; excluded from the batch rather than merged by ID", id, counts[id]))
		}
	}

	var batch []Rule
	var positions []int
	for i, r := range rf.Rules {
		if _, ok := inBatch[r.ID]; !ok {
			continue
		}
		if counts[r.ID] > 1 {
			continue
		}
		batch = append(batch, r)
		positions = append(positions, i)
	}
	if len(batch) < 2 {
		return nil, nil, errs
	}

	var clusters []categorizedCluster
	for _, c := range ClusterNearDuplicates(batch, params) {
		for _, sub := range splitSecurityBoundary(c, params) {
			clusters = append(clusters, categorizedCluster{
				Cluster:   sub,
				Category:  dominantCategory(sub.Rules),
				Positions: mapPositions(positions, sub.Indices),
			})
		}
	}

	rep := applyClusters(ctx, repoDir, rf, clusters, runner)
	return rep.Replaced, rep.Summary, append(errs, rep.Errors...)
}

// securityCategory is the one category a cross-category merge may not move a
// rule out of. See splitSecurityBoundary.
const securityCategory = "security"

// splitSecurityBoundary divides a cross-category cluster into its security
// members and its non-security ones, dropping either side that is left with
// fewer than two rules. A cluster that does not straddle the boundary is
// returned unchanged.
//
// It exists because the merged rule's category is not cosmetic — it is a
// review-time gate. FilterRules drops a rule whose canonical category is not
// in the set categoriesForFile derives for the changed files, and those sets
// are narrow: a `.ts`/`.tsx`/`.css` diff selects {ui, style, other} and a
// `.cs` diff selects everything EXCEPT testing and ui. So a batch that
// learns one `security` rule alongside two near-duplicate `testing` ones —
// exactly the shape this pass exists to collapse, since the same check
// arrives under a different model-chosen category each session — would merge
// to dominantCategory `testing` and stop being reviewed on the next .NET
// diff, where the original security rule would have applied. Nothing in the
// commit message would say so: it reports a consolidation, not a change of
// scope.
//
// Splitting and not promoting, because the reverse move loses coverage too:
// a `ui` rule relabelled `security` disappears from every `.ts` diff. The
// only reclassification-free answer is to leave the two sides as separate
// rules, which costs one unmerged duplicate and keeps both rules landing
// wherever they landed before. MergeRule unions Paths for the same reason —
// "the merged rule still applies wherever its parents applied" — and the
// category is the one selector that union cannot preserve.
//
// Non-security members keep clustering with each other, so the batch's
// ordinary duplicates still collapse; only the boundary itself is refused.
//
// Each surviving side is RE-CLUSTERED rather than returned as it stands,
// because ClusterNearDuplicates unions transitively: a cluster is a
// connected component, not a clique, so removing a member can remove the
// only edge that held the rest together. A batch holding X (testing),
// S (security) and Y (testing) with X~S and S~Y above the threshold but
// X~Y below both criteria is one component; partitioned by category alone
// it would hand [X, Y] to applyClusters and merge two rules the
// near-duplicate test had explicitly declined to join — a merge on no
// evidence at all, and irreversible from the reviewer's point of view. The
// mirror case is a `sec` side of two security rules bridged by a
// non-security member. Re-running the clusterer over each side answers
// both: what comes back is whatever components survive the removal, which
// may be none.
func splitSecurityBoundary(c Cluster, p DedupParams) []Cluster {
	var sec, other Cluster
	sec.MaxSimilarity, other.MaxSimilarity = c.MaxSimilarity, c.MaxSimilarity
	for i, r := range c.Rules {
		if strings.TrimSpace(strings.ToLower(r.Category)) == securityCategory {
			sec.Rules = append(sec.Rules, r)
			sec.Indices = append(sec.Indices, c.Indices[i])
			continue
		}
		other.Rules = append(other.Rules, r)
		other.Indices = append(other.Indices, c.Indices[i])
	}
	if len(sec.Rules) == 0 || len(other.Rules) == 0 {
		return []Cluster{c}
	}
	var out []Cluster
	out = append(out, reclusterPartition(sec, p)...)
	out = append(out, reclusterPartition(other, p)...)
	return out
}

// reclusterPartition re-runs the near-duplicate clustering over one side of
// a split and remaps each resulting cluster's local indices back onto the
// indices the partition was carrying, so a member's identity survives the
// round trip. MaxSimilarity is taken from the sub-cluster rather than
// inherited from the parent: the pair that set the parent's score may have
// been the very edge the split removed.
func reclusterPartition(part Cluster, p DedupParams) []Cluster {
	if len(part.Rules) < 2 {
		return nil
	}
	var out []Cluster
	for _, sub := range ClusterNearDuplicates(part.Rules, p) {
		out = append(out, Cluster{
			Rules:         sub.Rules,
			Indices:       mapPositions(part.Indices, sub.Indices),
			MaxSimilarity: sub.MaxSimilarity,
		})
	}
	return out
}

// categorizedCluster is a cluster plus the category its merged rule will
// carry. The whole-file pass takes it from the partition it clustered
// within; the batch pass derives it from the members.
type categorizedCluster struct {
	Cluster
	Category string
	// Positions are the members' indices into rf.Rules — the identity
	// applyClusters removes and archives by. Cluster.Indices is relative to
	// whatever subset was clustered (one category's rules, or the batch), so
	// it cannot be used against the file directly.
	Positions []int
}

// mapPositions translates cluster-local indices into the positions of the
// slice the clustered subset was drawn from.
func mapPositions(subsetPositions, indices []int) []int {
	out := make([]int, 0, len(indices))
	for _, ix := range indices {
		out = append(out, subsetPositions[ix])
	}
	return out
}

// groupRulePositionsByCategory is GroupRulesByCategory plus, for each
// category, the positions in rules that its members came from. The two are
// built in one walk so a member and its position can never come apart.
func groupRulePositionsByCategory(rules []Rule) (order []string, byCat map[string][]Rule, posByCat map[string][]int) {
	byCat = make(map[string][]Rule)
	posByCat = make(map[string][]int)
	for i, r := range rules {
		if _, ok := byCat[r.Category]; !ok {
			order = append(order, r.Category)
		}
		byCat[r.Category] = append(byCat[r.Category], r)
		posByCat[r.Category] = append(posByCat[r.Category], i)
	}
	return order, byCat, posByCat
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
func applyClusters(ctx context.Context, repoDir string, rf *RulesFile, clusters []categorizedCluster, runner ConsolidationRunner) ConsolidationReport {
	// Counted here rather than by the caller because this is the one place a
	// cluster is actually attempted: every cluster below leaves either a
	// Summary entry or an Errors entry, so the denominator and the two
	// numerators are established together and cannot come apart.
	report := ConsolidationReport{ClustersAttempted: len(clusters)}
	if len(clusters) == 0 {
		return report
	}
	var (
		replaced []Rule
		summary  []MergeResult
		errs     []error
	)

	// Track which rule POSITIONS are removed so we can rebuild rf.Rules in
	// stable order after all clusters are processed. Positions and not IDs:
	// two rules in one file can carry the same ID, and removing by ID drops
	// every one of them while archiving only the member the cluster held.
	removed := make(map[int]struct{})
	var merged []Rule

	// Every ID the file has held during this run, for collision checking
	// when picking a merged rule's ID. A replaced rule's ID is never
	// released: reusing it would produce a self-referential archive entry
	// (superseded_by == own ID), and where the file holds a duplicate it
	// would hand the merged rule an ID a surviving rule still carries.
	usedIDs := make(map[string]struct{}, len(rf.Rules))
	for _, r := range rf.Rules {
		if r.ID != "" {
			usedIDs[r.ID] = struct{}{}
		}
	}

	for _, cc := range clusters {
		if len(cc.Positions) != len(cc.Rules) {
			// A cluster whose members cannot be located in rf.Rules is not
			// one this pass may act on: removing it would have to fall back
			// to matching by ID, which is the ambiguity Positions exists to
			// remove.
			errs = append(errs, fmt.Errorf("consolidating cluster (category=%s, size=%d): %d member position(s) supplied", cc.Category, len(cc.Rules), len(cc.Positions)))
			continue
		}
		pattern, check, suggestedID, err := DistillMergedRule(ctx, repoDir, cc.Rules, runner)
		if err != nil {
			errs = append(errs, fmt.Errorf("consolidating cluster (category=%s, size=%d): %w", cc.Category, len(cc.Rules), err))
			continue
		}

		mergedRule := MergeRule(cc.Rules, cc.Category, pattern, check, suggestedID, usedIDs)
		usedIDs[mergedRule.ID] = struct{}{}

		ids := make([]string, 0, len(cc.Rules))
		for i, mem := range cc.Rules {
			removed[cc.Positions[i]] = struct{}{}
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
		report.Errors = errs
		return report
	}

	// Rebuild rf.Rules: keep original order minus removed, then append
	// merged rules at the end so the active file remains diff-readable.
	newRules := make([]Rule, 0, len(rf.Rules)-len(removed)+len(merged))
	for i, r := range rf.Rules {
		if _, gone := removed[i]; gone {
			continue
		}
		newRules = append(newRules, r)
	}
	newRules = append(newRules, merged...)
	rf.Rules = newRules

	report.Replaced = replaced
	report.Summary = summary
	report.Errors = errs
	return report
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
