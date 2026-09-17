package daemon

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

func TestWarnAssayProviderConflictsLogsOncePerCondition(t *testing.T) {
	var buf bytes.Buffer
	d := &Daemon{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	enabled := true
	cfg := &config.Config{
		Assay: config.AssayConfig{Enabled: &enabled, TriageProvider: "claude"},
		Anvils: map[string]config.AnvilConfig{
			"api": {Path: "/a", StageProviders: map[string][]string{"assay": {"gemini"}}},
		},
	}

	d.warnAssayProviderConflicts(cfg)
	d.warnAssayProviderConflicts(cfg) // an unchanged reload stays quiet
	if n := strings.Count(buf.String(), "Assay provider keys overlap"); n != 1 {
		t.Fatalf("logged %d times, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "stage_providers wins for triage") {
		t.Errorf("warning does not name the winner:\n%s", buf.String())
	}

	// An edit that changes what wins is news again.
	a := cfg.Anvils["api"]
	a.StageProviders = map[string][]string{"assay.logic": {"gemini"}}
	cfg.Anvils["api"] = a
	d.warnAssayProviderConflicts(cfg)
	if n := strings.Count(buf.String(), "Assay provider keys overlap"); n != 2 {
		t.Fatalf("logged %d times after the edit, want 2:\n%s", n, buf.String())
	}
}
