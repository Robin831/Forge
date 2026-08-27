package depcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	} {
		assert.Equal(t, want, normalizeVersion(in), "normalizeVersion(%q)", in)
	}
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
