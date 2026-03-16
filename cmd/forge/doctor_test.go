package main

import (
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
	// Only meaningful when env vars are not set; skip if they are.
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
