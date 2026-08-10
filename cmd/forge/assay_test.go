package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestParsePRNumberArg(t *testing.T) {
	tests := []struct {
		name    string
		arg     string
		want    int
		wantErr bool
	}{
		{"plain", "431", 431, false},
		{"hash prefix as copied off a PR page", "#431", 431, false},
		{"surrounding space", "  431 ", 431, false},
		// Zero is the daemon's "no target supplied", so it must never be sent.
		{"zero", "0", 0, true},
		{"negative", "-3", 0, true},
		{"not a number", "abc", 0, true},
		{"empty", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePRNumberArg(tt.arg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePRNumberArg(%q) = %d, want an error", tt.arg, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePRNumberArg(%q) returned %v", tt.arg, err)
			}
			if got != tt.want {
				t.Errorf("parsePRNumberArg(%q) = %d, want %d", tt.arg, got, tt.want)
			}
		})
	}
}

// TestAssayRerunCmdWiring pins the verb's shape: one positional PR number and a
// required --anvil, since a PR number means nothing without the repository.
func TestAssayRerunCmdWiring(t *testing.T) {
	if assayRerunCmd.Args == nil {
		t.Fatal("assay rerun must constrain its positional args")
	}
	if err := assayRerunCmd.Args(assayRerunCmd, []string{}); err == nil {
		t.Error("assay rerun with no PR number should be rejected")
	}
	if err := assayRerunCmd.Args(assayRerunCmd, []string{"1", "2"}); err == nil {
		t.Error("assay rerun with two positionals should be rejected")
	}
	if err := assayRerunCmd.Args(assayRerunCmd, []string{"431"}); err != nil {
		t.Errorf("assay rerun with one PR number should be accepted: %v", err)
	}

	flag := assayRerunCmd.Flags().Lookup("anvil")
	if flag == nil {
		t.Fatal("assay rerun must expose --anvil")
	}
	// BashCompOneRequiredFlag is the annotation MarkFlagRequired sets; reading
	// cobra's own constant keeps the check true if the key ever changes.
	if flag.Annotations[cobra.BashCompOneRequiredFlag] == nil {
		t.Error("--anvil must be required on assay rerun")
	}

	if parent := assayRerunCmd.Parent(); parent == nil || parent.Name() != "assay" {
		t.Errorf("assay rerun must hang off the assay command, got %v", parent)
	}
}
