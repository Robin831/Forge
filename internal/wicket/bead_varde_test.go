package wicket

import (
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/vardebeads"
)

// Wicket's create passes the Varde translation (DevInfra-gbx.8): no flag Varde would refuse.
func TestBuildBDArgsTranslateThroughVarde(t *testing.T) {
	t.Setenv(vardebeads.EnvURL, "https://varde.invalid")
	t.Setenv(vardebeads.EnvKeyFile, "/nowhere")
	args := append([]string{"create"}, buildBDArgs(TriageDecision{BeadTitle: "t"}, Issue{Repo: "o/r", Number: 1}, 2, "munin")...)
	if strings.Contains(strings.Join(args, " "), "--metadata") {
		t.Errorf("metadata sent through Varde: %v", args)
	}
	if _, err := vardebeads.Translate(args, "Forge", "EP-1"); err != nil {
		t.Errorf("Translate(%v): %v", args, err)
	}
}
