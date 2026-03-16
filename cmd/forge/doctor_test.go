package main

import (
	"errors"
	"testing"

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
