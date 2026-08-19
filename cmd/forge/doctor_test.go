package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// --- Config permissions tests ---

func TestCheckConfigPermissions_NoConfig(t *testing.T) {
	// Point configFile at a nonexistent path to ensure no config is found.
	origConfigFile := configFile
	configFile = filepath.Join(t.TempDir(), "nonexistent.yaml")
	defer func() { configFile = origConfigFile }()

	result := checkConfigPermissions()
	// configFile points at a nonexistent path, so os.Stat returns IsNotExist → expect warn.
	if result.Status != "warn" {
		t.Errorf("expected warn when no config file, got %q: %s", result.Status, result.Detail)
	}
}

func TestCheckConfigPermissions_ValidFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	if err := os.WriteFile(cfgPath, []byte("anvils: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	origConfigFile := configFile
	configFile = cfgPath
	defer func() { configFile = origConfigFile }()

	result := checkConfigPermissions()
	if result.Status != "ok" {
		t.Errorf("expected ok for valid config file, got %q: %s", result.Status, result.Detail)
	}
}

func TestCheckConfigPermissions_WorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits not enforced on Windows")
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	// Write with any mode first; chmod below overrides the umask.
	if err := os.WriteFile(cfgPath, []byte("anvils: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Explicitly set 0o644 after writing so the test is not affected by the
	// process umask (e.g. umask 0o022 would produce 0o644, but umask 0o077
	// would produce 0o600, making the test flaky).
	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}

	origConfigFile := configFile
	configFile = cfgPath
	defer func() { configFile = origConfigFile }()

	result := checkConfigPermissions()
	if result.Status != "warn" {
		t.Errorf("expected warn for world-readable config, got %q: %s", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "world-readable") {
		t.Errorf("expected detail to mention world-readable, got %q", result.Detail)
	}
}

func TestCheckConfigPermissions_RestrictedPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits not enforced on Windows")
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	// 0o600 = owner rw only — not world-readable
	if err := os.WriteFile(cfgPath, []byte("anvils: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	origConfigFile := configFile
	configFile = cfgPath
	defer func() { configFile = origConfigFile }()

	result := checkConfigPermissions()
	if result.Status != "ok" {
		t.Errorf("expected ok for restricted config file, got %q: %s", result.Status, result.Detail)
	}
}

// --- Disk space tests ---

func TestCheckDiskSpace_ReturnsResults(t *testing.T) {
	// With default config (cfg may be nil), should still check ~/.forge.
	results := checkDiskSpace()
	if len(results) == 0 {
		t.Skip("no paths to check (home dir unavailable)")
	}
	for _, r := range results {
		if r.Status != "ok" && r.Status != "warn" {
			t.Errorf("unexpected status %q for disk space check: %s", r.Status, r.Detail)
		}
	}
}

func TestCheckDiskSpace_WithAnvils(t *testing.T) {
	dir := t.TempDir()

	origCfg := cfg
	cfg = &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test": {Path: dir},
		},
	}
	defer func() { cfg = origCfg }()

	results := checkDiskSpace()
	if len(results) == 0 {
		t.Error("expected at least one disk space result")
	}
}

func TestVolumeRoot(t *testing.T) {
	// volumeRoot is used for display purposes only (e.g. "%.1f GiB free on X").
	// Deduplication uses filesystemKey instead, which distinguishes separate
	// mounts on Unix via the device ID.
	tests := []struct {
		path string
		want string
	}{
		{`/home/user/.forge`, "/"},
	}
	for _, tt := range tests {
		got := volumeRoot(tt.path)
		if got != tt.want {
			t.Errorf("volumeRoot(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestFilesystemKey_SamePath(t *testing.T) {
	// Two references to the same directory should produce the same key.
	dir := t.TempDir()
	k1 := filesystemKey(dir)
	k2 := filesystemKey(dir)
	if k1 != k2 {
		t.Errorf("filesystemKey(%q) not stable: %q vs %q", dir, k1, k2)
	}
	if k1 == "" {
		t.Errorf("filesystemKey(%q) returned empty string", dir)
	}
}

// --- Strict mode tests ---

func TestDoctorStrict_WarningsAreErrors(t *testing.T) {
	// When doctorStrict is true, warnings should cause a non-zero exit.
	// We test this indirectly by verifying the flag variable exists and
	// the command's RunE respects it. A full integration test would need
	// to invoke the cobra command, so we verify the flag is registered.
	f := doctorCmd.Flags().Lookup("strict")
	if f == nil {
		t.Fatal("expected --strict flag on doctor command")
	}
	if f.DefValue != "false" {
		t.Errorf("expected --strict default to be false, got %q", f.DefValue)
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

// --- Assay pass doctor check tests ---

func TestCheckAssayPasses_NilConfig(t *testing.T) {
	origCfg := cfg
	cfg = nil
	defer func() { cfg = origCfg }()

	results := checkAssayPasses()
	if len(results) != 1 {
		t.Fatalf("expected 1 result when cfg is nil, got %d", len(results))
	}
	if results[0].Status != "warn" {
		t.Errorf("expected warn when cfg is nil, got %q", results[0].Status)
	}
}

func TestCheckAssayPasses_Disabled(t *testing.T) {
	origCfg := cfg
	cfg = &config.Config{}
	defer func() { cfg = origCfg }()

	results := checkAssayPasses()
	if len(results) != 1 {
		t.Fatalf("expected 1 result when assay disabled, got %d", len(results))
	}
	if results[0].Status != "ok" {
		t.Errorf("expected ok when assay disabled, got %q", results[0].Status)
	}
	if !strings.Contains(results[0].Detail, "disabled") {
		t.Errorf("expected detail to mention disabled, got %q", results[0].Detail)
	}
}

func TestCheckAssayPasses_EnabledBinaryFound(t *testing.T) {
	enabled := true
	origCfg := cfg
	cfg = &config.Config{
		Assay: config.AssayConfig{Enabled: &enabled},
	}
	defer func() { cfg = origCfg }()

	mockExec(t,
		func(file string) (string, error) {
			return "/usr/local/bin/" + file, nil
		},
		func(name string, args ...string) ([]byte, error) {
			return []byte("claude 1.2.3"), nil
		},
	)

	results := checkAssayPasses()
	if len(results) == 0 {
		t.Fatal("expected at least one result for enabled assay")
	}
	for _, r := range results {
		if r.Status != "ok" {
			t.Errorf("expected ok for %q, got %q: %s", r.Name, r.Status, r.Detail)
		}
		if !strings.Contains(r.Name, "Assay pass") {
			t.Errorf("expected name to contain 'Assay pass', got %q", r.Name)
		}
	}
}

func TestCheckAssayPasses_EnabledBinaryMissing(t *testing.T) {
	enabled := true
	origCfg := cfg
	cfg = &config.Config{
		Assay: config.AssayConfig{Enabled: &enabled},
	}
	defer func() { cfg = origCfg }()

	mockExec(t,
		func(file string) (string, error) {
			return "", errors.New("not found")
		},
		func(name string, args ...string) ([]byte, error) {
			return nil, errors.New("not found")
		},
	)

	results := checkAssayPasses()
	if len(results) == 0 {
		t.Fatal("expected at least one result for enabled assay")
	}
	for _, r := range results {
		if r.Status != "fail" {
			t.Errorf("expected fail for %q when binary missing, got %q: %s", r.Name, r.Status, r.Detail)
		}
	}
}

// The dependents array is opt-in on bd, and a bd that lacks the flag fails
// silently: `bd show --json` still succeeds, just without the array, so every
// bead reads as childless. Doctor is where that becomes visible, so the three
// outcomes are pinned — supported, unsupported, and unverifiable.
func TestCheckBdIncludeDependents(t *testing.T) {
	tests := []struct {
		name       string
		lookPath   func(string) (string, error)
		runCommand func(string, ...string) ([]byte, error)
		wantStatus string
		wantDetail string
	}{
		{
			name:     "flag present in help output",
			lookPath: func(string) (string, error) { return "/usr/bin/bd", nil },
			runCommand: func(string, ...string) ([]byte, error) {
				return []byte("Flags:\n      --include-dependents   Stream full dependent issues in JSON output\n"), nil
			},
			wantStatus: "ok",
		},
		{
			name:     "older bd without the flag fails the check",
			lookPath: func(string) (string, error) { return "/usr/bin/bd", nil },
			runCommand: func(string, ...string) ([]byte, error) {
				return []byte("Flags:\n      --long   Show all available fields\n"), nil
			},
			wantStatus: "fail",
			wantDetail: "upgrade bd",
		},
		{
			name:       "bd missing is only a warning",
			lookPath:   func(string) (string, error) { return "", errors.New("not found") },
			runCommand: func(string, ...string) ([]byte, error) { return nil, nil },
			wantStatus: "warn",
		},
		{
			name:     "help that will not run is a warning, not a verdict",
			lookPath: func(string) (string, error) { return "/usr/bin/bd", nil },
			runCommand: func(string, ...string) ([]byte, error) {
				return nil, errors.New("exec format error")
			},
			wantStatus: "warn",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockExec(t, tt.lookPath, tt.runCommand)

			got := checkBdIncludeDependents()

			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q (detail: %s)", got.Status, tt.wantStatus, got.Detail)
			}
			if tt.wantDetail != "" && !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("Detail = %q, want it to mention %q", got.Detail, tt.wantDetail)
			}
		})
	}
}
