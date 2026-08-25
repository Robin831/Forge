package assay

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// firstDiff reports the byte offset at which a and b first differ, together
// with the surrounding context of each, or ok=false when they are identical.
// A bare "prefixes differ" failure on two multi-kilobyte strings is unactionable
// — the point of the assertion is to name which section drifted.
func firstDiff(a, b string) (off int, ctxA, ctxB string, ok bool) {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	if i == len(a) && i == len(b) {
		return 0, "", "", false
	}
	window := func(s string) string {
		lo := max(i-80, 0)
		hi := min(i+80, len(s))
		return s[lo:hi]
	}
	return i, window(a), window(b), true
}

// runMetadataVariant returns the fixture request with every field that belongs
// to a RUN rather than to the pull request moved: a different head, a different
// incremental baseline, a different bead, and the prior findings handed over in
// reverse. None of it may reach the stable prefix.
func runMetadataVariant(req ReviewRequest) ReviewRequest {
	out := req
	out.HeadSHA = "0000feed0000feed0000feed0000feed0000feed"
	out.BaselineSHA = "9999beef9999beef9999beef9999beef9999beef"
	out.BeadID = "Forge-zzzz"
	out.WorkDir = "/tmp/some-other-worktree"
	out.PriorFindings = reversedFindings(req.PriorFindings)
	return out
}

func reversedFindings(in []PriorFinding) []PriorFinding {
	out := make([]PriorFinding, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		out = append(out, in[i])
	}
	return out
}

// TestStablePrefixIsByteStableAcrossRuns is the cross-run counterpart to
// TestDeepPassPromptsShareCachePrefix. That test guards the prefix the five
// passes of ONE run share; this one guards the prefix a SECOND run of the same
// PR must find already in the cache. A prompt cache matches from the first
// byte, so a single run-varying byte up here — a head SHA, a baseline, a bead
// id, or prior findings in whatever order the DB returned them — costs the
// re-run the whole prefix behind it.
func TestStablePrefixIsByteStableAcrossRuns(t *testing.T) {
	base, _, _ := prefixFixture()
	variant := runMetadataVariant(base)

	first := stablePrefix(base)
	second := stablePrefix(variant)
	if off, a, b, differ := firstDiff(first, second); differ {
		t.Fatalf("stable prefix differs between two runs of the same PR at byte %d\n run 1: %q\n run 2: %q", off, a, b)
	}
	if first == "" {
		t.Fatal("stable prefix is empty; the fixture is not exercising it")
	}

	// The prefix must actually carry the PR-scoped material, or byte equality
	// is trivially satisfied by rendering nothing.
	for _, want := range []string{
		sharedPromptPreamble,
		"Repository Review Guidance",
		"integration test",
		"Change Context",
		"Reject negative charge amounts",
		"Already-Reported Findings",
		"unchecked error",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("stable prefix is missing %q", want)
		}
	}

	// And it must carry none of the run metadata. The baseline SHA is the one
	// worth naming twice: it used to sit above the already-reported list, so a
	// push invalidated the cache several kilobytes higher than the diff it
	// describes.
	for _, unwanted := range []string{
		shortSHA(base.HeadSHA), shortSHA(variant.HeadSHA),
		shortSHA(base.BaselineSHA), shortSHA(variant.BaselineSHA),
		base.BeadID, variant.BeadID,
		"Incremental Review",
	} {
		if unwanted == "" {
			continue
		}
		if strings.Contains(first, unwanted) {
			t.Errorf("run metadata %q leaked into the stable prefix", unwanted)
		}
	}
}

// TestStablePrefixIgnoresPriorFindingOrder is what actually catches a
// regression to DB-order (or map-iteration) rendering. Go map iteration and a
// SQLite scan without an ORDER BY are both free to hand back the same rows in a
// different sequence on a later read, and neither reliably does so twice in one
// process — so the shuffle has to be explicit.
func TestStablePrefixIgnoresPriorFindingOrder(t *testing.T) {
	req, _, _ := prefixFixture()
	req.PriorFindings = []PriorFinding{
		{Anchor: "internal/pay/charge.go:12", Severity: "Nit", Title: "name the constant"},
		{Anchor: "internal/pay/charge.go:12", Severity: "Nit", Title: "add a unit"},
		{Anchor: "internal/pay/charge.go:9", Severity: "Important", Title: "unchecked error"},
		{Anchor: "internal/pay/ledger.go:88", Severity: "Important", Title: "unchecked error", Resolved: true},
		{Anchor: "internal/pay/ledger.go:2", Severity: "Minor", Title: "stale comment"},
		// The next two reach the digest tiebreaker: they tie with an entry
		// above on file, line, severity and title alike, so nothing before the
		// digest separates them. slices.SortFunc is unstable, so a tie is left
		// in input — i.e. DB — order, which is the instability this whole file
		// exists to remove. See TestSortedPriorFindingsTiesResolveByDigest for
		// the direct assertion; their presence here is what makes THIS shuffle
		// loop fail when the digest stops discriminating.
		{Anchor: "internal/pay/charge.go:12", Severity: "Nit", Title: "name the constant", Resolved: true},
		{Anchor: "internal/pay/charge.go:12-15", Severity: "Nit", Title: "name the constant"},
		{Anchor: "docs/pay.md", Severity: "Nit", Title: "no line anchor"},
	}
	want := stablePrefix(req)

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 40; i++ {
		shuffled := req
		shuffled.PriorFindings = append([]PriorFinding(nil), req.PriorFindings...)
		rng.Shuffle(len(shuffled.PriorFindings), func(a, b int) {
			shuffled.PriorFindings[a], shuffled.PriorFindings[b] = shuffled.PriorFindings[b], shuffled.PriorFindings[a]
		})
		if off, a, b, differ := firstDiff(want, stablePrefix(shuffled)); differ {
			t.Fatalf("shuffle %d changed the stable prefix at byte %d\n want: %q\n got:  %q", i, off, a, b)
		}
	}

	// Sorting must not mutate the caller's slice: one ReviewRequest is shared
	// by five deep passes building their prompts on five goroutines.
	if req.PriorFindings[0].Anchor != "internal/pay/charge.go:12" ||
		req.PriorFindings[len(req.PriorFindings)-1].Anchor != "docs/pay.md" {
		t.Fatalf("sortedPriorFindings reordered the caller's slice in place: %+v", req.PriorFindings)
	}
}

// TestSortedPriorFindingsTiesResolveByDigest pins the one key that makes the
// order total. Every key above the digest — file, line, severity, title — can
// tie: two findings can differ only in Resolved (which renders as a visible
// " (resolved)" suffix), and two distinct anchors can parse to the same
// file/line ("a.go:12" against the range "a.go:12-15", since parseAnchor
// anchors a range on its start). slices.SortFunc is NOT stable, so entries the
// comparator calls equal come back in whatever order they went in — for real
// prior findings, the order SQLite handed them over in.
//
// So the digest is not defensive garnish, it is the load-bearing line: with it
// mutated to a constant the rest of the suite still passes, and only a fixture
// that actually reaches it fails.
func TestSortedPriorFindingsTiesResolveByDigest(t *testing.T) {
	tied := []PriorFinding{
		{Anchor: "internal/pay/charge.go:12", Severity: "Nit", Title: "name the constant"},
		{Anchor: "internal/pay/charge.go:12", Severity: "Nit", Title: "name the constant", Resolved: true},
		{Anchor: "internal/pay/charge.go:12-15", Severity: "Nit", Title: "name the constant"},
	}

	// The fixture is only worth anything while it really does tie on every key
	// above the digest; an edit that separates the entries by title would leave
	// this test green against a digest that discriminates nothing.
	for i := range tied {
		for j := i + 1; j < len(tied); j++ {
			a, b := parseAnchor(tied[i].Anchor), parseAnchor(tied[j].Anchor)
			if a.file != b.file || a.line != b.line ||
				tied[i].Severity != tied[j].Severity || tied[i].Title != tied[j].Title {
				t.Fatalf("fixture entries %d and %d do not reach the digest tiebreaker: %+v vs %+v", i, j, tied[i], tied[j])
			}
		}
	}

	// And the digest must separate what the keys above it could not — every
	// field distinguishing two PriorFindings has to be hashed, Resolved
	// included.
	seen := map[string]int{}
	for i, p := range tied {
		if prev, dup := seen[priorFindingDigest(p)]; dup {
			t.Fatalf("findings %d and %d share a digest, so the order is not total: %+v vs %+v", prev, i, tied[prev], p)
		}
		seen[priorFindingDigest(p)] = i
	}

	want := sortedPriorFindings(tied)
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 40; i++ {
		shuffled := append([]PriorFinding(nil), tied...)
		rng.Shuffle(len(shuffled), func(a, b int) {
			shuffled[a], shuffled[b] = shuffled[b], shuffled[a]
		})
		if got := sortedPriorFindings(shuffled); !slices.Equal(got, want) {
			t.Fatalf("shuffle %d reordered digest-tied findings\n want: %+v\n got:  %+v", i, want, got)
		}
	}
}

// TestSortedPriorFindingsOrdersExtremeLineNumbers covers the comparator's line
// key on values parseAnchor does not bound. Anchors are model-authored text
// read back out of pr_findings, and parseAnchor accumulates digits with no
// range check, so a long enough tail overflows to an arbitrary — possibly
// large-negative — int. A subtraction comparator wraps on such a pair and
// reports the larger line as the smaller one, which is not a strict weak
// ordering and lets the sort return a different permutation per input order.
func TestSortedPriorFindingsOrdersExtremeLineNumbers(t *testing.T) {
	// One anchorless entry (line -1) and two whose tails overflow int64 in
	// opposite directions once parseAnchor is through with them.
	extreme := []PriorFinding{
		{Anchor: "internal/pay/charge.go", Severity: "Nit", Title: "no line at all"},
		{Anchor: "internal/pay/charge.go:99999999999999999999", Severity: "Nit", Title: "overflowing tail"},
		{Anchor: "internal/pay/charge.go:88888888888888888888", Severity: "Nit", Title: "another overflowing tail"},
		{Anchor: "internal/pay/charge.go:7", Severity: "Nit", Title: "an ordinary line"},
	}
	want := sortedPriorFindings(extreme)

	rng := rand.New(rand.NewSource(13))
	for i := 0; i < 40; i++ {
		shuffled := append([]PriorFinding(nil), extreme...)
		rng.Shuffle(len(shuffled), func(a, b int) {
			shuffled[a], shuffled[b] = shuffled[b], shuffled[a]
		})
		if got := sortedPriorFindings(shuffled); !slices.Equal(got, want) {
			t.Fatalf("shuffle %d reordered findings with out-of-range line anchors\n want: %+v\n got:  %+v", i, want, got)
		}
	}
}

// TestPriorFindingsCapIsOrderIndependent covers the truncation half: past
// maxPriorFindingsListed the block lists a subset, and which subset must come
// from the total order rather than from the order rows arrived in.
func TestPriorFindingsCapIsOrderIndependent(t *testing.T) {
	req, _, _ := prefixFixture()
	findings := make([]PriorFinding, 0, maxPriorFindingsListed+37)
	for i := 0; i < cap(findings); i++ {
		findings = append(findings, PriorFinding{
			Anchor:   fmt.Sprintf("internal/pkg%02d/file.go:%d", i%7, i),
			Severity: "Minor",
			Title:    fmt.Sprintf("finding number %d", i),
		})
	}
	req.PriorFindings = findings
	want := priorFindingsSection(req)

	if !strings.Contains(want, fmt.Sprintf("…and %d more not listed", len(findings)-maxPriorFindingsListed)) {
		t.Fatalf("cap notice missing from:\n%s", want)
	}
	if got := strings.Count(want, "\n- ["); got != maxPriorFindingsListed {
		t.Fatalf("listed %d findings, want %d", got, maxPriorFindingsListed)
	}

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20; i++ {
		shuffled := req
		shuffled.PriorFindings = append([]PriorFinding(nil), findings...)
		rng.Shuffle(len(shuffled.PriorFindings), func(a, b int) {
			shuffled.PriorFindings[a], shuffled.PriorFindings[b] = shuffled.PriorFindings[b], shuffled.PriorFindings[a]
		})
		if got := priorFindingsSection(shuffled); got != want {
			off, a, b, _ := firstDiff(want, got)
			t.Fatalf("shuffle %d truncated to a different hundred at byte %d\n want: %q\n got:  %q", i, off, a, b)
		}
	}
}

// TestPassPromptsOpenWithStablePrefix pins the seam: the stable region is not a
// substring some caller happens to reproduce, it is the literal opening of
// every prompt the engine builds — triage included.
func TestPassPromptsOpenWithStablePrefix(t *testing.T) {
	req, unifiedDiff, notes := prefixFixture()
	prefix := stablePrefix(req)

	for _, p := range deepPasses {
		got, err := buildPassPrompt(p, req, unifiedDiff, notes)
		if err != nil {
			t.Fatalf("buildPassPrompt(%s): %v", p.Name, err)
		}
		if !strings.HasPrefix(got, prefix) {
			off, a, b, _ := firstDiff(prefix, got)
			t.Fatalf("pass %s does not open with the stable prefix; first difference at byte %d\n want: %q\n got:  %q", p.Name, off, a, b)
		}
	}
	triagePrompt, err := buildTriagePrompt(req, unifiedDiff)
	if err != nil {
		t.Fatalf("buildTriagePrompt: %v", err)
	}
	if !strings.HasPrefix(triagePrompt, prefix) {
		t.Error("triage prompt does not open with the stable prefix")
	}
}

// TestRunMetadataSurvivesBelowThePrefix is the other half of the relocation:
// nothing was dropped on the way out of the prefix. The incremental framing and
// its baseline commit still reach the model, just below the stable region and
// adjacent to the diff they describe.
func TestRunMetadataSurvivesBelowThePrefix(t *testing.T) {
	req, unifiedDiff, notes := prefixFixture()
	got, err := buildPassPrompt(deepPasses[0], req, unifiedDiff, notes)
	if err != nil {
		t.Fatalf("buildPassPrompt: %v", err)
	}
	prefix := stablePrefix(req)
	tail := got[len(prefix):]

	for _, want := range []string{
		"Incremental Review",
		shortSHA(req.BaselineSHA),
		"Triage Notes",
		notes,
		unifiedDiff,
	} {
		if !strings.Contains(tail, want) {
			t.Errorf("relocated section %q is missing from the prompt below the stable prefix", want)
		}
	}

	// Order below the prefix: framing, then notes, then the diff the two
	// describe. A note or a baseline landing after the diff would read as
	// commentary on nothing.
	iInc := strings.Index(tail, "## Incremental Review")
	iNotes := strings.Index(tail, "## Triage Notes")
	iDiff := strings.Index(tail, "## Diff Under Review")
	if !(iInc >= 0 && iInc < iNotes && iNotes < iDiff) {
		t.Errorf("sections below the prefix are out of order: incremental=%d notes=%d diff=%d", iInc, iNotes, iDiff)
	}
}
