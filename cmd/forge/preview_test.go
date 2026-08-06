package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
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
