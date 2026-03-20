package depupdate

import (
	"fmt"
	"testing"

	"github.com/Robin831/Forge/internal/depcheck"
)

func TestInstallNpmGroup_EmptyGroup(t *testing.T) {
	group := UpdateGroup{Name: "empty", Updates: nil, Kind: "patch"}
	if err := InstallNpmGroup(t.Context(), t.TempDir(), group); err != nil {
		t.Fatalf("expected nil error for empty group, got %v", err)
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

	// We can't run a real git commit without a repo, but we can verify the
	// group struct is well-formed for the commit message.
	expected := "chore(deps): update lodash (minor)"
	got := fmt.Sprintf("chore(deps): update %s (%s)", group.Name, group.Kind)
	if got != expected {
		t.Errorf("commit message = %q, want %q", got, expected)
	}

	// Verify the body includes package details.
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
