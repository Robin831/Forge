package kiln

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// claimRestartLocked is the whole policy in one function, so it is tested as
// one: which deaths a manifest asked Kiln to recover from, and how many times.
// The process-level behaviour it drives lives in restart_unix_test.go.
func TestClaimRestart(t *testing.T) {
	code := func(n int) *int { return &n }

	tests := []struct {
		name        string
		service     Service
		consumed    int
		exitCode    *int
		wantAttempt int
		wantRestart bool
	}{
		{
			name:     "default policy never restarts",
			service:  Service{Restart: RestartOff},
			exitCode: code(1),
		},
		{
			name:        "on-failure restarts a non-zero exit",
			service:     Service{Restart: RestartOnFailure, MaxRestarts: 3},
			exitCode:    code(1),
			wantAttempt: 1,
			wantRestart: true,
		},
		{
			// A process killed by a signal has no exit status. Nothing asks a
			// preview service to be SIGKILLed, and the one death that is
			// intentional — teardown — never reaches this decision.
			name:        "on-failure restarts a signalled process",
			service:     Service{Restart: RestartOnFailure, MaxRestarts: 3},
			exitCode:    nil,
			wantAttempt: 1,
			wantRestart: true,
		},
		{
			// The service did what it was told. Relaunching it would argue with
			// a decision the process already made.
			name:     "on-failure leaves a clean exit alone",
			service:  Service{Restart: RestartOnFailure, MaxRestarts: 3},
			exitCode: code(0),
		},
		{
			name:        "budget partly spent",
			service:     Service{Restart: RestartOnFailure, MaxRestarts: 3},
			consumed:    2,
			exitCode:    code(1),
			wantAttempt: 3,
			wantRestart: true,
		},
		{
			name:     "budget spent",
			service:  Service{Restart: RestartOnFailure, MaxRestarts: 2},
			consumed: 2,
			exitCode: code(1),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &previewService{
				spec:     ServiceSpec{Service: tc.service},
				restarts: tc.consumed,
			}
			attempt, restart := svc.claimRestartLocked(tc.exitCode)
			if restart != tc.wantRestart || attempt != tc.wantAttempt {
				t.Fatalf("claimRestartLocked = (%d, %v), want (%d, %v)",
					attempt, restart, tc.wantAttempt, tc.wantRestart)
			}
			// A refused restart must not consume budget, or a preview would
			// quietly run out of attempts it never made.
			wantConsumed := tc.consumed
			if tc.wantRestart {
				wantConsumed++
			}
			if svc.restarts != wantConsumed {
				t.Errorf("restarts = %d, want %d", svc.restarts, wantConsumed)
			}
		})
	}
}

func TestRestartDelayBacksOff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		attempt := i + 1
		if got := restartDelay(attempt); got != w {
			t.Errorf("restartDelay(%d) = %s, want %s", attempt, got, w)
		}
	}
	// Attempt numbers are 1-based; a caller that ever passes 0 must still wait.
	if got := restartDelay(0); got != restartBackoffBase {
		t.Errorf("restartDelay(0) = %s, want %s", got, restartBackoffBase)
	}
}

func TestFormatServiceRestart(t *testing.T) {
	if got, want := FormatRestartAttempt(1, 3), "attempt 1 of 3"; got != want {
		t.Errorf("FormatRestartAttempt = %q, want %q", got, want)
	}
	got := FormatServiceRestart(1, 3, state.PreviewServiceHealthy, nil)
	if want := "restarted (attempt 1 of 3): healthy"; got != want {
		t.Errorf("FormatServiceRestart = %q, want %q", got, want)
	}
	// The failure text is the only part that says what to fix, so it survives
	// verbatim.
	got = FormatServiceRestart(3, 3, state.PreviewServiceFailed, errors.New("not healthy within 60s: connection refused"))
	if !strings.HasPrefix(got, "restart failed (attempt 3 of 3): ") ||
		!strings.Contains(got, "connection refused") {
		t.Errorf("FormatServiceRestart = %q, want it to name the attempt and the cause", got)
	}
}
