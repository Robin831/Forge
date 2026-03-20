package depupdate

import (
	"bytes"
	"testing"

	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeCheckResult(ecosystem string, patch, minor, major int) *depcheck.CheckResult {
	cr := &depcheck.CheckResult{Ecosystem: ecosystem}
	for i := 0; i < patch; i++ {
		cr.Patch = append(cr.Patch, depcheck.ModuleUpdate{Path: "patch-dep", Kind: "patch"})
	}
	for i := 0; i < minor; i++ {
		cr.Minor = append(cr.Minor, depcheck.ModuleUpdate{Path: "minor-dep", Kind: "minor"})
	}
	for i := 0; i < major; i++ {
		cr.Major = append(cr.Major, depcheck.ModuleUpdate{Path: "major-dep", Kind: "major"})
	}
	return cr
}

func TestTotalUpdates_NoFilters(t *testing.T) {
	ar := AnvilResult{
		Anvil: "test",
		Ecosystems: []*depcheck.CheckResult{
			makeCheckResult("Go", 2, 3, 1),
		},
	}
	assert.Equal(t, 6, ar.TotalUpdates(Options{}))
}

func TestTotalUpdates_PatchOnly(t *testing.T) {
	ar := AnvilResult{
		Anvil: "test",
		Ecosystems: []*depcheck.CheckResult{
			makeCheckResult("Go", 2, 3, 1),
		},
	}
	assert.Equal(t, 2, ar.TotalUpdates(Options{PatchOnly: true}))
}

func TestTotalUpdates_NoMajor(t *testing.T) {
	ar := AnvilResult{
		Anvil: "test",
		Ecosystems: []*depcheck.CheckResult{
			makeCheckResult("Go", 2, 3, 1),
		},
	}
	assert.Equal(t, 5, ar.TotalUpdates(Options{NoMajor: true}))
}

func TestTotalUpdates_MultipleEcosystems(t *testing.T) {
	ar := AnvilResult{
		Anvil: "test",
		Ecosystems: []*depcheck.CheckResult{
			makeCheckResult("Go", 1, 0, 0),
			makeCheckResult("npm", 0, 2, 1),
		},
	}
	assert.Equal(t, 4, ar.TotalUpdates(Options{}))
	assert.Equal(t, 1, ar.TotalUpdates(Options{PatchOnly: true}))
	assert.Equal(t, 3, ar.TotalUpdates(Options{NoMajor: true}))
}

func TestTotalUpdates_Empty(t *testing.T) {
	ar := AnvilResult{Anvil: "test"}
	assert.Equal(t, 0, ar.TotalUpdates(Options{}))
}

func TestFilterUpdates(t *testing.T) {
	cr := makeCheckResult("Go", 2, 3, 1)

	all := filterUpdates(cr, Options{})
	assert.Len(t, all, 6)

	patchOnly := filterUpdates(cr, Options{PatchOnly: true})
	assert.Len(t, patchOnly, 2)

	noMajor := filterUpdates(cr, Options{NoMajor: true})
	assert.Len(t, noMajor, 5)
}

func TestPrintSummary_ShowsAnvilAndCounts(t *testing.T) {
	results := []AnvilResult{
		{
			Anvil: "myrepo",
			Path:  "/path/to/myrepo",
			Ecosystems: []*depcheck.CheckResult{
				{
					Ecosystem: "Go",
					Patch: []depcheck.ModuleUpdate{
						{Path: "github.com/foo/bar", Current: "v1.0.0", Latest: "v1.0.1", Kind: "patch"},
					},
					Minor: []depcheck.ModuleUpdate{
						{Path: "github.com/baz/qux", Current: "v1.0.0", Latest: "v1.1.0", Kind: "minor"},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	total := PrintSummary(&buf, results, Options{})
	assert.Equal(t, 2, total)
	assert.Contains(t, buf.String(), "Anvil: myrepo")
	assert.Contains(t, buf.String(), "Go (2 outdated)")
	assert.Contains(t, buf.String(), "github.com/foo/bar")
}

func TestPrintSummary_PatchOnlyFilters(t *testing.T) {
	results := []AnvilResult{
		{
			Anvil: "myrepo",
			Ecosystems: []*depcheck.CheckResult{
				{
					Ecosystem: "Go",
					Patch: []depcheck.ModuleUpdate{
						{Path: "patch-dep", Current: "v1.0.0", Latest: "v1.0.1", Kind: "patch"},
					},
					Minor: []depcheck.ModuleUpdate{
						{Path: "minor-dep", Current: "v1.0.0", Latest: "v1.1.0", Kind: "minor"},
					},
					Major: []depcheck.ModuleUpdate{
						{Path: "major-dep", Current: "v1.0.0", Latest: "v2.0.0", Kind: "major"},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	total := PrintSummary(&buf, results, Options{PatchOnly: true})
	assert.Equal(t, 1, total)
	assert.Contains(t, buf.String(), "patch-dep")
	assert.NotContains(t, buf.String(), "minor-dep")
	assert.NotContains(t, buf.String(), "major-dep")
}

func TestPrintSummary_EmptyEcosystem(t *testing.T) {
	results := []AnvilResult{
		{
			Anvil:      "empty-repo",
			Ecosystems: []*depcheck.CheckResult{{Ecosystem: "Go"}},
		},
	}

	var buf bytes.Buffer
	total := PrintSummary(&buf, results, Options{})
	assert.Equal(t, 0, total)
	assert.Contains(t, buf.String(), "all dependencies up to date")
}

func TestFormatSummaryLine(t *testing.T) {
	results := []AnvilResult{
		{
			Anvil:      "repo1",
			Ecosystems: []*depcheck.CheckResult{makeCheckResult("Go", 2, 1, 0)},
		},
		{
			Anvil:      "repo2",
			Ecosystems: []*depcheck.CheckResult{makeCheckResult("npm", 0, 0, 1)},
		},
	}

	line := FormatSummaryLine(results, Options{})
	assert.Contains(t, line, "4 outdated")
	assert.Contains(t, line, "2 anvil(s)")
}

func TestFormatSummaryLine_AllUpToDate(t *testing.T) {
	results := []AnvilResult{
		{
			Anvil:      "repo1",
			Ecosystems: []*depcheck.CheckResult{{Ecosystem: "Go"}},
		},
	}
	line := FormatSummaryLine(results, Options{})
	assert.Equal(t, "All dependencies up to date across all anvils.", line)
}

func TestFormatSummaryLine_PatchOnlyLabel(t *testing.T) {
	results := []AnvilResult{
		{
			Anvil:      "repo1",
			Ecosystems: []*depcheck.CheckResult{makeCheckResult("Go", 3, 0, 0)},
		},
	}
	line := FormatSummaryLine(results, Options{PatchOnly: true})
	assert.Contains(t, line, "(patch only)")
}

func TestFormatSummaryLine_NoMajorLabel(t *testing.T) {
	results := []AnvilResult{
		{
			Anvil:      "repo1",
			Ecosystems: []*depcheck.CheckResult{makeCheckResult("Go", 1, 1, 0)},
		},
	}
	line := FormatSummaryLine(results, Options{NoMajor: true})
	require.Contains(t, line, "(excluding major)")
}
