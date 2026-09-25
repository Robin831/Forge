package main

import (
	"encoding/json"
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

func TestAssayRerunShaFlag(t *testing.T) {
	flag := assayRerunCmd.Flags().Lookup("sha")
	if flag == nil {
		t.Fatal("assay rerun must expose --sha")
	}
	if flag.DefValue != "" {
		t.Errorf("--sha must default to empty (a head review), got %q", flag.DefValue)
	}
	if flag.Annotations[cobra.BashCompOneRequiredFlag] != nil {
		t.Error("--sha must be optional")
	}
	if err := assayRerunCmd.ParseFlags([]string{"--anvil", "munin", "--sha", "46e0f72"}); err != nil {
		t.Fatalf("parsing --sha: %v", err)
	}
	t.Cleanup(func() {
		for _, name := range []string{"sha", "anvil"} {
			_ = assayRerunCmd.Flags().Set(name, "")
			assayRerunCmd.Flags().Lookup(name).Changed = false
		}
	})
	if got, _ := assayRerunCmd.Flags().GetString("sha"); got != "46e0f72" {
		t.Errorf("--sha parsed as %q, want 46e0f72", got)
	}
}

func TestAssayRerunPayload(t *testing.T) {
	t.Run("without --sha the payload is the head review, unchanged on the wire", func(t *testing.T) {
		p, err := assayRerunPayload("munin", 5391, "", false)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(p)
		if string(b) != `{"anvil":"munin","pr_number":5391}` {
			t.Errorf("payload = %s, want the pre-flag shape", b)
		}
	})
	t.Run("a commit id is carried", func(t *testing.T) {
		p, err := assayRerunPayload("munin", 5391, " 46e0f72 ", true)
		if err != nil {
			t.Fatal(err)
		}
		if p.SHA != "46e0f72" || p.PRNumber != 5391 || p.Anvil != "munin" {
			t.Errorf("payload = %+v", p)
		}
	})
	for _, bad := range []string{"", "abc", "not-a-sha", "--upload-pack=x", "46e0f72f946a46e0f72f946a46e0f72f946a46e0f"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, err := assayRerunPayload("munin", 5391, bad, true); err == nil {
				t.Errorf("--sha %q should be rejected", bad)
			}
		})
	}
}
