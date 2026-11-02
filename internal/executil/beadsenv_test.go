package executil

import (
	"reflect"
	"testing"
)

func TestWithoutBeadsCredentials(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"BEADS_DOLT_PASSWORD=secret",
		"BEADS_DOLT_SERVER_USER=forge",
		"BEADS_DOLT_SERVER_HOST=dolt",
		"DOLT_REMOTE_PASSWORD=secret",
		"BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD=secret",
		"BEADS_ACTOR=Robin",
		"BD_DOLT_AUTO_PULL=false",
	}
	got := WithoutBeadsCredentials(in)
	want := []string{"PATH=/usr/bin", "BEADS_ACTOR=Robin", "BD_DOLT_AUTO_PULL=false"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WithoutBeadsCredentials() = %v, want %v", got, want)
	}
}
