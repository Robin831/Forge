package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

func idlePtr(v int64) *int64 { return &v }

func TestFormatPreviewIdle(t *testing.T) {
	tests := []struct {
		name string
		secs *int64
		want string
	}{
		// The reaper is disabled: there is no deadline at all, which must not
		// render as an imminent one.
		{"reaper disabled", nil, "-"},
		{"deadline passed", idlePtr(0), "due"},
		{"clock skew", idlePtr(-5), "due"},
		{"sub-minute", idlePtr(45), "45s"},
		{"minutes and seconds", idlePtr(272), "4m32s"},
		{"hours", idlePtr(3600), "1h0m0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatPreviewIdle(tt.secs); got != tt.want {
				t.Errorf("formatPreviewIdle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOrDash(t *testing.T) {
	if got := orDash(""); got != "-" {
		t.Errorf("orDash(\"\") = %q, want \"-\"", got)
	}
	if got := orDash("x"); got != "x" {
		t.Errorf("orDash(\"x\") = %q, want \"x\"", got)
	}
}

func TestRenderPreviewList_Disabled(t *testing.T) {
	var buf bytes.Buffer
	renderPreviewList(&buf, ipc.PreviewListResponse{Enabled: false})

	got := buf.String()
	if !strings.Contains(got, "disabled") {
		t.Errorf("expected a disabled notice, got %q", got)
	}
	// "disabled" and "none running" are different states and must not be
	// reported with the same line.
	if strings.Contains(got, "No previews are running") {
		t.Errorf("disabled Kiln must not read as an empty fleet, got %q", got)
	}
}

func TestRenderPreviewList_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderPreviewList(&buf, ipc.PreviewListResponse{Enabled: true})

	if got := buf.String(); got != "No previews are running.\n" {
		t.Errorf("unexpected output %q", got)
	}
}

func TestRenderPreviewList_Table(t *testing.T) {
	var buf bytes.Buffer
	renderPreviewList(&buf, ipc.PreviewListResponse{
		Enabled: true,
		Previews: []ipc.PreviewInfo{
			{
				BeadID:               "Forge-abc1",
				Status:               "running",
				EntryURL:             "http://localhost:41000",
				IdleRemainingSeconds: idlePtr(272),
				ResourceNote:         "2 services, ports 41000, 41001",
			},
			{
				// Ports not allocated yet: no URL, no deadline.
				BeadID:       "Forge-def2",
				Status:       "starting",
				ResourceNote: "1 service, no ports allocated",
			},
		},
	})

	got := buf.String()
	for _, want := range []string{
		"BEAD", "STATUS", "URL", "IDLE", "RESOURCES",
		"Forge-abc1", "running", "http://localhost:41000", "4m32s", "2 services, ports 41000, 41001",
		"Forge-def2", "starting", "1 service, no ports allocated",
		"2 preview(s)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}

	// The preview with no URL and no deadline still renders both columns.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "Forge-def2") {
			if strings.Count(line, "-") < 2 {
				t.Errorf("expected dashes for the missing URL and idle columns: %q", line)
			}
		}
	}
}

func TestRenderPreviewList_ServiceError(t *testing.T) {
	var buf bytes.Buffer
	renderPreviewList(&buf, ipc.PreviewListResponse{
		Enabled: true,
		Previews: []ipc.PreviewInfo{
			{
				BeadID: "Forge-abc1",
				Status: "degraded",
				Services: []ipc.PreviewServiceInfo{
					{Name: "web", Port: 41000, Health: "healthy", Entry: true},
					{Name: "api", Port: 41001, Health: "failed", Error: "health check timed out"},
				},
				ResourceNote: "2 services, ports 41000, 41001",
			},
		},
	})

	got := buf.String()
	if !strings.Contains(got, "Forge-abc1/api: health check timed out") {
		t.Errorf("expected the failed service's error to be surfaced:\n%s", got)
	}
	if strings.Contains(got, "Forge-abc1/web") {
		t.Errorf("healthy services must not be listed as errors:\n%s", got)
	}
}

// Forge-bci1: a service that came up and later died reads as `exited (exit 1,
// lived 7m31s)` here, and the URL column says why it has no link instead of
// showing a bare dash next to a `degraded` status.
func TestRenderPreviewList_ExitedService(t *testing.T) {
	started := time.Now().Add(-10 * time.Minute)
	code := 1
	var buf bytes.Buffer
	renderPreviewList(&buf, ipc.PreviewListResponse{
		Enabled: true,
		Previews: []ipc.PreviewInfo{
			{
				BeadID:    "Forge-abc1",
				Status:    "degraded",
				EntryNote: `entry service "client" is not serving: exited (exit 1, lived 7m31s)`,
				Services: []ipc.PreviewServiceInfo{
					{Name: "api", Port: 41001, Health: state.PreviewServiceHealthy},
					{
						Name: "client", Port: 41000, Entry: true,
						Health:    state.PreviewServiceExited,
						Error:     "exited (exit 1, lived 7m31s)",
						StartedAt: started,
						ExitedAt:  started.Add(7*time.Minute + 31*time.Second),
						ExitCode:  &code,
					},
				},
				ResourceNote: "2 services, ports 41000, 41001",
			},
		},
	})

	got := buf.String()
	for _, want := range []string{
		"Forge-abc1/client: exited (exit 1, lived 7m31s)",
		`entry service "client" is not serving`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Forge-abc1/api") {
		t.Errorf("a healthy service must not be listed as an issue:\n%s", got)
	}
}

// TestRenderPreviewList_StripsTerminalEscapes: everything a preview row prints
// that Forge did not write — the manifest's service names, a process's failure
// text, the daemon's notes assembled out of both, the bead id itself — goes
// through termtext.Line first. A manifest is merged code from a previewed repo
// and a preview's bead id is an arbitrary registry key anyone who can reach the
// daemon may choose, so an escape sequence in either would otherwise be free to
// rewrite the operator's terminal: overwrite the status column, spoof a URL, or
// set the window title.
func TestRenderPreviewList_StripsTerminalEscapes(t *testing.T) {
	var buf bytes.Buffer
	renderPreviewList(&buf, ipc.PreviewListResponse{
		Enabled: true,
		Previews: []ipc.PreviewInfo{
			{
				BeadID:    "Forge-\x1b]0;pwn\x07abc1",
				Status:    "degraded",
				EntryNote: "entry service \x1b[31m\"web\"\x1b[0m is not serving",
				Services: []ipc.PreviewServiceInfo{
					{
						Name:   "we\x1b[2Kb",
						Port:   41000,
						Entry:  true,
						Health: state.PreviewServiceFailed,
						Error:  "spawn failed: \x1b]0;pwn\x07npm run dev",
					},
				},
				ResourceNote: "1 service, port \x1b[1m41000",
			},
		},
	})

	got := buf.String()
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("escape sequences reached the terminal:\n%q", got)
	}
	for _, want := range []string{
		`entry service "web" is not serving`,
		"Forge-abc1/web: spawn failed: npm run dev",
		"1 service, port 41000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// An exit the daemon recorded without error text still gets a line: "this was
// working and is now dead" is the fact worth printing, with or without prose.
func TestPreviewServiceIssue(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	code := 2
	tests := []struct {
		name string
		svc  ipc.PreviewServiceInfo
		want string
	}{
		{name: "healthy", svc: ipc.PreviewServiceInfo{Health: state.PreviewServiceHealthy}},
		{name: "starting", svc: ipc.PreviewServiceInfo{Health: state.PreviewServiceStarting}},
		{
			name: "failed keeps its recorded error",
			svc:  ipc.PreviewServiceInfo{Health: state.PreviewServiceFailed, Error: "health check timed out"},
			want: "health check timed out",
		},
		{
			name: "exited without prose is rendered from its fields",
			svc: ipc.PreviewServiceInfo{
				Health:    state.PreviewServiceExited,
				StartedAt: started,
				ExitedAt:  started.Add(90 * time.Second),
				ExitCode:  &code,
			},
			want: "exited (exit 2, lived 1m30s)",
		},
		{
			// A record with an exit but no spawn time (a version-skewed row)
			// has no interval to report. Subtracting anyway would measure from
			// the zero time and print `lived 17700000h` — an invented
			// measurement, which is exactly what state.PreviewService.Lifetime
			// exists to refuse.
			name: "exited with no spawn time reports no lifetime",
			svc: ipc.PreviewServiceInfo{
				Health:   state.PreviewServiceExited,
				ExitedAt: started.Add(90 * time.Second),
				ExitCode: &code,
			},
			want: "exited (exit 2)",
		},
		{
			// A service that is up again only because Kiln relaunched it reads
			// as plain `healthy` everywhere else. The count is the whole
			// evidence that it flapped, so the CLI prints a line for it even
			// though nothing is currently wrong.
			name: "healthy after a restart still reports",
			svc: ipc.PreviewServiceInfo{
				Health:   state.PreviewServiceHealthy,
				Restarts: 1,
			},
			want: "healthy [1 restart]",
		},
		{
			name: "an exit that spent restarts carries the count",
			svc: ipc.PreviewServiceInfo{
				Health:    state.PreviewServiceExited,
				StartedAt: started,
				ExitedAt:  started.Add(90 * time.Second),
				ExitCode:  &code,
				Restarts:  3,
			},
			want: "exited (exit 2, lived 1m30s) [3 restarts]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := previewServiceIssue(tt.svc); got != tt.want {
				t.Errorf("previewServiceIssue = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPreviewURLCell(t *testing.T) {
	tests := []struct {
		name string
		info ipc.PreviewInfo
		want string
	}{
		{name: "still coming up shows a dash", want: "-"},
		{
			name: "a live preview shows its link",
			info: ipc.PreviewInfo{EntryURL: "http://localhost:41000"},
			want: "http://localhost:41000",
		},
		{
			name: "a withheld link shows why",
			info: ipc.PreviewInfo{EntryNote: "entry service \"web\" is not serving: exited (exit 1)"},
			want: "(entry service \"web\" is not serving: exited (exit 1))",
		},
		{
			// The note is built from a manifest's service name and a process's
			// own failure text, neither of which Forge wrote — so an escape
			// sequence in either must not reach the terminal, where it could
			// rewrite the status and URL printed around it.
			name: "escape sequences in the note are stripped",
			info: ipc.PreviewInfo{EntryNote: "entry service \x1b[2K\x1b]0;pwn\x07\"web\" is not serving"},
			want: "(entry service \"web\" is not serving)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := previewURLCell(tt.info); got != tt.want {
				t.Errorf("previewURLCell = %q, want %q", got, tt.want)
			}
		})
	}
}
