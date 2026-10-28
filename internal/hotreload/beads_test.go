package hotreload

import (
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

// A beads-only edit must be reported, or reload() never stores it and the next
// bd create still reads the old parent.
func TestApplyChanges_BeadsOnly(t *testing.T) {
	mk := func(parent string) *config.Config {
		return &config.Config{Anvils: map[string]config.AnvilConfig{
			"heimdall": {Path: "/p", Beads: config.BeadsConfig{DefaultParent: parent}},
		}}
	}
	if ch := applyChanges(mk(""), mk("Heimdall-6tyq")); len(ch) == 0 {
		t.Error("a beads.default_parent edit must be detected")
	}
	if ch := applyChanges(mk("Heimdall-6tyq"), mk("Heimdall-6tyq")); len(ch) != 0 {
		t.Errorf("an unchanged parent is no change, got %v", ch)
	}
}
