//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
)

// startFakeDaemon runs an IPC server answering with the given handler, on a
// socket inside a temp HOME so it can never collide with a real daemon. It
// returns once the socket is accepting connections.
//
// Windows is excluded by the build tag: there the transport is a fixed-name
// named pipe (\\.\pipe\forge), so a test server would hijack the developer's
// own daemon rather than run beside it.
func startFakeDaemon(t *testing.T, handler func(ipc.Command) ipc.Response) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	srv := ipc.NewServer()
	srv.OnCommand(handler)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		<-done
	})

	deadline := time.Now().Add(5 * time.Second)
	for !ipc.SocketExists() {
		if time.Now().After(deadline) {
			t.Fatal("fake daemon socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func okResp(t *testing.T, v any) ipc.Response {
	t.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return ipc.Response{Type: "ok", Payload: payload}
}

func errResp(t *testing.T, message string) ipc.Response {
	t.Helper()
	resp := okResp(t, map[string]string{"message": message})
	resp.Type = "error"
	return resp
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()

	fn()

	os.Stdout = orig
	_ = w.Close()
	captured := <-out
	_ = r.Close()
	return captured
}

func TestPreviewStop_DaemonDown(t *testing.T) {
	// An empty HOME means no socket, which is what "the daemon is not running"
	// looks like from the CLI side.
	t.Setenv("HOME", t.TempDir())

	err := previewStopCmd.RunE(previewStopCmd, []string{"Forge-abc1"})
	if err == nil {
		t.Fatal("expected an error when the daemon is not running")
	}
	if !strings.Contains(err.Error(), "forge up") {
		t.Errorf("error should tell the operator to start the daemon, got %q", err)
	}
}

func TestPreviewList_DaemonDown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := previewListCmd.RunE(previewListCmd, nil)
	if err == nil {
		t.Fatal("expected an error when the daemon is not running")
	}
	if !strings.Contains(err.Error(), "forge up") {
		t.Errorf("error should tell the operator to start the daemon, got %q", err)
	}
}

func TestPreviewStop_NoPreviewForBead(t *testing.T) {
	startFakeDaemon(t, func(cmd ipc.Command) ipc.Response {
		if cmd.Type != "preview_stop" {
			return errResp(t, "unexpected command "+cmd.Type)
		}
		return errResp(t, "no preview running for bead Forge-abc1")
	})

	err := previewStopCmd.RunE(previewStopCmd, []string{"Forge-abc1"})
	if err == nil {
		t.Fatal("expected an error for a bead with no preview")
	}
	if !strings.Contains(err.Error(), "no preview running for bead Forge-abc1") {
		t.Errorf("expected the daemon's message to reach the operator, got %q", err)
	}
}

func TestPreviewStop_QueuedThenStopped(t *testing.T) {
	var statusCalls int
	startFakeDaemon(t, func(cmd ipc.Command) ipc.Response {
		switch cmd.Type {
		case "preview_stop":
			var p ipc.PreviewActionPayload
			if err := json.Unmarshal(cmd.Payload, &p); err != nil {
				return errResp(t, "bad payload")
			}
			if p.BeadID != "Forge-abc1" {
				return errResp(t, "unexpected bead "+p.BeadID)
			}
			resp, err := ipc.NewQueuedResponse("req-1", "stopping preview")
			if err != nil {
				return errResp(t, err.Error())
			}
			return resp
		case "request_status":
			statusCalls++
			// The first poll lands while teardown is still running.
			if statusCalls == 1 {
				return okResp(t, ipc.RequestStatusResponse{
					RequestID: "req-1",
					State:     ipc.RequestStatePending,
				})
			}
			return okResp(t, ipc.RequestStatusResponse{
				RequestID: "req-1",
				State:     ipc.RequestStateOK,
				Message:   "preview for Forge-abc1 stopped",
			})
		default:
			return errResp(t, "unexpected command "+cmd.Type)
		}
	})

	var err error
	out := captureStdout(t, func() {
		err = previewStopCmd.RunE(previewStopCmd, []string{"Forge-abc1"})
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if statusCalls < 2 {
		t.Errorf("expected the CLI to keep polling past the pending state, got %d call(s)", statusCalls)
	}
	if !strings.Contains(out, "preview for Forge-abc1 stopped") {
		t.Errorf("expected the daemon's confirmation on stdout, got %q", out)
	}
}

func TestPreviewStop_TeardownFails(t *testing.T) {
	startFakeDaemon(t, func(cmd ipc.Command) ipc.Response {
		switch cmd.Type {
		case "preview_stop":
			resp, err := ipc.NewQueuedResponse("req-2", "stopping preview")
			if err != nil {
				return errResp(t, err.Error())
			}
			return resp
		case "request_status":
			return okResp(t, ipc.RequestStatusResponse{
				RequestID: "req-2",
				State:     ipc.RequestStateError,
				Message:   "stopping preview for Forge-abc1 failed: teardown exited 1",
			})
		default:
			return errResp(t, "unexpected command "+cmd.Type)
		}
	})

	// A teardown that fails after being queued must not read as a success.
	err := previewStopCmd.RunE(previewStopCmd, []string{"Forge-abc1"})
	if err == nil {
		t.Fatal("expected an error when the queued teardown fails")
	}
	if !strings.Contains(err.Error(), "teardown exited 1") {
		t.Errorf("expected the daemon's failure detail, got %q", err)
	}
}

func TestPreviewStop_OutcomeEvicted(t *testing.T) {
	startFakeDaemon(t, func(cmd ipc.Command) ipc.Response {
		switch cmd.Type {
		case "preview_stop":
			resp, err := ipc.NewQueuedResponse("req-3", "stopping preview")
			if err != nil {
				return errResp(t, err.Error())
			}
			return resp
		case "request_status":
			return okResp(t, ipc.RequestStatusResponse{
				RequestID: "req-3",
				State:     ipc.RequestStateUnknown,
			})
		default:
			return errResp(t, "unexpected command "+cmd.Type)
		}
	})

	// An evicted record is not a success: the outcome is genuinely unknown.
	err := previewStopCmd.RunE(previewStopCmd, []string{"Forge-abc1"})
	if err == nil {
		t.Fatal("expected an error when the outcome record is gone")
	}
	if !strings.Contains(err.Error(), "lost track") {
		t.Errorf("expected an explicit unknown-outcome message, got %q", err)
	}
}

func TestPreviewList_RendersDaemonPayload(t *testing.T) {
	startFakeDaemon(t, func(cmd ipc.Command) ipc.Response {
		if cmd.Type != "preview_list" {
			return errResp(t, "unexpected command "+cmd.Type)
		}
		return okResp(t, ipc.PreviewListResponse{
			Enabled: true,
			Anvils:  []string{"forge"},
			Previews: []ipc.PreviewInfo{{
				BeadID:       "Forge-abc1",
				Anvil:        "forge",
				Status:       "running",
				EntryURL:     "http://localhost:41000",
				Port:         41000,
				ResourceNote: "1 service, ports 41000",
			}},
		})
	})

	var err error
	out := captureStdout(t, func() {
		err = previewListCmd.RunE(previewListCmd, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Forge-abc1") || !strings.Contains(out, "http://localhost:41000") {
		t.Errorf("expected the preview to be rendered, got %q", out)
	}
}

// TestPreviewList_PrintsTheProxyURL — with host-based routing configured the
// daemon reports the preview hostname as the entry URL, and the CLI prints
// that. The assertion that matters is the second one: the CLI must not assemble
// a link of its own out of the port that rides along in the same payload, or it
// would print a loopback address the operator cannot open while the dashboard
// prints the routable one.
func TestPreviewList_PrintsTheProxyURL(t *testing.T) {
	const proxyURL = "https://forge-abc1.preview.example.com/"
	startFakeDaemon(t, func(cmd ipc.Command) ipc.Response {
		if cmd.Type != "preview_list" {
			return errResp(t, "unexpected command "+cmd.Type)
		}
		return okResp(t, ipc.PreviewListResponse{
			Enabled:    true,
			Anvils:     []string{"forge"},
			PublicHost: "127.0.0.1",
			Previews: []ipc.PreviewInfo{{
				BeadID:       "Forge-abc1",
				Anvil:        "forge",
				Status:       "running",
				EntryURL:     proxyURL,
				Port:         41000,
				ResourceNote: "1 service, ports 41000",
			}},
		})
	})

	var err error
	out := captureStdout(t, func() {
		err = previewListCmd.RunE(previewListCmd, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, proxyURL) {
		t.Errorf("expected the proxy URL %q in the output, got %q", proxyURL, out)
	}
	if strings.Contains(out, "127.0.0.1:41000") || strings.Contains(out, "http://") {
		t.Errorf("the CLI must print the daemon's URL, not one built from the port: %q", out)
	}
}

func TestPreviewList_JSONEmitsRawPayload(t *testing.T) {
	startFakeDaemon(t, func(cmd ipc.Command) ipc.Response {
		return okResp(t, ipc.PreviewListResponse{
			Enabled:  true,
			Anvils:   []string{"forge"},
			Previews: []ipc.PreviewInfo{},
		})
	})

	orig := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = orig })

	var err error
	out := captureStdout(t, func() {
		err = previewListCmd.RunE(previewListCmd, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded ipc.PreviewListResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON (%v): %q", err, out)
	}
	if !decoded.Enabled || len(decoded.Anvils) != 1 || decoded.Anvils[0] != "forge" {
		t.Errorf("--json must pass the daemon payload through untouched, got %+v", decoded)
	}
}
