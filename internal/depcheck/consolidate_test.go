package depcheck

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsolidatedBeadTitle(t *testing.T) {
	d := time.Date(2026, 3, 27, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, "Package updates starting 27.03.2026", consolidatedBeadTitle(d))
}

func TestConsolidatedBeadTitle_LeadingZero(t *testing.T) {
	d := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, "Package updates starting 05.01.2026", consolidatedBeadTitle(d))
}

func TestEcoKey(t *testing.T) {
	assert.Equal(t, "go", ecoKey("Go"))
	assert.Equal(t, "npm", ecoKey("npm"))
	assert.Equal(t, "nuget", ecoKey("NuGet"))
	assert.Equal(t, "nuget", ecoKey(".NET"))
	assert.Equal(t, "gradle", ecoKey("Gradle"))
}

func TestFormatPkgEntries(t *testing.T) {
	updates := []ModuleUpdate{
		{Path: "react", Current: "18.2.0", Latest: "19.0.0"},
		{Path: "express", Current: "4.0.0", Latest: "5.0.0"},
	}
	got := formatPkgEntries(updates)
	assert.Equal(t, "react 18.2.0→19.0.0, express 4.0.0→5.0.0", got)
}

func TestBuildConsolidatedDescription_NpmAndNuget(t *testing.T) {
	results := []*CheckResult{
		{
			Ecosystem: "npm",
			Anvil:     "myanvil",
			Patch:     []ModuleUpdate{{Path: "express", Current: "4.18.0", Latest: "4.19.0", Kind: "patch"}},
			Minor:     []ModuleUpdate{{Path: "react", Current: "18.2.0", Latest: "18.3.0", Kind: "minor"}},
		},
		{
			Ecosystem: "NuGet",
			Anvil:     "myavil",
			Patch:     []ModuleUpdate{{Path: "Newtonsoft.Json", Current: "12.0.3", Latest: "12.0.4", Kind: "patch"}},
		},
	}
	desc := buildConsolidatedDescription("myanvil", results)

	assert.Contains(t, desc, "Automated dependency updates for myanvil:")
	assert.Contains(t, desc, "npm: ")
	assert.Contains(t, desc, "nuget: Newtonsoft.Json 12.0.3→12.0.4")
	assert.Contains(t, desc, "react 18.2.0→18.3.0")
	assert.Contains(t, desc, "express 4.18.0→4.19.0")
	assert.NotContains(t, desc, "Major updates")
}

func TestBuildConsolidatedDescription_MixedWithMajor(t *testing.T) {
	results := []*CheckResult{
		{
			Ecosystem: "npm",
			Minor:     []ModuleUpdate{{Path: "lodash", Current: "3.10.1", Latest: "3.10.2", Kind: "minor"}},
			Major:     []ModuleUpdate{{Path: "webpack", Current: "4.0.0", Latest: "5.0.0", Kind: "major"}},
		},
	}
	desc := buildConsolidatedDescription("testrepo", results)

	assert.Contains(t, desc, "npm: lodash 3.10.1→3.10.2")
	assert.Contains(t, desc, "Major updates (require manual review):")
	assert.Contains(t, desc, "npm: webpack 4.0.0→5.0.0")
}

func TestBuildConsolidatedDescription_GoNpmNuget(t *testing.T) {
	// All three ecosystems land in the same bead, in go/npm/nuget order.
	results := []*CheckResult{
		{Ecosystem: "NuGet", Patch: []ModuleUpdate{{Path: "Foo", Current: "1.0", Latest: "1.1", Kind: "patch"}}},
		{Ecosystem: "npm", Patch: []ModuleUpdate{{Path: "bar", Current: "2.0.0", Latest: "2.0.1", Kind: "patch"}}},
		{Ecosystem: "Go", Patch: []ModuleUpdate{{Path: "golang.org/x/net", Current: "v0.1.0", Latest: "v0.2.0", Kind: "patch"}}},
	}
	desc := buildConsolidatedDescription("myrepo", results)

	goIdx := strings.Index(desc, "go:")
	npmIdx := strings.Index(desc, "npm:")
	nugetIdx := strings.Index(desc, "nuget:")

	require.True(t, goIdx >= 0, "go section missing")
	require.True(t, npmIdx >= 0, "npm section missing")
	require.True(t, nugetIdx >= 0, "nuget section missing")

	// go before npm before nuget.
	assert.Less(t, goIdx, npmIdx)
	assert.Less(t, npmIdx, nugetIdx)
}

func TestParsePackageEntry(t *testing.T) {
	tests := []struct {
		input   string
		wantPkg string
		wantCur string
		wantLat string
	}{
		{"react 18.2.0→19.0.0", "react", "18.2.0", "19.0.0"},
		{"golang.org/x/net v0.1.0→v0.2.0", "golang.org/x/net", "v0.1.0", "v0.2.0"},
		{"Newtonsoft.Json 12.0.3→12.0.4", "Newtonsoft.Json", "12.0.3", "12.0.4"},
		{"  lodash 4.17.20→4.17.21  ", "lodash", "4.17.20", "4.17.21"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parsePackageEntry(tt.input)
			require.NotNil(t, got)
			assert.Equal(t, tt.wantPkg, got.Path)
			assert.Equal(t, tt.wantCur, got.Current)
			assert.Equal(t, tt.wantLat, got.Latest)
		})
	}
}

func TestParsePackageEntry_Invalid(t *testing.T) {
	assert.Nil(t, parsePackageEntry(""))
	assert.Nil(t, parsePackageEntry("no-arrow-here"))
	assert.Nil(t, parsePackageEntry("→only-latest"))
}

func TestParseConsolidatedDescription_RoundTrip(t *testing.T) {
	results := []*CheckResult{
		{
			Ecosystem: "npm",
			Patch:     []ModuleUpdate{{Path: "react", Current: "18.0.0", Latest: "18.1.0", Kind: "patch"}},
			Major:     []ModuleUpdate{{Path: "webpack", Current: "4.0.0", Latest: "5.0.0", Kind: "major"}},
		},
		{
			Ecosystem: "NuGet",
			Patch:     []ModuleUpdate{{Path: "Foo.Bar", Current: "2.0.0", Latest: "2.1.0", Kind: "minor"}},
		},
	}
	desc := buildConsolidatedDescription("myrepo", results)

	auto, major := parseConsolidatedDescription(desc)

	require.Contains(t, auto, "npm")
	require.Contains(t, auto["npm"], "react")
	assert.Equal(t, "18.0.0", auto["npm"]["react"].Current)
	assert.Equal(t, "18.1.0", auto["npm"]["react"].Latest)

	require.Contains(t, auto, "nuget")
	require.Contains(t, auto["nuget"], "Foo.Bar")

	require.Contains(t, major, "npm")
	require.Contains(t, major["npm"], "webpack")
	assert.Equal(t, "4.0.0", major["npm"]["webpack"].Current)
}

func TestMergeConsolidatedPackages_NewPackageAdded(t *testing.T) {
	existingAuto := map[string]map[string]ModuleUpdate{
		"npm": {"react": {Path: "react", Current: "18.0.0", Latest: "18.1.0", Kind: "patch"}},
	}
	existingMajor := map[string]map[string]ModuleUpdate{}

	newResults := []*CheckResult{
		{
			Ecosystem: "npm",
			Patch:     []ModuleUpdate{{Path: "express", Current: "4.18.0", Latest: "4.19.0", Kind: "patch"}},
		},
	}
	mergedAuto, mergedMajor := mergeConsolidatedPackages(existingAuto, existingMajor, newResults)

	assert.Contains(t, mergedAuto["npm"], "react")
	assert.Contains(t, mergedAuto["npm"], "express")
	assert.Empty(t, mergedMajor)
}

func TestMergeConsolidatedPackages_NewVersionWins(t *testing.T) {
	existingAuto := map[string]map[string]ModuleUpdate{
		"npm": {"react": {Path: "react", Current: "18.0.0", Latest: "18.1.0", Kind: "patch"}},
	}
	existingMajor := map[string]map[string]ModuleUpdate{}

	// Same package but with a newer target version.
	newResults := []*CheckResult{
		{
			Ecosystem: "npm",
			Minor:     []ModuleUpdate{{Path: "react", Current: "18.0.0", Latest: "18.3.0", Kind: "minor"}},
		},
	}
	mergedAuto, _ := mergeConsolidatedPackages(existingAuto, existingMajor, newResults)

	assert.Equal(t, "18.3.0", mergedAuto["npm"]["react"].Latest)
}

func TestMergeConsolidatedPackages_NpmAndNugetSameBead(t *testing.T) {
	existingAuto := map[string]map[string]ModuleUpdate{
		"npm": {"react": {Path: "react", Current: "18.0.0", Latest: "18.1.0"}},
	}
	existingMajor := map[string]map[string]ModuleUpdate{}

	newResults := []*CheckResult{
		{
			Ecosystem: "NuGet",
			Patch:     []ModuleUpdate{{Path: "Newtonsoft.Json", Current: "12.0.3", Latest: "12.0.4", Kind: "patch"}},
		},
	}
	mergedAuto, _ := mergeConsolidatedPackages(existingAuto, existingMajor, newResults)

	assert.Contains(t, mergedAuto, "npm")
	assert.Contains(t, mergedAuto, "nuget")
	assert.Contains(t, mergedAuto["nuget"], "Newtonsoft.Json")
}

func TestBuildDescriptionFromMaps_EmptyAutoKeepsMajor(t *testing.T) {
	autoByEco := map[string]map[string]ModuleUpdate{}
	majorByEco := map[string]map[string]ModuleUpdate{
		"npm": {"webpack": {Path: "webpack", Current: "4.0.0", Latest: "5.0.0", Kind: "major"}},
	}
	desc := buildDescriptionFromMaps("myrepo", autoByEco, majorByEco)

	assert.Contains(t, desc, "Major updates (require manual review):")
	assert.Contains(t, desc, "npm: webpack 4.0.0→5.0.0")
}
