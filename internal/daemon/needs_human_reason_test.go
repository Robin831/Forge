package daemon

import (
	"testing"

	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/warden"
)

func TestNeedsHumanReason(t *testing.T) {
	const smithEscalation = "Bead premise is wrong — choose scope a/b/c"

	tests := []struct {
		name    string
		outcome *pipeline.Outcome
		want    string
	}{
		{
			name: "smith escalation wins over schematic reason",
			outcome: &pipeline.Outcome{
				SchematicResult: &schematic.Result{Reason: "decomposing would create artificial splits"},
				SmithResult:     &smith.Result{FullOutput: "Some work...\nNEEDS_HUMAN: " + smithEscalation + "\nI have not committed anything."},
			},
			want: "Smith escalated: " + smithEscalation,
		},
		{
			name: "smith escalation wins over warden no-diff",
			outcome: &pipeline.Outcome{
				ReviewResult: &warden.ReviewResult{NoDiff: true, Summary: "no changes detected"},
				SmithResult:  &smith.Result{FullOutput: "NEEDS_HUMAN: " + smithEscalation},
			},
			want: "Smith escalated: " + smithEscalation,
		},
		{
			name: "warden no-diff wins over schematic reason",
			outcome: &pipeline.Outcome{
				SchematicResult: &schematic.Result{Reason: "single cohesive change"},
				ReviewResult:    &warden.ReviewResult{NoDiff: true, Summary: "no changes detected"},
				SmithResult:     &smith.Result{FullOutput: "Smith ran but did not escalate"},
			},
			want: "Warden rejected (no diff): no changes detected",
		},
		{
			name: "schematic reason is labelled when nothing more specific exists",
			outcome: &pipeline.Outcome{
				SchematicResult: &schematic.Result{Reason: "single cohesive change"},
			},
			want: "Schematic: single cohesive change",
		},
		{
			name: "warden no-diff requires NoDiff flag",
			outcome: &pipeline.Outcome{
				ReviewResult: &warden.ReviewResult{NoDiff: false, Summary: "requested changes"},
			},
			want: "Smith produced no diff, needs human attention",
		},
		{
			name:    "nil outcome falls back to default",
			outcome: nil,
			want:    "Smith produced no diff, needs human attention",
		},
		{
			name:    "empty outcome falls back to default",
			outcome: &pipeline.Outcome{},
			want:    "Smith produced no diff, needs human attention",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsHumanReason(tt.outcome)
			if got != tt.want {
				t.Errorf("needsHumanReason() = %q, want %q", got, tt.want)
			}
		})
	}
}
