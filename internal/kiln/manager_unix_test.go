//go:build !windows

package kiln

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestManagerRunsManifestLifecycleCommands is the end-to-end shape of the
// setup/teardown contract: both commands really run, in the preview worktree,
// with the FORGE_* context, in the right order relative to the services. It is
// Unix-only because the commands are a shell script.
func TestManagerRunsManifestLifecycleCommands(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2, CommandTimeout: 30 * time.Second})
	trace := filepath.Join(t.TempDir(), "trace")
	h.manifest.Setup = fmt.Sprintf(`echo "setup $FORGE_PREVIEW_ID $FORGE_BEAD_ID $(pwd)" >> %q`, trace)
	h.manifest.Teardown = fmt.Sprintf(`echo "teardown $FORGE_PREVIEW_ID" >> %q`, trace)

	env, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Setup must have run before the runtime reported the preview up.
	lines := readLines(t, trace)
	if len(lines) != 1 {
		t.Fatalf("trace after Start = %v, want exactly the setup line", lines)
	}
	wantPrefix := "setup forge_aaa1 Forge-aaa1 "
	if !strings.HasPrefix(lines[0], wantPrefix) {
		t.Errorf("setup line = %q, want prefix %q", lines[0], wantPrefix)
	}
	if cwd := strings.TrimPrefix(lines[0], wantPrefix); !sameDir(cwd, env.WorktreePath) {
		t.Errorf("setup ran in %q, want the preview worktree %q", cwd, env.WorktreePath)
	}

	if err := h.mgr.Stop(context.Background(), "Forge-aaa1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	lines = readLines(t, trace)
	if len(lines) != 2 || lines[1] != "teardown forge_aaa1" {
		t.Fatalf("trace after Stop = %v, want the teardown line appended", lines)
	}
}

// TestManagerExpandsBindHostInTeardown proves ManagerConfig.BindHost reaches
// the teardown command, so a cleanup script sees the same address the services
// were told to listen on rather than the loopback default.
func TestManagerExpandsBindHostInTeardown(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2, CommandTimeout: 30 * time.Second, BindHost: "0.0.0.0"})
	trace := filepath.Join(t.TempDir(), "trace")
	h.manifest.Teardown = fmt.Sprintf(`echo "teardown {{.BindHost}}" >> %q`, trace)

	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.mgr.Stop(context.Background(), "Forge-aaa1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	lines := readLines(t, trace)
	if len(lines) != 1 || lines[0] != "teardown 0.0.0.0" {
		t.Fatalf("trace = %v, want the teardown line with the configured bind host", lines)
	}
}

// TestManagerStartRollsBackOnSetupFailure covers the unwind path a failing
// setup script triggers: no services, no worktree, no row, no cap slot held.
func TestManagerStartRollsBackOnSetupFailure(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2, CommandTimeout: 30 * time.Second})
	h.manifest.Setup = "echo 'no database for you' >&2; exit 7"
	// Teardown still runs on the unwind, so whatever setup half-created is
	// still cleaned up.
	h.manifest.Teardown = "true"

	_, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err == nil {
		t.Fatal("Start succeeded despite a failing setup command")
	}
	if !strings.Contains(err.Error(), "setup") {
		t.Errorf("error %q does not name the setup command", err.Error())
	}
	if !strings.Contains(err.Error(), "no database for you") {
		t.Errorf("error %q does not carry the command's output", err.Error())
	}
	assertFullyUnwound(t, h, "Forge-aaa1")
}

// TestManagerStopCompletesDespiteTeardownFailure: a project's cleanup script
// exiting non-zero is reported, not obeyed.
func TestManagerStopCompletesDespiteTeardownFailure(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2, CommandTimeout: 30 * time.Second})
	h.manifest.Teardown = "exit 3"

	env, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = h.mgr.Stop(context.Background(), "Forge-aaa1")
	if err == nil {
		t.Fatal("Stop hid the teardown failure")
	}
	if !strings.Contains(err.Error(), "teardown") {
		t.Errorf("error %q does not name the teardown command", err.Error())
	}
	if _, err := os.Stat(env.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("a failing teardown command stranded the worktree: %v", err)
	}
	if _, ok := h.mgr.Get("Forge-aaa1"); ok {
		t.Error("a failing teardown command stranded the registry entry")
	}
	if row, _ := h.store.GetPreview("Forge-aaa1"); row != nil {
		t.Errorf("a failing teardown command stranded the state row: %+v", row)
	}
}

// TestManagerTeardownRunsOnACancelledContext: stopping because the daemon is
// shutting down must still release the preview's external resources.
func TestManagerTeardownRunsOnACancelledContext(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2, CommandTimeout: 30 * time.Second})
	marker := filepath.Join(t.TempDir(), "teardown-ran")
	h.manifest.Teardown = fmt.Sprintf("touch %q", marker)

	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1")); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.mgr.Stop(ctx, "Forge-aaa1"); err != nil {
		t.Fatalf("Stop with a cancelled context: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("teardown did not run on a cancelled context: %v", err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// sameDir compares two paths after resolving symlinks (macOS hands out
// /var/folders temp dirs that are really /private/var/folders).
func sameDir(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = a
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = b
	}
	return ra == rb
}
