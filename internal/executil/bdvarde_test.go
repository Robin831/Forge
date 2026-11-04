package executil

import (
	"context"
	"strings"
	"testing"
)

func TestVardeRoutesEveryBdCommand(t *testing.T) {
	const prefix = "/opt/forge --config /etc/forge.yaml bd-varde --base /srv "
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a write", []string{"update", "B-1", "--append-notes", "x"}, prefix + "-- update B-1 --append-notes x"},
		{"a read", []string{"show", "B-1", "--json"}, prefix + "--read -- show B-1 --json"},
		{"the poll", []string{"ready", "--json", "--limit", "100"}, prefix + "--read -- ready --json --limit 100"},
		{"comments", []string{"comments", "B-1", "--json"}, prefix + "--read -- comments B-1 --json"},
		// Not a read Varde runs: bd-varde refuses it itself rather than a local bd running it (DevInfra-rjb.6).
		{"sql", []string{"sql", "--json", "SELECT 1"}, prefix + "--read -- sql --json SELECT 1"},
	}
	SetVardeWrites("/opt/forge", "/etc/forge.yaml", "/srv")
	defer SetVardeWrites("", "", "")
	for _, c := range cases {
		cmd, cancel := BdCommand(context.Background(), c.args...)
		if got := strings.Join(cmd.Args, " "); got != c.want {
			t.Errorf("%s runs %q, want %q", c.name, got, c.want)
		}
		cancel()
	}
}

func TestOutsideVardeEveryBdCommandIsBd(t *testing.T) {
	SetVardeWrites("", "", "")
	for _, args := range [][]string{{"close", "B-1"}, {"show", "B-1", "--json"}, {"sql", "--json", "SELECT 1"}} {
		cmd, cancel := BdCommand(context.Background(), args...)
		if cmd.Args[0] != "bd" || strings.Join(cmd.Args[1:], " ") != strings.Join(args, " ") {
			t.Errorf("switched off, %q runs %q", args, cmd.Args)
		}
		cancel()
	}
}
