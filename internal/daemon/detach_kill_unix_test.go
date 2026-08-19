//go:build !windows

package daemon

import (
	"encoding/json"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// TestPRAction_DetachBellows_KillsLiveProcess is the half a PID-less worker row
// cannot exercise: killWorkerProcess short-circuits on pid <= 0 and merely
// marks the row failed, so a test seeded with PID 0 passes while the claude
// session keeps running and pushes the very commit detaching was meant to
// prevent. Here the row carries a real, live PID — the one quench, burnish and
// rebase now record alongside the log path — and the assertion is that the
// process is gone, not that a column changed.
func TestPRAction_DetachBellows_KillsLiveProcess(t *testing.T) {
	d, db := newDetachTestDaemon(t)
	prID := insertDetachPR(t, db, 71, "TEST-live")

	pid := startDetachVictim(t)
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-live-burnish", BeadID: "TEST-live", Anvil: "munin", Branch: "forge/TEST-live",
		Status: state.WorkerRunning, Phase: "burnish", PRNumber: 71, StartedAt: time.Now(),
	}))
	require.NoError(t, db.UpdateWorkerPID("w-live-burnish", pid))

	payload, _ := json.Marshal(ipc.PRActionPayload{
		Action: "detach_bellows", PRID: prID, PRNumber: 71, Anvil: "munin", BeadID: "TEST-live",
	})
	resp := d.handleIPC(ipc.Command{Type: "pr_action", Payload: payload})
	require.Equal(t, "ok", resp.Type)
	d.wg.Wait()

	assert.False(t, processAlive(pid), "detach must signal the fix worker's process, not just its row")

	killed, err := db.GetWorker("w-live-burnish")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerFailed, killed.Status)
}

// startDetachVictim spawns a long-lived child in its OWN process group and
// returns its PID. The group matters: killWorkerProcess signals the group when
// it can resolve one, and a child inheriting the test binary's group would take
// `go test` down with it. A goroutine reaps the child so it does not linger as
// a zombie, which kill(pid, 0) — and therefore processAlive — still reports as
// running.
func startDetachVictim(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start(), "could not start the victim process")

	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
		}
	})
	return cmd.Process.Pid
}
