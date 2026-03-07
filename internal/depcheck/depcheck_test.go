package depcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGoListOutput(t *testing.T) {
	input := `github.com/Robin831/Forge
github.com/foo/bar v1.2.3 [v1.2.5]
github.com/baz/qux v0.3.0 [v0.4.1]
github.com/big/lib v2.0.0 [v3.0.0]
github.com/up/to/date v1.0.0
github.com/another/patch v0.1.1 [v0.1.2]
`

	updates := parseGoListOutput(input)
	require.Len(t, updates, 4)

	// All should be Go ecosystem
	for _, u := range updates {
		assert.Equal(t, EcosystemGo, u.Ecosystem)
	}

	// parseGoListOutput doesn't sort — sort is done in checkAnvil.
	// But we know the order from the input: foo/bar, baz/qux, big/lib, another/patch
	byPkg := make(map[string]DependencyUpdate)
	for _, u := range updates {
		byPkg[u.Package] = u
	}

	assert.Equal(t, "major", byPkg["github.com/big/lib"].Kind)
	assert.Equal(t, "v2.0.0", byPkg["github.com/big/lib"].Current)
	assert.Equal(t, "v3.0.0", byPkg["github.com/big/lib"].Latest)

	assert.Equal(t, "minor", byPkg["github.com/baz/qux"].Kind)
	assert.Equal(t, "patch", byPkg["github.com/another/patch"].Kind)
	assert.Equal(t, "patch", byPkg["github.com/foo/bar"].Kind)
}

func TestParseGoListOutput_Empty(t *testing.T) {
	input := `github.com/Robin831/Forge
github.com/foo/bar v1.0.0
`
	updates := parseGoListOutput(input)
	assert.Empty(t, updates)
}

func TestParseGoListOutput_PseudoVersions(t *testing.T) {
	input := `github.com/Robin831/Forge
github.com/foo/bar v0.0.0-20230101-abc1234 [v0.1.0]
`
	updates := parseGoListOutput(input)
	require.Len(t, updates, 1)
	assert.Equal(t, "minor", updates[0].Kind)
	assert.Equal(t, "v0.0.0-20230101-abc1234", updates[0].Current)
	assert.Equal(t, "v0.1.0", updates[0].Latest)
}

func TestClassifyUpdate(t *testing.T) {
	tests := []struct {
		current  string
		latest   string
		expected string
	}{
		{"v1.2.3", "v1.2.5", "patch"},
		{"v1.2.3", "v1.3.0", "minor"},
		{"v1.2.3", "v2.0.0", "major"},
		{"v0.1.0", "v0.2.0", "minor"},
		{"v0.0.1", "v0.0.2", "patch"},
		{"v1.0.0", "v1.0.0-rc1", "patch"},
		// npm-style versions (no v prefix)
		{"1.2.3", "1.2.5", "patch"},
		{"1.2.3", "2.0.0", "major"},
	}

	for _, tt := range tests {
		t.Run(tt.current+"->"+tt.latest, func(t *testing.T) {
			result := classifyUpdate(tt.current, tt.latest)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input               string
		major, minor, patch string
	}{
		{"v1.2.3", "1", "2", "3"},
		{"v0.0.0", "0", "0", "0"},
		{"v2.1.0-pre.1", "2", "1", "0"},
		{"v0.0.0-20230101120000-abc1234def56", "0", "0", "0"},
		{"1.2.3", "1", "2", "3"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			maj, min, pat := parseSemver(tt.input)
			assert.Equal(t, tt.major, maj)
			assert.Equal(t, tt.minor, min)
			assert.Equal(t, tt.patch, pat)
		})
	}
}

func TestParseDotnetOutput(t *testing.T) {
	data := []byte(`{
  "version": 1,
  "parameters": "",
  "projects": [
    {
      "path": "MyApp.csproj",
      "frameworks": [
        {
          "framework": "net8.0",
          "topLevelPackages": [
            {
              "id": "Newtonsoft.Json",
              "requestedVersion": "13.0.1",
              "resolvedVersion": "13.0.1",
              "latestVersion": "13.0.3"
            },
            {
              "id": "Serilog",
              "requestedVersion": "3.0.0",
              "resolvedVersion": "3.0.0",
              "latestVersion": "4.0.0"
            },
            {
              "id": "UpToDate.Pkg",
              "requestedVersion": "1.0.0",
              "resolvedVersion": "1.0.0",
              "latestVersion": "1.0.0"
            }
          ]
        }
      ]
    }
  ]
}`)

	updates, err := parseDotnetOutput(data, "api")
	require.NoError(t, err)
	require.Len(t, updates, 2)

	// Find by package name since map iteration order varies
	var newtonsoft, serilog DependencyUpdate
	for _, u := range updates {
		switch u.Package {
		case "Newtonsoft.Json":
			newtonsoft = u
		case "Serilog":
			serilog = u
		}
	}

	assert.Equal(t, EcosystemDotnet, newtonsoft.Ecosystem)
	assert.Equal(t, "13.0.1", newtonsoft.Current)
	assert.Equal(t, "13.0.3", newtonsoft.Latest)
	assert.Equal(t, "patch", newtonsoft.Kind)
	assert.Equal(t, "api", newtonsoft.Subdir)

	assert.Equal(t, EcosystemDotnet, serilog.Ecosystem)
	assert.Equal(t, "major", serilog.Kind)
}

func TestParseDotnetOutput_DedupAcrossFrameworks(t *testing.T) {
	data := []byte(`{
  "version": 1,
  "parameters": "",
  "projects": [
    {
      "path": "MyApp.csproj",
      "frameworks": [
        {
          "framework": "net8.0",
          "topLevelPackages": [
            {"id": "Foo", "requestedVersion": "1.0.0", "resolvedVersion": "1.0.0", "latestVersion": "1.1.0"}
          ]
        },
        {
          "framework": "net7.0",
          "topLevelPackages": [
            {"id": "Foo", "requestedVersion": "1.0.0", "resolvedVersion": "1.0.0", "latestVersion": "1.1.0"}
          ]
        }
      ]
    }
  ]
}`)

	updates, err := parseDotnetOutput(data, "")
	require.NoError(t, err)
	require.Len(t, updates, 1, "should deduplicate across target frameworks")
}

func TestParseNpmOutput(t *testing.T) {
	data := []byte(`{
  "express": {
    "current": "4.18.2",
    "wanted": "4.18.3",
    "latest": "4.18.3",
    "location": "node_modules/express"
  },
  "typescript": {
    "current": "4.9.5",
    "wanted": "4.9.5",
    "latest": "5.3.3",
    "location": "node_modules/typescript"
  },
  "lodash": {
    "current": "4.17.20",
    "wanted": "4.17.21",
    "latest": "4.17.21",
    "location": "node_modules/lodash"
  }
}`)

	updates, err := parseNpmOutput(data, "client")
	require.NoError(t, err)
	require.Len(t, updates, 3)

	byPkg := make(map[string]DependencyUpdate)
	for _, u := range updates {
		byPkg[u.Package] = u
	}

	assert.Equal(t, EcosystemNpm, byPkg["express"].Ecosystem)
	assert.Equal(t, "patch", byPkg["express"].Kind)
	assert.Equal(t, "client", byPkg["express"].Subdir)

	assert.Equal(t, "major", byPkg["typescript"].Kind)

	assert.Equal(t, "patch", byPkg["lodash"].Kind)
}

func TestParseNpmOutput_Empty(t *testing.T) {
	updates, err := parseNpmOutput([]byte("{}"), "")
	require.NoError(t, err)
	assert.Empty(t, updates)

	updates, err = parseNpmOutput([]byte(""), "")
	require.NoError(t, err)
	assert.Empty(t, updates)
}

func TestScanResultUpdatesByEcosystem(t *testing.T) {
	result := ScanResult{
		Updates: []DependencyUpdate{
			{Ecosystem: EcosystemGo, Package: "go-pkg", Kind: "patch"},
			{Ecosystem: EcosystemNpm, Package: "npm-pkg", Kind: "minor"},
			{Ecosystem: EcosystemGo, Package: "go-pkg2", Kind: "major"},
			{Ecosystem: EcosystemDotnet, Package: "dotnet-pkg", Kind: "patch"},
		},
	}

	byEco := result.UpdatesByEcosystem()
	assert.Len(t, byEco[EcosystemGo], 2)
	assert.Len(t, byEco[EcosystemNpm], 1)
	assert.Len(t, byEco[EcosystemDotnet], 1)
}

func TestScanResultUpdatesByKind(t *testing.T) {
	result := ScanResult{
		Updates: []DependencyUpdate{
			{Package: "a", Kind: "patch"},
			{Package: "b", Kind: "minor"},
			{Package: "c", Kind: "major"},
			{Package: "d", Kind: "patch"},
		},
	}

	patchMinor, major := result.UpdatesByKind()
	assert.Len(t, patchMinor, 3)
	assert.Len(t, major, 1)
	assert.Equal(t, "c", major[0].Package)
}
