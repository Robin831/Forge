package warden

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractLintRuleNames(t *testing.T) {
	tests := []struct {
		name     string
		logs     map[string]string
		expected []string
	}{
		{
			name:     "empty logs",
			logs:     map[string]string{},
			expected: nil,
		},
		{
			name: "react-hooks rule",
			logs: map[string]string{
				"eslint": "  2:5  error  React Hook useEffect contains a call to setState  react-hooks/exhaustive-deps",
			},
			expected: []string{"react-hooks/exhaustive-deps"},
		},
		{
			name: "scoped typescript rule",
			logs: map[string]string{
				"eslint": "  5:3  error  Promises must be awaited  @typescript-eslint/no-floating-promises",
			},
			expected: []string{"@typescript-eslint/no-floating-promises"},
		},
		{
			name: "multiple rules deduplicated",
			logs: map[string]string{
				"eslint": "  2:5  error  msg  react-hooks/exhaustive-deps\n  7:1  error  msg  react-hooks/exhaustive-deps\n  9:3  error  msg  import/no-cycle",
			},
			expected: []string{"import/no-cycle", "react-hooks/exhaustive-deps"},
		},
		{
			name: "rules across multiple log sources",
			logs: map[string]string{
				"check1": "  1:1  error  msg  react-hooks/rules-of-hooks",
				"check2": "  3:2  error  msg  import/no-cycle",
			},
			expected: []string{"import/no-cycle", "react-hooks/rules-of-hooks"},
		},
		{
			name: "no lint rules in logs",
			logs: map[string]string{
				"build": "Error: cannot find module './foo'\n  at Object.<anonymous> (src/index.ts:1:1)",
			},
			expected: nil,
		},
		{
			name: "no-hyphen rule import/order is matched",
			logs: map[string]string{
				"eslint": "  1:1  error  Import order violation  import/order",
			},
			expected: []string{"import/order"},
		},
		{
			name: "file path prefixes are excluded",
			logs: map[string]string{
				"build": "  at src/components/App\n  at internal/server/handler",
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLintRuleNames(tt.logs)
			if len(got) != len(tt.expected) {
				t.Fatalf("extractLintRuleNames() = %v, want %v", got, tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("extractLintRuleNames()[%d] = %q, want %q", i, got[i], tt.expected[i])
				}
			}
		})
	}
}

// TestLearnFromCIFix_SkipExisting verifies that LearnFromCIFix does not add
// a duplicate rule when a matching rule ID already exists in warden-rules.yaml.
func TestLearnFromCIFix_SkipExisting(t *testing.T) {
	anvilPath := t.TempDir()

	// Pre-populate a rules file with the rule that would be derived from the log.
	// The derived ID for "react-hooks/exhaustive-deps" is "react-hooks-exhaustive-deps".
	existing := &RulesFile{
		Rules: []Rule{
			{
				ID:       "react-hooks-exhaustive-deps",
				Category: "ui",
				Pattern:  "calling setState inside useEffect",
				Check:    "don't call setState unconditionally in useEffect",
				Source:   "cifix:PR#1",
				Added:    "2025-01-01",
			},
		},
	}
	if err := SaveRules(anvilPath, existing); err != nil {
		t.Fatalf("SaveRules: %v", err)
	}

	logs := map[string]string{
		"eslint": "  2:5  error  React Hook issue  react-hooks/exhaustive-deps",
	}
	fixDiff := "diff --git a/src/Foo.tsx b/src/Foo.tsx\n--- a/src/Foo.tsx\n+++ b/src/Foo.tsx\n@@ -1 +1 @@\n-bad\n+good"

	// LearnFromCIFix should detect the existing rule and skip Claude entirely.
	// Since this rule ID already exists, no Claude call is made and no error returned.
	ctx := context.Background()
	err := LearnFromCIFix(ctx, anvilPath, anvilPath, logs, fixDiff, 42)
	if err != nil {
		t.Fatalf("LearnFromCIFix returned unexpected error: %v", err)
	}

	// Verify the rules file was NOT modified (still has exactly one rule).
	loaded, err := LoadRules(anvilPath)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(loaded.Rules) != 1 {
		t.Errorf("expected 1 rule (unchanged), got %d", len(loaded.Rules))
	}
}

// TestLearnFromCIFix_DistillsNewRule verifies that LearnFromCIFix calls the
// AI runner and stores a new rule when the rule ID does not yet exist.
func TestLearnFromCIFix_DistillsNewRule(t *testing.T) {
	anvilPath := t.TempDir()

	// Stub out aiRunner so no real process is spawned.
	old := aiRunner
	t.Cleanup(func() { aiRunner = old })
	aiRunner = func(_ context.Context, _, _ string) ([]byte, error) {
		return []byte(`{"id":"react-hooks-exhaustive-deps","category":"ui","pattern":"missing dep in useEffect","check":"ensure all used values are in the deps array"}`), nil
	}

	logs := map[string]string{
		"eslint": "  2:5  error  React Hook issue  react-hooks/exhaustive-deps",
	}
	fixDiff := "diff --git a/src/Foo.tsx b/src/Foo.tsx\n--- a/src/Foo.tsx\n+++ b/src/Foo.tsx\n@@ -1 +1 @@\n-bad\n+good"

	ctx := context.Background()
	if err := LearnFromCIFix(ctx, anvilPath, anvilPath, logs, fixDiff, 99); err != nil {
		t.Fatalf("LearnFromCIFix returned unexpected error: %v", err)
	}

	loaded, err := LoadRules(anvilPath)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(loaded.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(loaded.Rules))
	}
	r := loaded.Rules[0]
	if r.ID != "react-hooks-exhaustive-deps" {
		t.Errorf("unexpected rule ID %q", r.ID)
	}
	wantSource := fmt.Sprintf("cifix:PR#%d", 99)
	if r.Source != wantSource {
		t.Errorf("expected source %q, got %q", wantSource, r.Source)
	}
}

// TestLearnFromCIFix_CapRules verifies that LearnFromCIFix stops calling AI
// after maxRulesToLearn distillation calls, even when more rules are present.
func TestLearnFromCIFix_CapRules(t *testing.T) {
	anvilPath := t.TempDir()

	callCount := 0
	old := aiRunner
	t.Cleanup(func() { aiRunner = old })
	aiRunner = func(_ context.Context, _, prompt string) ([]byte, error) {
		callCount++
		// Return a valid rule JSON that matches the expected ruleID format.
		return []byte(`{"id":"placeholder","category":"style","pattern":"x","check":"y"}`), nil
	}

	// Build logs with 7 distinct lint rules (exceeds the cap of 5).
	logs := map[string]string{
		"eslint": strings.Join([]string{
			"error  react-hooks/exhaustive-deps",
			"error  react-hooks/rules-of-hooks",
			"error  import/no-cycle",
			"error  import/order",
			"error  jsx-a11y/alt-text",
			"error  jsx-a11y/no-autofocus",
			"error  unicorn/no-null",
		}, "\n"),
	}
	fixDiff := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-bad\n+good"

	ctx := context.Background()
	if err := LearnFromCIFix(ctx, anvilPath, anvilPath, logs, fixDiff, 7); err != nil {
		t.Fatalf("LearnFromCIFix returned unexpected error: %v", err)
	}

	if callCount > 5 {
		t.Errorf("expected at most 5 Claude calls (cap), got %d", callCount)
	}
}

// TestLearnFromCIFix_NoLogs verifies no error is returned for empty inputs.
func TestLearnFromCIFix_NoLogs(t *testing.T) {
	ctx := context.Background()
	anvilPath := t.TempDir()

	// No logs — should return nil immediately.
	if err := LearnFromCIFix(ctx, anvilPath, anvilPath, nil, "some diff", 1); err != nil {
		t.Errorf("expected nil for empty logs, got %v", err)
	}

	// No diff — should return nil immediately.
	logs := map[string]string{"eslint": "react-hooks/exhaustive-deps"}
	if err := LearnFromCIFix(ctx, anvilPath, anvilPath, logs, "", 1); err != nil {
		t.Errorf("expected nil for empty diff, got %v", err)
	}

	// Ensure no rules file was created.
	if _, err := os.Stat(filepath.Join(anvilPath, RulesFileName)); !os.IsNotExist(err) {
		t.Error("expected no rules file to be created for empty inputs")
	}
}
