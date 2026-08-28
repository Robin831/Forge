package temper

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeProject writes a project file, creating its directory. The content is
// a bare SDK project unless markers are supplied.
func writeProject(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

const libraryProject = `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net8.0</TargetFramework></PropertyGroup></Project>`

const testProject = `<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup>
    <PackageReference Include="Microsoft.NET.Test.Sdk" Version="17.11.1" />
    <PackageReference Include="xunit" Version="2.9.0" />
  </ItemGroup>
</Project>`

// TestDetectSteps_DotnetProjectDeeperThanOneLevel is the bead's case: the old
// detection asked filepath.Glob for `**/*.csproj`, which matches exactly one
// directory level, so a project under src/Api was never found and the whole
// repository fell through to the "No build system detected" echo step.
func TestDetectSteps_DotnetProjectDeeperThanOneLevel(t *testing.T) {
	dir := t.TempDir()
	writeProject(t, dir, "src/Api/Api.csproj", libraryProject)

	steps := detectSteps(dir, nil, false)
	names := stepNames(steps)

	require.NotContains(t, names, "echo", "a .NET repo must not fall through to the no-build-system step")
	require.Contains(t, names, "src/Api/Api:build")

	// The target has to be named: `dotnet build` with no argument resolves a
	// project in its working directory and fails with MSB1003 when the root
	// holds none.
	for _, s := range steps {
		if s.Name == "src/Api/Api:build" {
			assert.Equal(t, []string{"build", "--no-restore", filepath.FromSlash("src/Api/Api.csproj")}, s.Args)
			assert.Equal(t, defaultDotnetPaths, s.Paths)
		}
	}
}

// TestDetectSteps_DotnetDeepSolutionIsTheEntryPoint: a solution covers every
// project it references, so it wins over the individual projects.
func TestDetectSteps_DotnetDeepSolutionIsTheEntryPoint(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "App.sln"), []byte(""), 0o644))
	writeProject(t, dir, "src/Api/Api.csproj", libraryProject)
	writeProject(t, dir, "src/Api.Tests/Api.Tests.csproj", testProject)

	steps := detectSteps(dir, nil, false)
	require.ElementsMatch(t, []string{"build", "test"}, stepNames(steps))

	for _, s := range steps {
		assert.Equal(t, []string{s.Args[0], s.Args[1], filepath.FromSlash("src/App.sln")}, s.Args,
			"step %q must name the solution", s.Name)
	}
}

// TestDetectSteps_DotnetRootSolutionKeepsBareCommands pins the unchanged
// behaviour of an ordinary single-solution repository: `dotnet` resolves a
// root target itself, so no path is appended.
func TestDetectSteps_DotnetRootSolutionKeepsBareCommands(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "App.sln"), []byte(""), 0o644))
	writeProject(t, dir, "src/Api/Api.csproj", libraryProject)

	steps := detectSteps(dir, nil, false)
	require.ElementsMatch(t, []string{"build", "test"}, stepNames(steps))

	for _, s := range steps {
		switch s.Name {
		case "build":
			assert.Equal(t, []string{"build", "--no-restore"}, s.Args)
		case "test":
			assert.Equal(t, []string{"test", "--no-build"}, s.Args)
		}
	}
}

// TestDetectSteps_DotnetTestsOnlyTestProjects: `dotnet test` against a project
// with no test framework exits non-zero, so a library is built and not tested.
func TestDetectSteps_DotnetTestsOnlyTestProjects(t *testing.T) {
	dir := t.TempDir()
	writeProject(t, dir, "src/Api/Api.csproj", libraryProject)
	writeProject(t, dir, "tests/Api.Tests/Api.Tests.csproj", testProject)

	names := stepNames(detectSteps(dir, nil, false))

	assert.Contains(t, names, "src/Api/Api:build")
	assert.Contains(t, names, "tests/Api.Tests/Api.Tests:build")
	assert.Contains(t, names, "tests/Api.Tests/Api.Tests:test")
	assert.NotContains(t, names, "src/Api/Api:test",
		"a library project must not be handed to dotnet test")
}

// TestDetectSteps_DotnetIgnoresBuildOutput: bin/obj hold copies of project
// files, and a project found there names a build nobody asked for.
func TestDetectSteps_DotnetIgnoresBuildOutput(t *testing.T) {
	dir := t.TempDir()
	writeProject(t, dir, "src/Api/obj/Api.csproj", libraryProject)
	writeProject(t, dir, "src/Api/bin/Debug/Api.csproj", libraryProject)
	writeProject(t, dir, "client/node_modules/pkg/Weird.csproj", libraryProject)

	layout := scanDotnet(dir)
	assert.Empty(t, layout.projects)
	assert.Equal(t, []string{"echo"}, stepNames(detectSteps(dir, nil, false)),
		"build output alone is not a build system")
}

// TestDetectSteps_DotnetFsprojDetected: the default path globs already gate
// the .NET steps on .fsproj/.vbproj, so detection has to agree — otherwise a
// diff that matches the gate can never trigger a step.
func TestDetectSteps_DotnetFsprojDetected(t *testing.T) {
	dir := t.TempDir()
	writeProject(t, dir, "src/Calc/Calc.fsproj", libraryProject)

	assert.Contains(t, stepNames(detectSteps(dir, nil, false)), "src/Calc/Calc:build")
}

func TestScanDotnet_BoundedDepth(t *testing.T) {
	dir := t.TempDir()
	deep := "a/b/c/d/e/f/g/h/i/Deep.csproj"
	writeProject(t, dir, deep, libraryProject)

	assert.Empty(t, scanDotnet(dir).projects,
		"the scan stops at dotnetScanMaxDepth rather than walking whatever is checked out")
}

func TestScanDotnet_ShallowestSolutionFirst(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"deep/nested/Z.sln", "top/A.sln", "top/B.sln"} {
		writeProject(t, dir, rel, "")
	}

	layout := scanDotnet(dir)
	require.Len(t, layout.solutions, 3)
	assert.Equal(t, "top/A.sln", layout.solutions[0])
}
