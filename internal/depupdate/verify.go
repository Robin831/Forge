package depupdate

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/temper"
)

// VerifyGroup runs temper (build + lint + test) against the anvil to confirm
// that the group's updates haven't broken anything. Returns the temper result
// so the caller can decide to commit or rollback.
func VerifyGroup(ctx context.Context, anvilPath string, anvilConfig config.AnvilConfig) (*temper.Result, error) {
	raceEnabled := anvilConfig.GoRaceDetection != nil && *anvilConfig.GoRaceDetection
	detectOpts := temper.DetectOptionsFromAnvilFlag(anvilConfig.GolangciLint)

	cfg := temper.DefaultConfigWithRace(anvilPath, detectOpts, raceEnabled)
	if len(cfg.Steps) == 0 {
		return &temper.Result{Passed: true, Summary: "no verification steps detected"}, nil
	}

	// Pass nil for db/beadID/anvil since depupdate runs outside the normal
	// pipeline lifecycle and doesn't need event logging.
	result := temper.Run(ctx, anvilPath, cfg, nil, "", "")
	return result, nil
}

// RollbackGroup discards all uncommitted changes in the anvil directory by
// running `git checkout -- .`. It logs which group failed and why.
// Uses a fresh context with a 30-second timeout so that rollback still succeeds
// even when the caller's context has been cancelled.
func RollbackGroup(_ context.Context, anvilPath string, group UpdateGroup, reason error) error {
	log.Printf("depupdate: rolling back group %q (%s) — %v", group.Name, group.Kind, reason)

	rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(rollbackCtx, "git", "checkout", "--", "."))
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git checkout during rollback of group %q: %w\nstderr: %s", group.Name, err, stderr.String())
	}

	// Also clean any untracked files that the install may have created.
	cleanCmd := executil.HideWindow(exec.CommandContext(rollbackCtx, "git", "clean", "-fd"))
	cleanCmd.Dir = anvilPath

	var cleanStderr bytes.Buffer
	cleanCmd.Stderr = &cleanStderr

	if err := cleanCmd.Run(); err != nil {
		log.Printf("depupdate: git clean warning for group %q: %v", group.Name, err)
	}

	return nil
}

// CommitGroup stages all changes and creates a commit for the successfully
// installed group. The commit message follows the conventional format:
// "chore(deps): update <group-name> (<kind>)".
func CommitGroup(ctx context.Context, anvilPath string, group UpdateGroup) error {
	addCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "add", "-A"))
	addCmd.Dir = anvilPath

	var addStderr bytes.Buffer
	addCmd.Stderr = &addStderr

	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("git add for group %q: %w\nstderr: %s", group.Name, err, addStderr.String())
	}

	msg := fmt.Sprintf("chore(deps): update %s (%s)", group.Name, group.Kind)

	// Include individual package versions in the commit body for traceability.
	var body strings.Builder
	for _, u := range group.Updates {
		fmt.Fprintf(&body, "\n- %s: %s → %s", u.Path, u.Current, u.Latest)
	}

	commitMsg := msg + "\n" + body.String()
	commitCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "commit", "-m", commitMsg))
	commitCmd.Dir = anvilPath

	var commitStderr bytes.Buffer
	commitCmd.Stderr = &commitStderr

	if err := commitCmd.Run(); err != nil {
		return fmt.Errorf("git commit for group %q: %w\nstderr: %s", group.Name, err, commitStderr.String())
	}

	return nil
}
