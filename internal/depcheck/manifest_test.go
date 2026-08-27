package depcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGoModRequires(t *testing.T) {
	mod := []byte(`module github.com/example/app

go 1.26

require github.com/single/dep v1.4.0

require (
	github.com/foo/bar v1.2.3
	github.com/baz/qux v0.3.0 // some note
	golang.org/x/sys v0.47.0 // indirect
)

require github.com/other/indirect v0.1.0 // indirect

replace github.com/foo/bar => ../bar

exclude github.com/bad/mod v6.6.6
`)

	got := parseGoModRequires(mod)
	assert.Equal(t, map[string]string{
		"github.com/single/dep": "v1.4.0",
		"github.com/foo/bar":    "v1.2.3",
		"github.com/baz/qux":    "v0.3.0",
	}, got, "only direct requires, with replace/exclude and indirect entries left out")
}

func TestParseGoModRequires_Empty(t *testing.T) {
	assert.Empty(t, parseGoModRequires([]byte("module x\n\ngo 1.26\n")))
}

func TestParsePackageRefs(t *testing.T) {
	csproj := []byte(`<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup>
    <PackageReference Include="Serilog" Version="3.1.1" />
    <PackageReference Include="Nested">
      <Version>2.0.0</Version>
    </PackageReference>
    <PackageReference Include="FromProperty" Version="$(SerilogVersion)" />
    <PackageReference Include="NoVersion" />
  </ItemGroup>
</Project>`)

	assert.Equal(t, map[string]string{
		"Serilog": "3.1.1",
		"Nested":  "2.0.0",
	}, parsePackageRefs(csproj), "property references and version-less entries are not pins")
}

func TestParsePackageRefs_CentralPackageManagement(t *testing.T) {
	props := []byte(`<Project>
  <ItemGroup>
    <PackageVersion Include="Newtonsoft.Json" Version="13.0.3" />
    <PackageVersion Update="Serilog" Version="4.0.0" />
  </ItemGroup>
</Project>`)

	assert.Equal(t, map[string]string{
		"Newtonsoft.Json": "13.0.3",
		"Serilog":         "4.0.0",
	}, parsePackageRefs(props))
}

func TestParsePackageJSONDeps(t *testing.T) {
	pkg := []byte(`{
  "name": "app",
  "dependencies": {"lodash": "^4.17.21"},
  "devDependencies": {"vitest": "~1.2.0"},
  "optionalDependencies": {"fsevents": "2.3.3"}
}`)

	assert.Equal(t, map[string]string{
		"lodash":   "^4.17.21",
		"vitest":   "~1.2.0",
		"fsevents": "2.3.3",
	}, parsePackageJSONDeps(pkg))
}

func TestParsePackageJSONDeps_Malformed(t *testing.T) {
	assert.Nil(t, parsePackageJSONDeps([]byte("not json")))
}

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.3":           "1.2.3",
		"v1.2.3":          "1.2.3",
		"^1.2.3":          "1.2.3",
		"~1.2.3":          "1.2.3",
		">=1.2.3 <2.0.0":  "1.2.3",
		"  v1.2.3  ":      "1.2.3",
		"v0.0.0-2026-abc": "0.0.0-2026-abc",
		"":                "",
		// A bare strict inequality names the version its range EXCLUDES, so it
		// pins nothing — reducing it to that version made a manifest that still
		// needs bumping read as one already bumped.
		"<2.0.0": "",
		">1.2.3": "",
		// The non-strict forms do admit the version they name.
		"<=1.2.3": "1.2.3",
		">=1.2.3": "1.2.3",
	} {
		assert.Equal(t, want, normalizeVersion(in), "normalizeVersion(%q)", in)
	}
}

// TestReconcileWithCommitted_KeepsAnUpdateABareUpperBoundExcludes is that
// reduction at the level it mattered: "<2.0.0" excludes 2.0.0, so the manifest
// genuinely needs bumping and the update must not vanish.
func TestReconcileWithCommitted_KeepsAnUpdateABareUpperBoundExcludes(t *testing.T) {
	updates := []ModuleUpdate{
		{Path: "left-pad", Current: "1.0.0", Latest: "2.0.0", Kind: "major"},
	}
	got := reconcileWithCommitted(updates, map[string]string{"left-pad": "<2.0.0"}, false)
	require.Len(t, got, 1, "the range excludes the latest version, so this update is still outstanding")
	assert.Equal(t, "1.0.0", got[0].Current)
}

// TestParsePackageRefs_RangesAndFloatsAreNotPins covers NuGet's version syntax
// beyond the bare form. A bracketed single version IS an exact pin and is read
// as one; an interval or a float names no version and is recorded as nothing,
// so reconciliation passes such a package through untouched.
func TestParsePackageRefs_RangesAndFloatsAreNotPins(t *testing.T) {
	csproj := []byte(`<Project>
  <ItemGroup>
    <PackageReference Include="Exact" Version="[1.2.3]" />
    <PackageReference Include="Interval" Version="[1.0.0,2.0.0)" />
    <PackageReference Include="ClosedInterval" Version="[1.0.0,2.0.0]" />
    <PackageReference Include="MinOnly" Version="(1.0.0,)" />
    <PackageReference Include="Float" Version="1.0.*" />
    <PackageReference Include="Bare" Version="3.1.1" />
  </ItemGroup>
</Project>`)

	assert.Equal(t, map[string]string{
		"Exact": "1.2.3",
		"Bare":  "3.1.1",
	}, parsePackageRefs(csproj))
}

// TestReconcileWithCommitted_BracketPinnedNuGetPackage is what the two
// consequences of recording a range verbatim looked like. Unreduced, "[1.2.3]"
// was written into Current and classifyUpdate re-derived the kind from "[1",
// reporting a patch bump as `major` (which routes it to needs-attention rather
// than auto-dispatch); and "[1.2.4]" never equalled "1.2.4", so an update
// upstream had ALREADY merged survived the drop and was emitted as a nonsense
// no-op from [1.2.4] to 1.2.4 — the duplicate bead this reconciliation exists
// to prevent.
func TestReconcileWithCommitted_BracketPinnedNuGetPackage(t *testing.T) {
	updates := []ModuleUpdate{
		{Path: "Serilog", Current: "1.2.3", Latest: "1.2.4", Kind: "patch"},
	}

	got := reconcileWithCommitted(updates, parsePackageRefs(
		[]byte(`<Project><ItemGroup><PackageReference Include="Serilog" Version="[1.2.3]" /></ItemGroup></Project>`)), true)
	require.Len(t, got, 1)
	assert.Equal(t, "1.2.3", got[0].Current)
	assert.Equal(t, "patch", got[0].Kind, "a bracket pin must not turn a patch bump into a major one")

	assert.Empty(t, reconcileWithCommitted(updates, parsePackageRefs(
		[]byte(`<Project><ItemGroup><PackageReference Include="Serilog" Version="[1.2.4]" /></ItemGroup></Project>`)), true),
		"upstream already pins the latest — filing a bead for it duplicates merged work")
}

// TestReconcileWithCommitted_FloatingNuGetPackageIsPassedThrough is the safe
// direction for a version that names no single version: nothing is recorded, so
// the reported update is neither rewritten nor dropped.
func TestReconcileWithCommitted_FloatingNuGetPackageIsPassedThrough(t *testing.T) {
	updates := []ModuleUpdate{
		{Path: "Serilog", Current: "1.2.3", Latest: "1.2.4", Kind: "patch"},
	}
	got := reconcileWithCommitted(updates, parsePackageRefs(
		[]byte(`<Project><ItemGroup><PackageReference Include="Serilog" Version="1.0.*" /></ItemGroup></Project>`)), true)
	require.Len(t, got, 1)
	assert.Equal(t, "1.2.3", got[0].Current)
	assert.Equal(t, "patch", got[0].Kind)
}

func TestReconcileWithCommitted_DropsUpdatesUpstreamAlreadyApplied(t *testing.T) {
	updates := []ModuleUpdate{
		{Path: "github.com/foo/bar", Current: "v1.2.3", Latest: "v1.3.0", Kind: "minor"},
		{Path: "github.com/keep/me", Current: "v0.1.0", Latest: "v0.2.0", Kind: "minor"},
	}
	committed := map[string]string{
		"github.com/foo/bar": "v1.3.0", // upstream has already merged this bump
		"github.com/keep/me": "v0.1.0",
	}

	got := reconcileWithCommitted(updates, committed, true)
	if assert.Len(t, got, 1, "an update the tracking ref already pins at latest is not outstanding work") {
		assert.Equal(t, "github.com/keep/me", got[0].Path)
	}
}

func TestReconcileWithCommitted_RewritesStaleCurrentWhenExact(t *testing.T) {
	updates := []ModuleUpdate{
		// The checkout is behind: it still has v1.0.0 while upstream pins v1.9.0.
		{Path: "github.com/foo/bar", Current: "v1.0.0", Latest: "v2.0.0", Kind: "major"},
	}
	committed := map[string]string{"github.com/foo/bar": "v1.9.0"}

	got := reconcileWithCommitted(updates, committed, true)
	if assert.Len(t, got, 1) {
		assert.Equal(t, "v1.9.0", got[0].Current, "the committed version is the one a bead should quote")
		assert.Equal(t, "major", got[0].Kind)
	}
}

func TestReconcileWithCommitted_LeavesRangesAlone(t *testing.T) {
	updates := []ModuleUpdate{
		{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
	}
	// A package.json range says nothing about which version is installed.
	got := reconcileWithCommitted(updates, map[string]string{"lodash": "^4.0.0"}, false)
	if assert.Len(t, got, 1) {
		assert.Equal(t, "4.17.20", got[0].Current)
	}
}

func TestReconcileWithCommitted_DropsRangeAlreadyAtLatest(t *testing.T) {
	updates := []ModuleUpdate{
		{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
	}
	assert.Empty(t, reconcileWithCommitted(updates, map[string]string{"lodash": "^4.17.21"}, false))
}

func TestReconcileWithCommitted_PassesThroughUnknownPackages(t *testing.T) {
	updates := []ModuleUpdate{
		{Path: "github.com/foo/bar", Current: "v1.2.3", Latest: "v1.3.0", Kind: "minor"},
	}
	// Absence means the committed state could not be established — dropping on
	// that would silently shrink the scan.
	assert.Len(t, reconcileWithCommitted(updates, map[string]string{"other": "v9"}, true), 1)
	assert.Len(t, reconcileWithCommitted(updates, nil, true), 1)
}
