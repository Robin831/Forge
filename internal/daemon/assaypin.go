package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/state"
)

// assayPin is a review aimed at one commit of a PR (`forge assay rerun --sha`)
// rather than its head: the diff reviewed is Base..SHA, both full commit ids.
type assayPin struct {
	SHA  string
	Base string
}

// assayPinTimeout bounds the synchronous fetch-and-validate the assay_rerun
// handler runs before replying; it stays under the CLI's read timeout.
const assayPinTimeout = 90 * time.Second

// assayPinSHAPattern admits abbreviated and full commit ids only. It also keeps
// the value from ever reaching git as an option.
var assayPinSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)

// assayPinGitRaw runs git in dir and returns its stdout verbatim. Stderr is
// folded into the error, since these errors are what the operator reads.
func assayPinGitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...))
	cmd.Env = executil.CleanGitEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func assayPinGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := assayPinGitRaw(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

func assayPinHasCommit(ctx context.Context, repo, rev string) bool {
	_, err := assayPinGit(ctx, repo, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return err == nil
}

// resolveAssayPin validates sha as a commit of PR prNumber and computes the base
// of its review. repo is the anvil checkout; headSHA is the PR head as the VCS
// reports it, which anchors the history check without trusting FETCH_HEAD.
//
// The base is merge-base(sha, origin/<baseBranch>): where the PR's own diff
// would have started had sha been its head. A sha already on the base branch
// has an empty range and is refused.
func resolveAssayPin(ctx context.Context, repo string, prNumber int, baseBranch, headSHA, sha string) (*assayPin, error) {
	if !assayPinSHAPattern.MatchString(sha) {
		return nil, fmt.Errorf("invalid sha %q: expected a commit id of 4-40 hex characters", sha)
	}
	if headSHA == "" {
		return nil, fmt.Errorf("cannot validate sha %s: PR #%d's head commit is unknown", sha, prNumber)
	}

	if _, err := assayPinGit(ctx, repo, "fetch", "origin"); err != nil {
		return nil, fmt.Errorf("fetching origin to validate sha %s: %w", sha, err)
	}
	// A merged PR's branch is usually deleted; the forge's own PR refs outlive it.
	if !assayPinHasCommit(ctx, repo, headSHA) {
		for _, ref := range []string{
			"refs/pull/" + strconv.Itoa(prNumber) + "/head",
			"refs/merge-requests/" + strconv.Itoa(prNumber) + "/head",
		} {
			if _, err := assayPinGit(ctx, repo, "fetch", "origin", ref); err == nil && assayPinHasCommit(ctx, repo, headSHA) {
				break
			}
		}
	}
	if !assayPinHasCommit(ctx, repo, headSHA) {
		return nil, fmt.Errorf("cannot validate sha %s: PR #%d's head %s could not be fetched", sha, prNumber, shortSHA(headSHA))
	}

	full, err := assayPinGit(ctx, repo, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil || full == "" {
		return nil, fmt.Errorf("commit %s not found in the repository (after fetching PR #%d)", sha, prNumber)
	}

	if _, err := assayPinGit(ctx, repo, "merge-base", "--is-ancestor", full, headSHA); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, fmt.Errorf("commit %s is not in PR #%d's history (head %s)", shortSHA(full), prNumber, shortSHA(headSHA))
		}
		return nil, fmt.Errorf("checking commit %s against PR #%d's head: %w", shortSHA(full), prNumber, err)
	}

	baseRef, err := assayPinBaseRef(ctx, repo, baseBranch)
	if err != nil {
		return nil, err
	}
	base, err := assayPinGit(ctx, repo, "merge-base", full, baseRef)
	if err != nil {
		return nil, fmt.Errorf("computing merge-base of %s and %s: %w", shortSHA(full), baseRef, err)
	}
	if base == full {
		return nil, fmt.Errorf("commit %s is already on %s, so base..sha is empty; pick a commit PR #%d introduced", shortSHA(full), baseRef, prNumber)
	}
	return &assayPin{SHA: full, Base: base}, nil
}

// assayPinBaseRef names the remote-tracking ref of the PR's base branch. An
// empty base branch means the repository default, resolved as the worktree
// manager does: origin/main, else origin/master.
func assayPinBaseRef(ctx context.Context, repo, baseBranch string) (string, error) {
	candidates := []string{"origin/main", "origin/master"}
	if baseBranch != "" {
		candidates = []string{"origin/" + baseBranch}
	}
	for _, ref := range candidates {
		if assayPinHasCommit(ctx, repo, ref) {
			return ref, nil
		}
	}
	return "", fmt.Errorf("base branch %s not found on origin", strings.Join(candidates, " or "))
}

// resolveAssayRerunPin validates a --sha rerun before the handler replies, so a
// bad commit is an error on the CLI rather than a failed run in the feed.
// d.assayPinResolve replaces it in tests.
func (d *Daemon) resolveAssayRerunPin(pr *state.PR, anvilCfg config.AnvilConfig, sha string) (*assayPin, error) {
	if d.assayPinResolve != nil {
		return d.assayPinResolve(pr, anvilCfg.Path, sha)
	}
	ctx, cancel := context.WithTimeout(d.runCtx, assayPinTimeout)
	defer cancel()
	st, err := d.vcsForAnvil(pr.Anvil).CheckStatusLight(ctx, anvilCfg.Path, pr.Number)
	if err != nil {
		return nil, fmt.Errorf("cannot validate sha %s: resolving PR #%d's head: %w", sha, pr.Number, err)
	}
	if st == nil {
		return nil, fmt.Errorf("cannot validate sha %s: PR #%d's head commit is unknown", sha, pr.Number)
	}
	return resolveAssayPin(ctx, anvilCfg.Path, pr.Number, pr.BaseBranch, st.HeadSHA, sha)
}

// fetchAssayPinnedDiff checks the worktree out at the pinned commit, so the
// passes read files as they were there, and returns the Base..SHA diff. The
// lifecycle worktree is removed after an Assay run, so the detached HEAD does
// not outlive it. d.assayPinnedDiff replaces it in tests.
func (d *Daemon) fetchAssayPinnedDiff(ctx context.Context, worktreePath string, pin assayPin) ([]byte, error) {
	if d.assayPinnedDiff != nil {
		return d.assayPinnedDiff(ctx, worktreePath, pin)
	}
	if _, err := assayPinGit(ctx, worktreePath, "checkout", "--detach", "--force", pin.SHA); err != nil {
		return nil, err
	}
	return assayPinGitRaw(ctx, worktreePath, "diff", pin.Base, pin.SHA)
}

// startPinnedAssayRerun is the --sha branch of the assay_rerun handler. It
// validates the commit synchronously, then dispatches a manual Assay review
// pinned to it.
func (d *Daemon) startPinnedAssayRerun(pr *state.PR, anvilCfg config.AnvilConfig, sha string) ipc.Response {
	pin, err := d.resolveAssayRerunPin(pr, anvilCfg, sha)
	if err != nil {
		return errorResponse(err.Error())
	}
	_ = d.db.LogEvent(state.EventPRReviewNeeded,
		fmt.Sprintf("Assay re-review requested for PR #%d at %s (manual, pinned, shadow)", pr.Number, shortSHA(pin.SHA)),
		pr.BeadID, pr.Anvil)
	d.logger.Info("Assay pinned re-review requested", "pr", pr.Number, "anvil", pr.Anvil, "bead", pr.BeadID,
		"sha", pin.SHA, "base", pin.Base)

	req := lifecycle.ActionRequest{
		Action:     lifecycle.ActionAssayReview,
		PRNumber:   pr.Number,
		BeadID:     pr.BeadID,
		Anvil:      pr.Anvil,
		Branch:     pr.Branch,
		BaseBranch: pr.BaseBranch,
		HeadSHA:    pin.SHA,
		IsManual:   true,
		PinnedSHA:  pin.SHA,
		PinnedBase: pin.Base,
	}
	dispatch := d.lifecycleDispatch
	if dispatch == nil {
		dispatch = d.handleLifecycleAction
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		dispatch(d.runCtx, req)
	}()

	return okResponse(map[string]string{"message": fmt.Sprintf(
		"Assay re-review started for PR #%d at %s (base %s, pinned, shadow mode)",
		pr.Number, shortSHA(pin.SHA), shortSHA(pin.Base))})
}

// logAssayPinnedFindings writes a pinned run's findings to the daemon log, one
// line each. The run keeps them out of pr_findings, so this is where they live.
func (d *Daemon) logAssayPinnedFindings(prNumber int, beadID string, pin assayPin, findings []assay.Finding) {
	for _, f := range findings {
		d.logger.Info("Assay pinned finding",
			"pr", prNumber, "bead", beadID, "sha", pin.SHA, "base", pin.Base,
			"severity", string(f.Severity), "category", f.Category, "anchor", f.Anchor,
			"pass", f.SourcePass, "title", f.Title)
	}
}
