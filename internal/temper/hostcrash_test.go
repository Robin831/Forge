package temper

import (
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

// realistic VSTest all-passed summaries for the two Munin assemblies.
const passedSummaries = `Passed!  - Failed:     0, Passed:   455, Skipped:     0, Total:   455, Duration: 12 s
Passed!  - Failed:     0, Passed:  6617, Skipped:     0, Total:  6617, Duration: 3 m`

func TestDotnetTestHostCrashTolerable(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "all passed + OOM host crash → tolerate",
			out:  passedSummaries + "\nTest host process crashed : Out of memory\n",
			want: true,
		},
		{
			name: "all passed + active-run-aborted → tolerate",
			out:  passedSummaries + "\nThe active test run was aborted. Reason: \n",
			want: true,
		},
		{
			name: "host crash but a test failed → do NOT tolerate",
			out:  "Failed!  - Failed:     3, Passed:   452, Total: 455\nTest host process crashed : Out of memory\n",
			want: false,
		},
		{
			name: "non-zero failed count with a crash marker → do NOT tolerate",
			out:  "Passed!  - Failed:     0, Passed: 455\nFailed:  2\nTest host process crashed\n",
			want: false,
		},
		{
			name: "all passed but NO crash marker (e.g. build error) → do NOT tolerate",
			out:  passedSummaries + "\nerror CS1002: ; expected\nBuild FAILED\n",
			want: false,
		},
		{
			name: "crash marker but no completed pass summary → do NOT tolerate",
			out:  "Starting test execution...\nTest host process crashed : Out of memory\n",
			want: false,
		},
		{
			name: "empty output → do NOT tolerate",
			out:  "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dotnetTestHostCrashTolerable(tc.out); got != tc.want {
				t.Errorf("dotnetTestHostCrashTolerable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConfigFromSteps_TolerateHostCrash verifies the config flag threads through
// to the temper.Step.
func TestConfigFromSteps_TolerateHostCrash(t *testing.T) {
	cfg := ConfigFromSteps([]config.TemperStepConfig{
		{Name: "api-test", Command: "dotnet", TolerateHostCrash: true},
		{Name: "api-build", Command: "dotnet"},
	})
	if cfg == nil || len(cfg.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %+v", cfg)
	}
	if !cfg.Steps[0].TolerateHostCrash {
		t.Error("api-test step should carry TolerateHostCrash=true")
	}
	if cfg.Steps[1].TolerateHostCrash {
		t.Error("api-build step should default TolerateHostCrash=false")
	}
}
