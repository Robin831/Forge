package warden

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTokenize_BasicSplitAndLowercase(t *testing.T) {
	got := Tokenize("Database Connection Leak Detected")
	for _, want := range []string{"database", "connection", "leak", "detected"} {
		_, ok := got[want]
		assert.True(t, ok, "expected token %q in set", want)
	}
}

func TestTokenize_DropsShortAndStopwords(t *testing.T) {
	got := Tokenize("the err is not nil")
	// "the" is a stopword, "is" / "not" too short or stopword, "nil" passes (3 chars, not stopword)
	_, hasErr := got["err"]
	_, hasNil := got["nil"]
	assert.True(t, hasErr, "expected 'err' to be tokenized")
	assert.True(t, hasNil, "expected 'nil' to be tokenized")
	_, hasThe := got["the"]
	assert.False(t, hasThe, "'the' should be dropped as a stopword")
}

func TestTokenize_EmptyInput(t *testing.T) {
	got := Tokenize("")
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestTokenize_NonAlphanumericSeparators(t *testing.T) {
	got := Tokenize("error.handling/missing-check")
	for _, want := range []string{"error", "handling", "missing", "check"} {
		_, ok := got[want]
		assert.True(t, ok, "expected token %q", want)
	}
}

func TestJaccard_Identical(t *testing.T) {
	a := Tokenize("missing error check")
	assert.InDelta(t, 1.0, Jaccard(a, a), 1e-9)
}

func TestJaccard_Disjoint(t *testing.T) {
	a := Tokenize("database connection leak")
	b := Tokenize("button label spelling")
	assert.InDelta(t, 0.0, Jaccard(a, b), 1e-9)
}

func TestJaccard_PartialOverlap(t *testing.T) {
	a := Tokenize("missing error check return")  // 4
	b := Tokenize("unchecked error return value") // 4 (unchecked, error, return, value)
	// intersection: {error, return} = 2
	// union: {missing, error, check, return, unchecked, value} = 6
	got := Jaccard(a, b)
	assert.InDelta(t, 2.0/6.0, got, 1e-9)
}

func TestJaccard_EmptyInputs(t *testing.T) {
	assert.Equal(t, 0.0, Jaccard(TokenSet{}, TokenSet{}))
	assert.Equal(t, 0.0, Jaccard(Tokenize("anything"), TokenSet{}))
}

func TestRuleWordBag_MergesPatternAndCheck(t *testing.T) {
	r := Rule{Pattern: "missing error check", Check: "verify error handling"}
	bag := RuleWordBag(r)
	for _, tok := range []string{"missing", "error", "check", "verify", "handling"} {
		_, ok := bag[tok]
		assert.True(t, ok, "expected token %q in bag", tok)
	}
}

func TestClusterByJaccard_SingleCluster(t *testing.T) {
	rules := []Rule{
		{ID: "r1", Pattern: "missing error check", Check: "verify error handling"},
		{ID: "r2", Pattern: "unchecked error return", Check: "verify error handling on returns"},
	}
	clusters := ClusterByJaccard(rules, 0.3)
	require := assert.New(t)
	require.Len(clusters, 1)
	require.Len(clusters[0].Rules, 2)
	require.Greater(clusters[0].MaxSimilarity, 0.3)
}

func TestClusterByJaccard_MultipleClusters(t *testing.T) {
	rules := []Rule{
		{ID: "r1", Pattern: "missing error check", Check: "error handling"},
		{ID: "r2", Pattern: "unchecked error", Check: "error handling on return"},
		{ID: "r3", Pattern: "spelling mistake", Check: "verify spelling in button"},
		{ID: "r4", Pattern: "button label typo", Check: "spelling consistency button"},
	}
	// Threshold chosen so r1<->r2 (J ≈ 0.333) and r3<->r4 (J ≈ 0.286) both
	// cluster, while cross-cluster pairs (J = 0) stay separate. Boundary
	// behavior is exercised by TestClusterByJaccard_BoundaryThreshold.
	clusters := ClusterByJaccard(rules, 0.25)
	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(clusters))
	}
	// Cluster order is by first occurrence: r1's cluster, then r3's.
	assert.Equal(t, "r1", clusters[0].Rules[0].ID)
	assert.Equal(t, "r3", clusters[1].Rules[0].ID)
}

func TestClusterByJaccard_NoMerge(t *testing.T) {
	rules := []Rule{
		{ID: "r1", Pattern: "alpha beta", Check: "gamma"},
		{ID: "r2", Pattern: "delta", Check: "epsilon"},
	}
	clusters := ClusterByJaccard(rules, 0.5)
	assert.Empty(t, clusters, "non-overlapping rules should not form clusters")
}

func TestClusterByJaccard_BoundaryThreshold(t *testing.T) {
	// Two rules whose Jaccard is exactly the threshold should cluster.
	rules := []Rule{
		{ID: "r1", Pattern: "alpha beta", Check: "gamma delta"},   // {alpha, beta, gamma, delta}
		{ID: "r2", Pattern: "alpha epsilon", Check: "zeta gamma"}, // {alpha, epsilon, zeta, gamma}
	}
	// intersection: {alpha, gamma} = 2; union: 6; jaccard = 2/6 ≈ 0.333
	clusters := ClusterByJaccard(rules, 0.33)
	assert.Len(t, clusters, 1, "boundary case should still cluster when sim >= threshold")
	clusters = ClusterByJaccard(rules, 0.5)
	assert.Empty(t, clusters)
}

func TestClusterByJaccard_TransitiveMerge(t *testing.T) {
	// r1<->r2 similar, r2<->r3 similar, but r1<->r3 not necessarily.
	rules := []Rule{
		{ID: "r1", Pattern: "alpha beta gamma", Check: "delta"},
		{ID: "r2", Pattern: "alpha beta", Check: "epsilon zeta"},
		{ID: "r3", Pattern: "epsilon zeta", Check: "eta theta"},
	}
	clusters := ClusterByJaccard(rules, 0.20)
	assert.Len(t, clusters, 1, "transitive matches should land in one cluster")
	assert.Len(t, clusters[0].Rules, 3)
}

func TestClusterByJaccard_EmptyOrSingle(t *testing.T) {
	assert.Nil(t, ClusterByJaccard(nil, 0.5))
	assert.Nil(t, ClusterByJaccard([]Rule{{ID: "only"}}, 0.5))
}

func TestGroupRulesByCategory(t *testing.T) {
	rules := []Rule{
		{ID: "r1", Category: "style"},
		{ID: "r2", Category: "security"},
		{ID: "r3", Category: "style"},
		{ID: "r4", Category: ""},
	}
	order, byCat := GroupRulesByCategory(rules)
	assert.Equal(t, []string{"style", "security", ""}, order)
	assert.Len(t, byCat["style"], 2)
	assert.Len(t, byCat["security"], 1)
	assert.Len(t, byCat[""], 1)
}
