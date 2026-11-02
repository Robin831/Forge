package executil

import (
	"context"
	"strings"
	"testing"
)

func TestVardeWritesRouteOnlyWrites(t *testing.T) {
	SetVardeWrites("/opt/forge", "/etc/forge.yaml")
	defer SetVardeWrites("", "")
	write, cancel := BdCommand(context.Background(), "update", "B-1", "--append-notes", "x")
	defer cancel()
	if got := strings.Join(write.Args, " "); got != "/opt/forge --config /etc/forge.yaml bd-varde -- update B-1 --append-notes x" {
		t.Errorf("a write runs %q", got)
	}
	read, cancel2 := BdCommand(context.Background(), "show", "B-1", "--json")
	defer cancel2()
	if read.Args[0] != "bd" {
		t.Errorf("a read runs %q, want bd", read.Args)
	}
	SetVardeWrites("", "")
	off, cancel3 := BdCommand(context.Background(), "close", "B-1")
	defer cancel3()
	if off.Args[0] != "bd" {
		t.Errorf("switched off, a write runs %q", off.Args)
	}
}
