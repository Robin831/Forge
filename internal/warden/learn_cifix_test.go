package warden

import (
	"context"
	"os"
	"path/filepath"
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
