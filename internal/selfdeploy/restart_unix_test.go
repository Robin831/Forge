//go:build !windows

package selfdeploy

import "testing"

// TestRestart_SetsidDetach is the second half of the cgroup-escape guarantee:
// without Setsid the restart child stays in the daemon's session and process
// group, so a group-directed signal aimed at the daemon takes the restart down
// with it.
func TestRestart_SetsidDetach(t *testing.T) {
	s := testRestarter(&recorder{}, true, "", nil)
	cmd, _ := s.buildRestartCmd(sampleRequest())

	if cmd.SysProcAttr == nil {
		t.Fatal("restart command has no SysProcAttr: it is not detached")
	}
	if !cmd.SysProcAttr.Setsid {
		t.Error("restart command must be started with Setsid so it leaves the daemon's session")
	}
	if cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid conflicts with Setsid; setsid already makes the child a group leader")
	}
}
