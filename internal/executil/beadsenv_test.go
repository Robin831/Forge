package executil

import (
	"reflect"
	"testing"
)

func TestWithoutBeadsCredentials(t *testing.T) {
	t.Setenv("FORGE_VARDE_URL", "http://varde.invalid")
	t.Setenv("FORGE_VARDE_KEY_FILE", "/nowhere")
	in := []string{
		"PATH=/usr/bin",
		"BEADS_DOLT_PASSWORD=secret",
		"BEADS_DOLT_SERVER_USER=forge",
		"BEADS_DOLT_SERVER_HOST=dolt",
		"DOLT_REMOTE_PASSWORD=secret",
		"BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD=secret",
		"BEADS_CREDENTIALS_FILE=/home/forge/.config/beads/credentials",
		"beads_dolt_password=secret",
		"DOLT_REMOTE_USER=forge",
		"BEADS_ACTOR=Robin",
		"BD_DOLT_AUTO_PULL=false",
	}
	got := WithoutBeadsCredentials(in)
	want := []string{"PATH=/usr/bin", "DOLT_REMOTE_USER=forge", "BEADS_ACTOR=Robin", "BD_DOLT_AUTO_PULL=false"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WithoutBeadsCredentials() = %v, want %v", got, want)
	}
}

// A forge on plain bd keeps 0.37.2's behaviour: its agent sessions keep the beads environment (DevInfra-gbx.8).
func TestWithoutBeadsCredentialsKeepsThemOnPlainBd(t *testing.T) {
	in := []string{"PATH=/bin", "BEADS_DOLT_PASSWORD=x"}
	if got := WithoutBeadsCredentials(in); len(got) != 2 {
		t.Errorf("plain bd: got %v, want the environment as is", got)
	}
}
