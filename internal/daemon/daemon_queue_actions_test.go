package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/queueactions"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newQueueActionDaemon builds a minimal Daemon backed by a fresh sqlite
// state.db. forgeID, when non-empty, is wired through SettingsConfig.ForgeID
// so queueActionsHandle().LocalForgeID() returns the same value the test
// passes on the IPC payload.
func newQueueActionDaemon(t *testing.T, forgeID string) (*Daemon, *state.DB) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "forge-queue-action-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{
		db:         db,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx:     context.Background(),
		reqTracker: *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{
		Settings: config.SettingsConfig{ForgeID: forgeID},
	})
	return d, db
}

// stubBd installs a fake `bd` executable on PATH for the duration of the test.
// It records every invocation's arguments (one line per call) to the returned
// log path and exits 0 on `update`, letting the async claim-release goroutine
// succeed so tests can assert the exact `bd update ... --status=open
// --assignee=` arguments the stop verbs issue.
func stubBd(t *testing.T, dir string) (bdLog string) {
	t.Helper()
	bdLog = filepath.Join(dir, "bd-args.log")
	var bdScript, bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(dir, "bd.bat")
		bdContent = "@echo off\r\necho %*>>\"" + bdLog + "\"\r\nif \"%1\"==\"update\" (\r\n  echo {\"id\":\"%2\",\"status\":\"open\"}\r\n  exit /b 0\r\n)\r\nexit /b 1\r\n"
	} else {
		bdScript = filepath.Join(dir, "bd")
		bdContent = "#!/bin/sh\necho \"$@\" >> \"" + bdLog + "\"\nif [ \"$1\" = \"update\" ]; then\n  echo '{\"id\":\"'\"$2\"'\",\"status\":\"open\"}'\n  exit 0\nfi\nexit 1\n"
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	return bdLog
}

// waitAsyncDone blocks until every queued async request has completed, so a
// test can safely read side effects the async goroutine produced (e.g. the bd
// claim-release log).
func waitAsyncDone(t *testing.T, d *Daemon) {
	t.Helper()
	require.Eventually(t, func() bool {
		return d.reqTracker.Pending() == 0
	}, 5*time.Second, 10*time.Millisecond, "async goroutine never completed")
}

// expectEvent asserts that an event of the given type was logged for
// (beadID, anvil) and that its message contains the supplied note substring.
func expectEvent(t *testing.T, db *state.DB, wantType state.EventType, beadID, anvil, noteSubstr string) {
	t.Helper()
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	for _, e := range events {
		if e.Type != wantType || e.BeadID != beadID || e.Anvil != anvil {
			continue
		}
		if noteSubstr != "" && !strings.Contains(e.Message, noteSubstr) {
			t.Fatalf("event found but message %q does not contain note %q", e.Message, noteSubstr)
		}
		return
	}
	t.Fatalf("did not find %s event for bead=%s anvil=%s", wantType, beadID, anvil)
}

// unmarshalErrorMessage extracts the "message" field from an error response.
func unmarshalErrorMessage(t *testing.T, resp ipc.Response) string {
	t.Helper()
	require.Equal(t, "error", resp.Type)
	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	return msg["message"]
}

// ---------- queue_clarify ----------

func TestHandleIPC_QueueClarify(t *testing.T) {
	t.Run("invalid JSON payload", func(t *testing.T) {
		d, _ := newQueueActionDaemon(t, "forge-a")
		resp := d.handleIPC(ipc.Command{Type: "queue_clarify", Payload: []byte("not-json")})
		assert.Contains(t, unmarshalErrorMessage(t, resp), "invalid queue_clarify payload")
	})

	t.Run("missing bead_id is rejected", func(t *testing.T) {
		d, _ := newQueueActionDaemon(t, "forge-a")
		payload, _ := json.Marshal(ipc.QueueActionPayload{AnvilName: "anvil-1", Note: "x"})
		resp := d.handleIPC(ipc.Command{Type: "queue_clarify", Payload: payload})
		assert.Contains(t, unmarshalErrorMessage(t, resp), "bead_id and anvil are required")
	})

	t.Run("missing note is rejected", func(t *testing.T) {
		d, _ := newQueueActionDaemon(t, "forge-a")
		payload, _ := json.Marshal(ipc.QueueActionPayload{BeadID: "BD-1", AnvilName: "anvil-1"})
		resp := d.handleIPC(ipc.Command{Type: "queue_clarify", Payload: payload})
		assert.Contains(t, unmarshalErrorMessage(t, resp), "reason is required")
	})

	t.Run("happy path writes clarification, event, and note", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-CLARIFY",
			AnvilName: "anvil-1",
			Note:      "which auth lib?",
			ForgeID:   "forge-a",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_clarify", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		r, err := db.GetRetry("BD-CLARIFY", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.True(t, r.ClarificationNeeded, "clarification_needed flag should be set")
		// SetClarificationNeeded stores the reason in last_error.
		assert.Equal(t, "which auth lib?", r.LastError)

		expectEvent(t, db, state.EventClarificationNeeded, "BD-CLARIFY", "anvil-1", "which auth lib?")
	})

	t.Run("forge_id mismatch is rejected with no state mutation", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-MISMATCH",
			AnvilName: "anvil-1",
			Note:      "any",
			ForgeID:   "forge-b",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_clarify", Payload: payload})
		assert.Contains(t, unmarshalErrorMessage(t, resp), queueactions.ErrForgeMismatch.Error())

		r, _ := db.GetRetry("BD-MISMATCH", "anvil-1")
		assert.Nil(t, r, "no retry row should be created on forge mismatch")
		events, _ := db.RecentEvents(50)
		for _, e := range events {
			if e.BeadID == "BD-MISMATCH" {
				t.Fatalf("no event should be logged on forge mismatch; got %+v", e)
			}
		}
	})
}

// ---------- queue_unclarify ----------

func TestHandleIPC_QueueUnclarify(t *testing.T) {
	t.Run("happy path clears flag and logs event with note", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		// Seed an existing clarification row to verify it is cleared.
		require.NoError(t, db.SetClarificationNeeded("BD-UNCLR", "anvil-1", true, "old reason"))

		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-UNCLR",
			AnvilName: "anvil-1",
			Note:      "resolved offline",
			ForgeID:   "forge-a",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_unclarify", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		r, err := db.GetRetry("BD-UNCLR", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.False(t, r.ClarificationNeeded)

		expectEvent(t, db, state.EventClarificationCleared, "BD-UNCLR", "anvil-1", "resolved offline")
	})

	t.Run("note is optional", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		// SetClarificationNeeded(false) requires an existing retry row;
		// seed one so the test isolates the "note is optional" behaviour.
		require.NoError(t, db.SetClarificationNeeded("BD-UNCLR-2", "anvil-1", true, "old reason"))

		payload, _ := json.Marshal(ipc.QueueActionPayload{BeadID: "BD-UNCLR-2", AnvilName: "anvil-1"})
		resp := d.handleIPC(ipc.Command{Type: "queue_unclarify", Payload: payload})
		assert.Equal(t, "ok", resp.Type)
		expectEvent(t, db, state.EventClarificationCleared, "BD-UNCLR-2", "anvil-1", "")
	})

	t.Run("forge_id mismatch is rejected", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-UNCLR-3",
			AnvilName: "anvil-1",
			ForgeID:   "forge-b",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_unclarify", Payload: payload})
		assert.Contains(t, unmarshalErrorMessage(t, resp), queueactions.ErrForgeMismatch.Error())

		// No retry row should be touched.
		r, _ := db.GetRetry("BD-UNCLR-3", "anvil-1")
		assert.Nil(t, r)
	})
}

// ---------- queue_retry ----------

func TestHandleIPC_QueueRetry(t *testing.T) {
	t.Run("happy path with circuit breaker resets and logs event with note", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		_, broke, err := db.IncrementDispatchFailures("BD-RETRY", "anvil-1", 1, "boom")
		require.NoError(t, err)
		require.True(t, broke, "expected circuit breaker to trip")

		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-RETRY",
			AnvilName: "anvil-1",
			Note:      "manual retry",
			ForgeID:   "forge-a",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_retry", Payload: payload})
		assert.Equal(t, "ok", resp.Type)
		var data map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &data))
		assert.Equal(t, "retry state reset", data["message"], "circuit-breaker path should report state-reset wording")

		r, err := db.GetRetry("BD-RETRY", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.Equal(t, 0, r.DispatchFailures)
		assert.False(t, r.NeedsHuman)

		expectEvent(t, db, state.EventRetryReset, "BD-RETRY", "anvil-1", "manual retry")
	})

	t.Run("happy path without circuit breaker reports plain retry reset", func(t *testing.T) {
		d, _ := newQueueActionDaemon(t, "forge-a")
		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-RETRY-2",
			AnvilName: "anvil-1",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_retry", Payload: payload})
		assert.Equal(t, "ok", resp.Type)
		var data map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &data))
		assert.Equal(t, "retry reset", data["message"])
	})

	t.Run("forge_id mismatch is rejected with no reset", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		_, _, err := db.IncrementDispatchFailures("BD-RETRY-MM", "anvil-1", 1, "boom")
		require.NoError(t, err)

		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-RETRY-MM",
			AnvilName: "anvil-1",
			ForgeID:   "forge-b",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_retry", Payload: payload})
		assert.Contains(t, unmarshalErrorMessage(t, resp), queueactions.ErrForgeMismatch.Error())

		r, err := db.GetRetry("BD-RETRY-MM", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.Greater(t, r.DispatchFailures, 0, "dispatch failures should not be cleared on mismatch")
	})
}

// ---------- queue_clear ----------

func TestHandleIPC_QueueClear(t *testing.T) {
	t.Run("happy path clears flags and logs event with note", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		require.NoError(t, db.UpsertRetry(&state.RetryRecord{
			BeadID:           "BD-CLEAR",
			Anvil:            "anvil-1",
			NeedsHuman:       true,
			DispatchFailures: 3,
			RecoveryFailures: 1,
			RetryCount:       5,
			LastError:        "boom",
		}))

		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-CLEAR",
			AnvilName: "anvil-1",
			Note:      "pr merged",
			ForgeID:   "forge-a",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_clear", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		r, err := db.GetRetry("BD-CLEAR", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.False(t, r.NeedsHuman)
		assert.Equal(t, 0, r.DispatchFailures)
		assert.Equal(t, 0, r.RecoveryFailures)
		assert.Empty(t, r.LastError)

		expectEvent(t, db, state.EventRetryCleared, "BD-CLEAR", "anvil-1", "pr merged")
	})

	t.Run("forge_id mismatch is rejected", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		require.NoError(t, db.UpsertRetry(&state.RetryRecord{
			BeadID:           "BD-CLEAR-MM",
			Anvil:            "anvil-1",
			NeedsHuman:       true,
			DispatchFailures: 2,
		}))
		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-CLEAR-MM",
			AnvilName: "anvil-1",
			ForgeID:   "forge-b",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_clear", Payload: payload})
		assert.Contains(t, unmarshalErrorMessage(t, resp), queueactions.ErrForgeMismatch.Error())

		r, err := db.GetRetry("BD-CLEAR-MM", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.True(t, r.NeedsHuman, "needs_human flag should remain on forge mismatch")
		assert.Equal(t, 2, r.DispatchFailures)
	})
}

// ---------- queue_stop ----------

// configureAnvil registers a single anvil pointing at dir so the stop verbs can
// resolve a path to shell out `bd` in. Returns the daemon and its state DB.
func configureStopDaemon(t *testing.T, forgeID, anvil, dir string) (*Daemon, *state.DB) {
	t.Helper()
	d, db := newQueueActionDaemon(t, forgeID)
	cfg := d.cfg.Load()
	updated := *cfg
	updated.Anvils = map[string]config.AnvilConfig{anvil: {Path: dir}}
	d.cfg.Store(&updated)
	return d, db
}

func TestHandleIPC_QueueStop(t *testing.T) {
	t.Run("happy path releases claim, sets clarification, frees activeBeads slot, logs event with note", func(t *testing.T) {
		tmpDir := t.TempDir()
		bdLog := stubBd(t, tmpDir)
		d, db := configureStopDaemon(t, "forge-a", "anvil-1", tmpDir)
		d.activeBeads.Store("BD-STOP", struct{}{})

		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-STOP",
			AnvilName: "anvil-1",
			Note:      "wrong approach",
			ForgeID:   "forge-a",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_stop", Payload: payload})
		// The bd claim-release is async, so the verb acknowledges with "queued".
		assert.Equal(t, "queued", resp.Type)

		// These mutations happen synchronously, before the release goroutine.
		r, err := db.GetRetry("BD-STOP", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.True(t, r.ClarificationNeeded)
		// SetClarificationNeeded stores the reason in last_error.
		assert.Equal(t, "wrong approach", r.LastError)

		_, stillActive := d.activeBeads.Load("BD-STOP")
		assert.False(t, stillActive, "activeBeads slot should be freed")

		expectEvent(t, db, state.EventBeadStopped, "BD-STOP", "anvil-1", "wrong approach")

		// The web GUI uses queue_stop; the fix is that it now releases the bd
		// claim (status=open, assignee cleared) so `bd ready` sees the bead again.
		waitAsyncDone(t, d)
		logged := readFileString(t, bdLog)
		assert.Contains(t, logged, "update BD-STOP", "bd update should target the bead")
		assert.Contains(t, logged, "--status=open", "stop must return the bead to open")
		assert.Contains(t, logged, "--assignee=", "stop must clear the assignee")
	})

	t.Run("missing note falls back to default reason", func(t *testing.T) {
		tmpDir := t.TempDir()
		stubBd(t, tmpDir)
		d, db := configureStopDaemon(t, "forge-a", "anvil-1", tmpDir)
		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-STOP-2",
			AnvilName: "anvil-1",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_stop", Payload: payload})
		assert.Equal(t, "queued", resp.Type)
		r, err := db.GetRetry("BD-STOP-2", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.Equal(t, "manually stopped", r.LastError)
		waitAsyncDone(t, d)
	})

	t.Run("forge_id mismatch is rejected with no state mutation", func(t *testing.T) {
		tmpDir := t.TempDir()
		stubBd(t, tmpDir)
		d, db := configureStopDaemon(t, "forge-a", "anvil-1", tmpDir)
		d.activeBeads.Store("BD-STOP-MM", struct{}{})

		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-STOP-MM",
			AnvilName: "anvil-1",
			Note:      "x",
			ForgeID:   "forge-b",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_stop", Payload: payload})
		assert.Contains(t, unmarshalErrorMessage(t, resp), queueactions.ErrForgeMismatch.Error())

		r, _ := db.GetRetry("BD-STOP-MM", "anvil-1")
		assert.Nil(t, r, "no retry row should be created on forge mismatch")

		_, stillActive := d.activeBeads.Load("BD-STOP-MM")
		assert.True(t, stillActive, "activeBeads slot must not be freed on mismatch")
	})

	t.Run("unknown anvil is rejected", func(t *testing.T) {
		d, _ := newQueueActionDaemon(t, "forge-a")
		payload, _ := json.Marshal(ipc.QueueActionPayload{
			BeadID:    "BD-STOP-NA",
			AnvilName: "no-such-anvil",
		})
		resp := d.handleIPC(ipc.Command{Type: "queue_stop", Payload: payload})
		assert.Contains(t, unmarshalErrorMessage(t, resp), "not found")
	})
}

// TestStopVerbsReleaseClaim asserts the core consolidation guarantee: both the
// stop_bead (CLI/Hearth) and queue_stop (web GUI) verbs run one implementation
// that releases the bd claim — status=open with the assignee cleared — so a
// bead stopped from either surface reappears in `bd ready`.
func TestStopVerbsReleaseClaim(t *testing.T) {
	cases := []struct {
		name    string
		command func(beadID string) ipc.Command
	}{
		{
			name: "stop_bead",
			command: func(beadID string) ipc.Command {
				payload, _ := json.Marshal(ipc.StopBeadPayload{BeadID: beadID, Anvil: "anvil-1", Reason: "done"})
				return ipc.Command{Type: "stop_bead", Payload: payload}
			},
		},
		{
			name: "queue_stop",
			command: func(beadID string) ipc.Command {
				payload, _ := json.Marshal(ipc.QueueActionPayload{BeadID: beadID, AnvilName: "anvil-1", Note: "done"})
				return ipc.Command{Type: "queue_stop", Payload: payload}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			bdLog := stubBd(t, tmpDir)
			d, db := configureStopDaemon(t, "", "anvil-1", tmpDir)

			beadID := "BD-" + tc.name
			resp := d.handleIPC(tc.command(beadID))
			assert.Equal(t, "queued", resp.Type)

			// Bead is marked stopped (clarification) synchronously.
			r, err := db.GetRetry(beadID, "anvil-1")
			require.NoError(t, err)
			require.NotNil(t, r)
			assert.True(t, r.ClarificationNeeded)

			waitAsyncDone(t, d)
			logged := readFileString(t, bdLog)
			assert.Contains(t, logged, "update "+beadID)
			assert.Contains(t, logged, "--status=open")
			assert.Contains(t, logged, "--assignee=")
		})
	}
}

// readFileString reads a file created by the fake bd stub. The stub appends on
// every call, so the file always exists by the time the async request settles.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// TestErrorResponseWireFormat asserts errorResponse produces byte-identical
// output to the inline json.Marshal envelope it replaced across the IPC switch,
// so the mechanical sweep did not change the wire format any surface unwraps.
func TestErrorResponseWireFormat(t *testing.T) {
	for _, msg := range []string{"boom", "anvil \"x\" not found", ""} {
		got := errorResponse(msg)
		assert.Equal(t, "error", got.Type)
		want, err := json.Marshal(map[string]string{"message": msg})
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got.Payload), "error envelope must be byte-identical to inline marshal")
	}
}

// TestOkResponseWireFormat asserts okResponse produces byte-identical output to
// the inline json.Marshal + ipc.Response{Type:"ok"} construction it replaced.
func TestOkResponseWireFormat(t *testing.T) {
	payloads := []any{
		map[string]string{"message": "bead X stopped"},
		map[string]string{"killed": "worker-7"},
		ipc.QueueResponse{},
	}
	for _, p := range payloads {
		got := okResponse(p)
		assert.Equal(t, "ok", got.Type)
		want, err := json.Marshal(p)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got.Payload), "ok envelope must be byte-identical to inline marshal")
	}
}
