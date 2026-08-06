package ipc

import (
	"encoding/json"
	"testing"
)

// TestFormatDispatchPause covers the four rendering cases the status line has
// to get right: no pause, an operator pause, a self-deploy drain (with and
// without detail), and an unknown reason from a newer daemon.
func TestFormatDispatchPause(t *testing.T) {
	tests := []struct {
		name   string
		paused bool
		reason string
		detail string
		want   string
	}{
		{
			name:   "not paused",
			paused: false,
			reason: PauseReasonManual,
			want:   "",
		},
		{
			name:   "manual",
			paused: true,
			reason: PauseReasonManual,
			want:   "PAUSED (manual) — running workers continue",
		},
		{
			name:   "empty reason falls back to manual",
			paused: true,
			reason: "",
			want:   "PAUSED (manual) — running workers continue",
		},
		{
			name:   "self-deploy with detail",
			paused: true,
			reason: PauseReasonSelfDeploy,
			detail: "waiting on 2 workers, max 30m",
			want:   "PAUSED (self-deploy drain, waiting on 2 workers, max 30m) — running workers continue",
		},
		{
			name:   "self-deploy without detail",
			paused: true,
			reason: PauseReasonSelfDeploy,
			want:   "PAUSED (self-deploy drain) — running workers continue",
		},
		{
			name:   "unknown reason is printed verbatim",
			paused: true,
			reason: "hot-reload",
			want:   "PAUSED (hot-reload) — running workers continue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatDispatchPause(tt.paused, tt.reason, tt.detail); got != tt.want {
				t.Errorf("FormatDispatchPause(%v, %q, %q) = %q, want %q",
					tt.paused, tt.reason, tt.detail, got, tt.want)
			}
		})
	}
}

// TestStatusPayload_PauseFieldsAreAdditive pins the wire contract: the boolean
// stays exactly where it was, and the two new fields are omitted entirely when
// empty so a client that only knows dispatch_paused sees no change.
func TestStatusPayload_PauseFieldsAreAdditive(t *testing.T) {
	withReason, err := json.Marshal(StatusPayload{
		DispatchPaused:      true,
		DispatchPauseReason: PauseReasonSelfDeploy,
		DispatchPauseDetail: "waiting on 1 worker, max 30m",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(withReason, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round["dispatch_paused"] != true {
		t.Errorf("dispatch_paused = %v, want true", round["dispatch_paused"])
	}
	if round["dispatch_pause_reason"] != PauseReasonSelfDeploy {
		t.Errorf("dispatch_pause_reason = %v, want %q", round["dispatch_pause_reason"], PauseReasonSelfDeploy)
	}
	if round["dispatch_pause_detail"] != "waiting on 1 worker, max 30m" {
		t.Errorf("dispatch_pause_detail = %v", round["dispatch_pause_detail"])
	}

	bare, err := json.Marshal(StatusPayload{DispatchPaused: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var bareRound map[string]any
	if err := json.Unmarshal(bare, &bareRound); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := bareRound["dispatch_pause_reason"]; ok {
		t.Error("dispatch_pause_reason must be omitted when empty")
	}
	if _, ok := bareRound["dispatch_pause_detail"]; ok {
		t.Error("dispatch_pause_detail must be omitted when empty")
	}
}
