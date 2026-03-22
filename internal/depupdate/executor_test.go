package depupdate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/depcheck"
)

func TestInstallNpmGroup_EmptyGroup(t *testing.T) {
	group := UpdateGroup{Name: "empty", Updates: nil, Kind: "patch"}
	if err := InstallNpmGroup(t.Context(), t.TempDir(), group); err != nil {
		t.Fatalf("expected nil error for empty group, got %v", err)
	}
}

func TestInstallNpmGroup_EmptyGroupWithSourceDir(t *testing.T) {
	// Verify that SourceDir does not cause issues for empty groups (early return).
	group := UpdateGroup{Name: "empty", Updates: nil, Kind: "patch", SourceDir: t.TempDir()}
	if err := InstallNpmGroup(t.Context(), t.TempDir(), group); err != nil {
		t.Fatalf("expected nil error for empty group with SourceDir, got %v", err)
	}
}

func TestInstallGoGroup_EmptyGroup(t *testing.T) {
	group := UpdateGroup{Name: "empty", Updates: nil, Kind: "patch"}
	if err := InstallGoGroup(t.Context(), t.TempDir(), group); err != nil {
		t.Fatalf("expected nil error for empty group, got %v", err)
	}
}

func TestInstallDotnetGroup_EmptyGroup(t *testing.T) {
	group := UpdateGroup{Name: "empty", Updates: nil, Kind: "patch"}
	if err := InstallDotnetGroup(t.Context(), t.TempDir(), group); err != nil {
		t.Fatalf("expected nil error for empty group, got %v", err)
	}
}

func TestInstallDotnetGroup_NoCsproj(t *testing.T) {
	group := UpdateGroup{
		Name: "test",
		Updates: []depcheck.ModuleUpdate{
			{Path: "Newtonsoft.Json", Current: "13.0.1", Latest: "13.0.3", Kind: "patch"},
		},
		Kind: "patch",
	}
	err := InstallDotnetGroup(t.Context(), t.TempDir(), group)
	if err == nil {
		t.Fatal("expected error for missing csproj, got nil")
	}
}

func TestCommitGroup_MessageFormat(t *testing.T) {
	group := UpdateGroup{
		Name: "lodash",
		Kind: "minor",
		Updates: []depcheck.ModuleUpdate{
			{Path: "lodash", Current: "4.17.20", Latest: "4.18.0", Kind: "minor"},
		},
	}

	expected := "chore(deps): update lodash (minor)"
	got := fmt.Sprintf("chore(deps): update %s (%s)", group.Name, group.Kind)
	if got != expected {
		t.Errorf("commit message = %q, want %q", got, expected)
	}

	for _, u := range group.Updates {
		line := fmt.Sprintf("- %s: %s → %s", u.Path, u.Current, u.Latest)
		if line == "" {
			t.Error("expected non-empty body line")
		}
	}
}

func TestFindCsprojForPackage_SingleFile(t *testing.T) {
	files := []string{"/project/App.csproj"}
	got, err := findCsprojForPackage(files, "Newtonsoft.Json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != files[0] {
		t.Errorf("got %q, want %q", got, files[0])
	}
}

func TestFindCsprojForPackage_MultipleFiles_MatchesPackageReference(t *testing.T) {
	dir := t.TempDir()

	// First csproj: has a PackageReference for Serilog
	csproj1 := filepath.Join(dir, "Logging.csproj")
	if err := os.WriteFile(csproj1, []byte(`<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup>
    <PackageReference Include="Serilog" Version="3.0.0" />
  </ItemGroup>
</Project>`), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", csproj1, err)
	}

	// Second csproj: has a PackageReference for Newtonsoft.Json
	csproj2 := filepath.Join(dir, "Api.csproj")
	if err := os.WriteFile(csproj2, []byte(`<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup>
    <PackageReference Include="Newtonsoft.Json" Version="13.0.1" />
  </ItemGroup>
</Project>`), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", csproj2, err)
	}

	got, err := findCsprojForPackage([]string{csproj1, csproj2}, "Newtonsoft.Json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != csproj2 {
		t.Errorf("expected %q, got %q", csproj2, got)
	}
}

func TestFindCsprojForPackage_MultipleFiles_FallbackWhenNotFound(t *testing.T) {
	dir := t.TempDir()

	csproj1 := filepath.Join(dir, "App.csproj")
	if err := os.WriteFile(csproj1, []byte(`<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup>
    <PackageReference Include="Serilog" Version="3.0.0" />
  </ItemGroup>
</Project>`), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", csproj1, err)
	}

	csproj2 := filepath.Join(dir, "Web.csproj")
	if err := os.WriteFile(csproj2, []byte(`<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup>
    <PackageReference Include="FluentValidation" Version="11.0.0" />
  </ItemGroup>
</Project>`), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", csproj2, err)
	}

	// Package not in any csproj — should fall back to the first file.
	files := []string{csproj1, csproj2}
	got, err := findCsprojForPackage(files, "SomeNewPackage")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != csproj1 {
		t.Errorf("expected fallback to %q, got %q", csproj1, got)
	}
}

func TestFindCsprojForPackage_DoesNotMatchLooseText(t *testing.T) {
	dir := t.TempDir()

	// This csproj mentions "Newtonsoft.Json" in a comment but NOT as a PackageReference.
	csproj1 := filepath.Join(dir, "App.csproj")
	if err := os.WriteFile(csproj1, []byte(`<Project Sdk="Microsoft.NET.Sdk">
  <!-- Removed Newtonsoft.Json in favor of System.Text.Json -->
  <ItemGroup>
    <PackageReference Include="System.Text.Json" Version="8.0.0" />
  </ItemGroup>
</Project>`), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", csproj1, err)
	}

	// This csproj has the actual PackageReference.
	csproj2 := filepath.Join(dir, "Legacy.csproj")
	if err := os.WriteFile(csproj2, []byte(`<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup>
    <PackageReference Include="Newtonsoft.Json" Version="13.0.1" />
  </ItemGroup>
</Project>`), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", csproj2, err)
	}

	got, err := findCsprojForPackage([]string{csproj1, csproj2}, "Newtonsoft.Json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != csproj2 {
		t.Errorf("expected %q (with PackageReference), got %q", csproj2, got)
	}
}

func TestFindCsprojFiles_SkipsExcludedDirs(t *testing.T) {
	dir := t.TempDir()

	// Create valid csproj files.
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "App.csproj"), []byte("<Project/>"), 0644); err != nil {
		t.Fatalf("failed to write App.csproj: %v", err)
	}

	// Create csproj files in excluded directories.
	for _, excluded := range []string{"bin", "obj", ".git", "node_modules"} {
		excDir := filepath.Join(dir, "src", excluded)
		if err := os.MkdirAll(excDir, 0755); err != nil {
			t.Fatalf("failed to create %s dir: %v", excluded, err)
		}
		if err := os.WriteFile(filepath.Join(excDir, "Bad.csproj"), []byte("<Project/>"), 0644); err != nil {
			t.Fatalf("failed to write Bad.csproj in %s: %v", excluded, err)
		}
	}

	files, err := findCsprojFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 csproj file, got %d: %v", len(files), files)
	}
	if filepath.Base(files[0]) != "App.csproj" {
		t.Errorf("expected App.csproj, got %s", filepath.Base(files[0]))
	}
}

func TestRollbackGroup_RestoresChanges(t *testing.T) {
	dir := initTestGitRepo(t)

	// Create and commit a file.
	testFile := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("original"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial")

	// Modify the file (uncommitted change).
	if err := os.WriteFile(testFile, []byte("modified"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	err := RollbackGroup(t.Context(), dir, UpdateGroup{Name: "test", Kind: "patch"}, fmt.Errorf("test failure"))
	if err != nil {
		t.Fatalf("RollbackGroup failed: %v", err)
	}

	data, _ := os.ReadFile(testFile)
	if string(data) != "original" {
		t.Errorf("expected file restored to %q, got %q", "original", string(data))
	}
}

func TestRollbackGroup_CleansUntrackedFiles(t *testing.T) {
	dir := initTestGitRepo(t)

	// Create an initial commit so the repo isn't empty.
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0644); err != nil {
		t.Fatalf("failed to write keep.txt: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial")

	// Create an untracked file.
	untrackedFile := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("junk"), 0644); err != nil {
		t.Fatalf("failed to write untracked file: %v", err)
	}

	err := RollbackGroup(t.Context(), dir, UpdateGroup{Name: "test", Kind: "patch"}, fmt.Errorf("test failure"))
	if err != nil {
		t.Fatalf("RollbackGroup failed: %v", err)
	}

	if _, err := os.Stat(untrackedFile); !os.IsNotExist(err) {
		t.Error("expected untracked file to be cleaned up")
	}
}

func TestCommitGroup_CreatesCommit(t *testing.T) {
	dir := initTestGitRepo(t)

	// Create and commit a baseline.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test"), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial")

	// Make a change to commit via CommitGroup.
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("some-dep v1.2.3 h1:abc"), 0644); err != nil {
		t.Fatalf("failed to write go.sum: %v", err)
	}

	group := UpdateGroup{
		Name: "some-dep",
		Kind: "patch",
		Updates: []depcheck.ModuleUpdate{
			{Path: "some-dep", Current: "1.2.2", Latest: "1.2.3", Kind: "patch"},
		},
	}

	if err := CommitGroup(t.Context(), dir, group); err != nil {
		t.Fatalf("CommitGroup failed: %v", err)
	}

	// Verify the commit message.
	out := runGit(t, dir, "log", "-1", "--pretty=%s")
	expectedSubject := "chore(deps): update some-dep (patch)"
	if strings.TrimSpace(out) != expectedSubject {
		t.Errorf("commit subject = %q, want %q", strings.TrimSpace(out), expectedSubject)
	}

	// Verify no uncommitted changes remain.
	status := runGit(t, dir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("expected clean working tree after commit, got: %s", status)
	}
}

// initTestGitRepo creates a temporary git repo and returns its path.
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	return dir
}

// runGit runs a git command in the given directory and returns stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("git %s failed: %v\nstderr: %s", strings.Join(args, " "), err, stderr)
	}
	return string(out)
}
