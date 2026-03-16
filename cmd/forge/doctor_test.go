package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/provider"
)

func TestCheckGit(t *testing.T) {
	// git should be available in any dev/CI environment.
	result := checkGit()
	if result.Name != "Git" {
		t.Errorf("expected Name=%q, got %q", "Git", result.Name)
	}
	if result.Status != "ok" {
		t.Skipf("git not in PATH, skipping: %s", result.Detail)
	}
	if result.Detail == "" {
		t.Error("expected non-empty Detail when git is found")
	}
}

// mockExec sets execLookPath and execRunCommand to the given stubs for the
// duration of the test, restoring the originals on cleanup.
func mockExec(t *testing.T,
	lookPath func(string) (string, error),
	runCommand func(string, ...string) ([]byte, error),
) {
	t.Helper()
	origLook := execLookPath
	origRun := execRunCommand
	execLookPath = lookPath
	execRunCommand = runCommand
	t.Cleanup(func() {
		execLookPath = origLook
		execRunCommand = origRun
	})
}

func TestCheckGit_MockFound(t *testing.T) {
	mockExec(t,
		func(file string) (string, error) {
			if file == "git" {
				return "/usr/bin/git", nil
			}
			return "", errors.New("not found")
		},
		func(name string, args ...string) ([]byte, error) {
			return []byte("git version 2.40.0"), nil
		},
	)
	result := checkGit()
	if result.Status != "ok" {
		t.Errorf("expected ok, got %q", result.Status)
	}
	if result.Detail != "git version 2.40.0" {
		t.Errorf("expected version string in Detail, got %q", result.Detail)
	}
}

func TestCheckGit_MockNotFound(t *testing.T) {
	mockExec(t,
		func(file string) (string, error) { return "", errors.New("not found") },
		func(name string, args ...string) ([]byte, error) { return nil, nil },
	)
	result := checkGit()
	if result.Status != "fail" {
		t.Errorf("expected fail when git not in PATH, got %q", result.Status)
	}
}

func TestCheckProviderCLI_Found(t *testing.T) {
	// Use "git" as a provider binary — it should always be present.
	p := provider.Provider{Kind: provider.Claude, Command: "git"}
	result := checkProviderCLI(p)
	if result.Status != "ok" {
		t.Skipf("git not in PATH, skipping: %s", result.Detail)
	}
	if result.Detail == "" {
		t.Error("expected non-empty Detail path")
	}
}

func TestCheckProviderCLI_NotFound(t *testing.T) {
	p := provider.Provider{Kind: provider.Claude, Command: "nonexistent-binary-xyz"}
	result := checkProviderCLI(p)
	if result.Status != "fail" {
		t.Errorf("expected status=fail for missing binary, got %q", result.Status)
	}
}

func TestCheckProviderAuth_BackendSkipsAuth(t *testing.T) {
	p := provider.Provider{Kind: provider.Claude, Backend: "ollama"}
	result := checkProviderAuth(p)
	if result.Status != "ok" {
		t.Errorf("expected status=ok for backend provider, got %q", result.Status)
	}
}

func TestCheckProviderAuth_UnknownKind(t *testing.T) {
	p := provider.Provider{Kind: "unknownprovider"}
	result := checkProviderAuth(p)
	if result.Status != "warn" {
		t.Errorf("expected status=warn for unknown kind, got %q", result.Status)
	}
}

func TestCheckGeminiAuth_NoKeys(t *testing.T) {
	// Force env vars to be unset so the check must return warn.
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	result := checkGeminiAuth("test")
	if result.Status != "warn" {
		t.Errorf("expected warn when no API keys set, got %q", result.Status)
	}
}

func TestCheckGeminiAuth_GoogleKey(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "test-key")
	t.Setenv("GEMINI_API_KEY", "")
	result := checkGeminiAuth("test")
	if result.Status != "ok" {
		t.Errorf("expected ok when GOOGLE_API_KEY set, got %q", result.Status)
	}
}

func TestCheckGeminiAuth_GeminiKey(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "test-key")
	result := checkGeminiAuth("test")
	if result.Status != "ok" {
		t.Errorf("expected ok when GEMINI_API_KEY set, got %q", result.Status)
	}
}

func TestCheckOpenAIAuth_NoKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	result := checkOpenAIAuth("test")
	if result.Status != "warn" {
		t.Errorf("expected warn when OPENAI_API_KEY not set, got %q", result.Status)
	}
}

func TestCheckOpenAIAuth_WithKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	result := checkOpenAIAuth("test")
	if result.Status != "ok" {
		t.Errorf("expected ok when OPENAI_API_KEY set, got %q", result.Status)
	}
}

func TestCheckClaudeAuth_WithAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	result := checkClaudeAuth("test")
	if result.Status != "ok" {
		t.Errorf("expected ok when ANTHROPIC_API_KEY set, got %q", result.Status)
	}
}

func TestCheckClaudeAuth_NoAPIKey_ReturnsWarn(t *testing.T) {
	// Without ANTHROPIC_API_KEY, claude --version only proves the binary is
	// present — it cannot verify an OAuth session. Expect warn, not ok.
	t.Setenv("ANTHROPIC_API_KEY", "")
	mockExec(t,
		func(file string) (string, error) {
			if file == "claude" {
				return "/usr/local/bin/claude", nil
			}
			return "", errors.New("not found")
		},
		func(name string, args ...string) ([]byte, error) {
			return []byte("claude 1.0.0"), nil
		},
	)
	result := checkClaudeAuth("test")
	if result.Status != "warn" {
		t.Errorf("expected warn when ANTHROPIC_API_KEY not set, got %q (detail: %s)", result.Status, result.Detail)
	}
}

// --- Dolt connectivity tests ---

func TestCheckDoltConnectivity_NoBd(t *testing.T) {
	mockExec(t,
		func(file string) (string, error) { return "", errors.New("not found") },
		func(name string, args ...string) ([]byte, error) { return nil, nil },
	)
	result := checkDoltConnectivity()
	if result.Status != "warn" {
		t.Errorf("expected warn when bd not in PATH, got %q", result.Status)
	}
}

func TestCheckDoltConnectivity_BdFails(t *testing.T) {
	mockExec(t,
		func(file string) (string, error) { return "/usr/bin/bd", nil },
		func(name string, args ...string) ([]byte, error) {
			return []byte("connection refused"), errors.New("exit 1")
		},
	)
	result := checkDoltConnectivity()
	if result.Status != "fail" {
		t.Errorf("expected fail when bd cannot connect, got %q", result.Status)
	}
	if result.Detail == "" {
		t.Error("expected non-empty detail on failure")
	}
}

func TestCheckDoltConnectivity_Success(t *testing.T) {
	mockExec(t,
		func(file string) (string, error) { return "/usr/bin/bd", nil },
		func(name string, args ...string) ([]byte, error) {
			return []byte(`[]`), nil
		},
	)
	result := checkDoltConnectivity()
	if result.Status != "ok" {
		t.Errorf("expected ok when bd list succeeds, got %q", result.Status)
	}
}

// --- Depcheck tooling tests ---

func TestCheckDepcheckTooling_NilConfig(t *testing.T) {
	origCfg := cfg
	cfg = nil
	defer func() { cfg = origCfg }()

	results := checkDepcheckTooling()
	if len(results) != 0 {
		t.Errorf("expected no results when cfg is nil, got %d", len(results))
	}
}

func TestCheckDepcheckTooling_Disabled(t *testing.T) {
	origCfg := cfg
	cfg = &config.Config{Settings: config.SettingsConfig{DepcheckInterval: 0}}
	defer func() { cfg = origCfg }()

	results := checkDepcheckTooling()
	if len(results) != 1 || results[0].Status != "ok" {
		t.Errorf("expected single ok result when depcheck disabled, got %+v", results)
	}
}

func TestCheckDepcheckTooling_GoAnvil(t *testing.T) {
	// Create a temp dir with a go.mod file.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0o644); err != nil {
		t.Fatal(err)
	}

	origCfg := cfg
	cfg = &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test": {Path: dir},
		},
		Settings: config.SettingsConfig{DepcheckInterval: 168 * time.Hour},
	}
	defer func() { cfg = origCfg }()

	results := checkDepcheckTooling()
	found := false
	for _, r := range results {
		if r.Name == "Depcheck: Go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Depcheck: Go check for Go anvil, got %+v", results)
	}
}

func TestCheckDepcheckTooling_NoEcosystems(t *testing.T) {
	dir := t.TempDir() // empty directory

	origCfg := cfg
	cfg = &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"empty": {Path: dir},
		},
		Settings: config.SettingsConfig{DepcheckInterval: 168 * time.Hour},
	}
	defer func() { cfg = origCfg }()

	results := checkDepcheckTooling()
	if len(results) != 1 || results[0].Name != "Depcheck tooling" {
		t.Errorf("expected fallback 'no ecosystems' result, got %+v", results)
	}
}

// --- Changelog fragment tests ---

func TestCheckChangelogFragments_NoDir(t *testing.T) {
	// Run from a temp dir where changelog.d doesn't exist.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(origDir); err != nil {
			t.Errorf("failed to restore working directory: %v", err)
		}
	}()

	result := checkChangelogFragments()
	if result.Status != "ok" {
		t.Errorf("expected ok when changelog.d missing, got %q", result.Status)
	}
}

func TestCheckChangelogFragments_ValidFragments(t *testing.T) {
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(origDir); err != nil {
			t.Errorf("failed to restore working directory: %v", err)
		}
	}()

	if err := os.Mkdir("changelog.d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("changelog.d", "test-1.md"), []byte("category: Added\n- **Test** - Added something\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := checkChangelogFragments()
	if result.Status != "ok" {
		t.Errorf("expected ok for valid fragments, got %q: %s", result.Status, result.Detail)
	}
}

func TestCheckChangelogFragments_InvalidFragment(t *testing.T) {
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(origDir); err != nil {
			t.Errorf("failed to restore working directory: %v", err)
		}
	}()

	if err := os.Mkdir("changelog.d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("changelog.d", "bad.md"), []byte("no category header\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := checkChangelogFragments()
	if result.Status != "warn" {
		t.Errorf("expected warn for invalid fragment, got %q: %s", result.Status, result.Detail)
	}
}

func TestCheckProviderChain_NilConfig(t *testing.T) {
	// When cfg is nil, checkProviderChain should return a single warn result
	// rather than an empty slice.
	origCfg := cfg
	cfg = nil
	defer func() { cfg = origCfg }()

	results := checkProviderChain()
	if len(results) != 1 {
		t.Fatalf("expected 1 result when cfg is nil, got %d", len(results))
	}
	if results[0].Status != "warn" {
		t.Errorf("expected warn when cfg is nil, got %q", results[0].Status)
	}
}
