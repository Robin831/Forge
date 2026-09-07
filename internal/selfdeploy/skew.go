package selfdeploy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/gitfail"
)

// Version skew: how far the RUNNING binary is behind the branch it deploys from.
//
// Deploy is triggered by a bellows pr_merged event, which is the only signal
// this package ever had — and it is not enough. A PR merged by hand emits no
// such event at all when its row is not bellows-managed (every `ext-*` PR), so
// between 2026-09-02 and 2026-09-07 six merges landed on main and the daemon
// kept running the build from before them: seven commits behind, for five days,
// with nothing anywhere saying so. The trigger was working exactly as designed;
// the design read "a PR merged" as "main moved", and those are not the same
// claim. Nor would fixing the event cover the rest of them — a direct push to
// main, a merge from the GitHub UI, a commit made while the daemon was down.
//
// So the running build is compared against the branch's tip directly, which is
// the one measurement that answers the question whatever moved the branch and
// whoever moved it. It is a READ: fetch (which writes only .git), resolve, count.
// Nothing here pulls, builds, swaps or restarts — the caller takes the ordinary
// drain-and-rebuild path on the result, so a skew-triggered deploy is the same
// deploy a merge event triggers, with the same drain, the same stash discipline
// and the same rollback.

// originRemote is the remote both the deploy's fast-forward pull (pull.go) and
// this check name. One constant that both use because the two must always mean
// the same remote: a check reading a tip the pull does not fetch from would
// report a skew every deploy closes and a deploy the check never notices.
const originRemote = "origin"

// skewAgeMaxCommits bounds the range the "how long has it been behind" figure is
// derived over. The derivation lists one line per undeployed commit, which is a
// few kilobytes for any skew an operator would call skew and unbounded for a
// checkout whose build predates a repository's whole history. Past the bound the
// count and the tip are still reported and only the age is left unknown — a
// figure that large adds nothing the count has not already said.
const skewAgeMaxCommits = 500

// Sentinels for the three ways the comparison cannot be made. All three mean
// "do not deploy": a skew that cannot be measured is not a skew of zero, and a
// deploy dispatched on a number nobody computed is a restart for no reason.
var (
	// ErrBuildUnidentified: the running binary carries no commit id to compare
	// against — an unstamped `go build`, a `go run`, a build identified by a
	// version string. Nothing is wrong with the checkout; the question simply
	// cannot be asked of this binary.
	ErrBuildUnidentified = errors.New("selfdeploy: the running build is not identified by a commit")
	// ErrBuildNotInCheckout: the build's commit is not an object in the deploy
	// checkout. The binary was built somewhere else, or from a commit this
	// checkout has never fetched.
	ErrBuildNotInCheckout = errors.New("selfdeploy: the running build's commit is not in the deploy checkout")
	// ErrBuildNotAncestor: the build's commit is not on the deploy branch — the
	// branch was force-pushed, or the binary was built from a side branch. The
	// distance is undefined in that direction, so it is reported rather than
	// guessed at: "behind by N" would be a claim about a history that no longer
	// contains the running build.
	ErrBuildNotAncestor = errors.New("selfdeploy: the running build is not an ancestor of the deploy branch")
)

// classifiedSkewError attaches one of the sentinels above to the cause that
// produced it while keeping the SINGLE-cause chain every other error in this
// package returns.
//
// `fmt.Errorf("%w: ...: %w", sentinel, cause)` says both things in one line, but
// what it builds is a multi-error: errors.Is traverses both branches, and so the
// classification works — but errors.Unwrap returns nil for it, and a caller that
// walks the chain by hand, or an errors.As into a type both branches could
// satisfy, resolves differently here than for every other error this package
// returns. Two shapes of error out of one package is a distinction nobody asked
// for and nothing documents.
//
// Here the sentinel is matched by Is and the cause is the one Unwrap link, so
// errors.Is finds both, errors.Unwrap still names the cause, and errors.As sees
// only the cause chain.
type classifiedSkewError struct {
	// sentinel classifies the failure. It is deliberately NOT the unwrap link:
	// the cause is what a caller walking the chain wants — the daemon's own
	// handler tests errors.Is(err, context.Canceled) first, so a check cancelled
	// by a shutdown is not logged as a claim about how the binary was built.
	sentinel error
	cause    error
	detail   string
}

func (e *classifiedSkewError) Error() string {
	if e.detail == "" {
		return e.sentinel.Error()
	}
	return e.sentinel.Error() + ": " + e.detail
}

func (e *classifiedSkewError) Unwrap() error { return e.cause }

func (e *classifiedSkewError) Is(target error) bool { return target == e.sentinel }

// classifySkewErr builds one. The detail carries the cause's own text (the
// sentinel says which failure this is; the cause says what git reported), and
// the cause itself rides structurally so errors.Is keeps finding it.
func classifySkewErr(sentinel, cause error, detailFormat string, args ...any) error {
	return &classifiedSkewError{
		sentinel: sentinel,
		cause:    cause,
		detail:   fmt.Sprintf(detailFormat, args...),
	}
}

// buildSHAPattern is what a build id must look like before it is handed to git
// as a revision. The test is deliberately narrow — an abbreviated or full hex
// object name and nothing else — because git resolves far more than SHAs: a
// build stamped `main` or `v1.2.0` would resolve to a branch or tag, and a
// binary built from an old commit but stamped with a moving name would report a
// skew of zero forever. Rejected here it comes back ErrBuildUnidentified, which
// says what is true.
var buildSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// SkewConfig names the comparison: which checkout, which branch, and the build
// the running process was made from.
type SkewConfig struct {
	// RepoPath is the deploy checkout — the same one Deployer pulls and builds.
	RepoPath string
	// Branch is the deploy branch, compared as <remote>/<branch>. Defaults to
	// "main", matching Config.Branch.
	Branch string
	// BuildSHA identifies the running binary (forge.Build). A "-dirty" suffix is
	// stripped: a dirty build was still made from that commit, and the distance
	// from it is exactly as meaningful.
	BuildSHA string
}

// Skew is the measured distance between the running build and the deploy
// branch's tip.
type Skew struct {
	// BuildSHA is the running build's commit, resolved to its full object name.
	BuildSHA string
	// HeadSHA is the tip of <remote>/<branch> after the fetch.
	HeadSHA string
	// Branch is the deploy branch the tip was read from, so every message
	// rendered from a Skew names the branch it is about rather than assuming
	// main.
	Branch string
	// Commits is how many commits the tip is ahead of the running build. Zero
	// means the running binary is current.
	Commits int
	// Since is the commit time of the OLDEST undeployed commit — when the
	// running build first fell behind, rather than when the newest commit
	// landed. Zero when it could not be derived (see skewAgeMaxCommits), which
	// is why every consumer treats a zero age as "unknown" and not as "new".
	Since time.Time
}

// Behind reports whether the deploy branch has moved past the running build.
func (s Skew) Behind() bool { return s.Commits > 0 }

// Age is how long the running build has been behind, measured from the oldest
// undeployed commit. It returns 0 when Since is unknown or in the future, so a
// caller thresholding on it errs towards not escalating.
func (s Skew) Age(now time.Time) time.Duration {
	if s.Since.IsZero() || !now.After(s.Since) {
		return 0
	}
	return now.Sub(s.Since)
}

// Summary renders the skew for a log line, an event, or the needs-attention
// detail — one sentence, in the order an operator reads it: what is running,
// how far behind, and since when.
func (s Skew) Summary(now time.Time) string {
	out := fmt.Sprintf("the running build %s is %d commit(s) behind %s/%s (%s)",
		shortSkewSHA(s.BuildSHA), s.Commits, originRemote, s.Branch, shortSkewSHA(s.HeadSHA))
	if age := s.Age(now); age > 0 {
		out += fmt.Sprintf(", behind for %s", age.Round(time.Minute))
	}
	return out
}

// SkewChecker measures Skew against a checkout. It only ever reads: the fetch
// writes inside .git and nothing else, so a checkout with local modifications —
// which Forge's own reliably has — is measured exactly as it stands.
type SkewChecker struct {
	cfg SkewConfig
	cmd Commander
}

// NewSkewChecker builds a checker, defaulting Branch the way Config does and
// defaulting the Commander to ExecCommander.
//
// The default is what makes the environment contract this check rests on hold by
// construction rather than by the call site remembering it. Every command here
// is `git -C <RepoPath>`, and an ambient GIT_DIR or GIT_WORK_TREE answers for
// the repository it names instead of that path: `rev-parse` would then resolve
// the running build against some other checkout and the check would report a
// skew of zero forever — the exact silence it exists to break, arriving through
// the environment. ExecCommander strips those variables (executil.CleanGitEnv)
// and pins LC_ALL=C besides. A Commander supplied here must do the same; nil
// gets one that does.
func NewSkewChecker(cfg SkewConfig, cmd Commander) *SkewChecker {
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cmd == nil {
		cmd = ExecCommander{}
	}
	return &SkewChecker{cfg: cfg, cmd: cmd}
}

// Check fetches the deploy branch and reports how far the running build is
// behind its tip.
//
// Every step that cannot be answered is an error and never a zero: the caller
// dispatches a deploy on Commits > 0, so "I could not tell" must not arrive as
// "nothing to do" (the silence this whole check exists to break) nor as
// "something to do" (a restart on a number nobody computed).
func (c *SkewChecker) Check(ctx context.Context) (Skew, error) {
	branch := c.cfg.Branch
	if strings.HasPrefix(branch, "-") {
		// The branch is operator config and reaches git as a positional
		// argument; one shaped like a flag would be parsed as one.
		return Skew{}, fmt.Errorf("selfdeploy: invalid deploy branch %q", branch)
	}

	build, ok := normalizeBuildSHA(c.cfg.BuildSHA)
	if !ok {
		return Skew{}, fmt.Errorf("%w: %q", ErrBuildUnidentified, c.cfg.BuildSHA)
	}

	// Fetch with an explicit forced refspec rather than a bare `git fetch
	// origin <branch>`: only the explicit form is guaranteed to write the
	// remote-tracking ref the next step reads, whatever the git version and
	// whatever the remote's configured refspec says. The alternative — reading
	// FETCH_HEAD — is not safe here, since worker worktrees share this
	// repository and fetch into it too.
	trackingRef := fmt.Sprintf("refs/remotes/%s/%s", originRemote, branch)
	refspec := fmt.Sprintf("+%s:%s", branch, trackingRef)
	if out, err := c.git(ctx, "fetch", originRemote, refspec); err != nil {
		// git's own words are quoted through the same treatment every other
		// caller in this package gives them: `git fetch` relays the remote's
		// `remote:` lines verbatim, so these bytes are chosen by whatever is on
		// the other end of origin, and they land in daemon.log, which the web
		// dashboard tails and Hearth renders.
		return Skew{}, fmt.Errorf("selfdeploy: fetching %s/%s: %w: %s", originRemote, branch, err,
			gitfail.Sanitize(firstNonEmpty(out, err.Error()), maxEvidenceBytes))
	}

	head, err := c.revParse(ctx, trackingRef)
	if err != nil {
		return Skew{}, fmt.Errorf("selfdeploy: resolving %s: %w", trackingRef, err)
	}

	// Resolve the build's own commit before comparing. This is what separates
	// "the binary was built elsewhere" from "the branch moved past it", and it
	// is why the pattern check above is worth having: git would happily resolve
	// a name.
	buildFull, err := c.revParse(ctx, build)
	if err != nil {
		// The cause is carried alongside the sentinel, not dropped for it: the
		// caller branches on context.Canceled first so a shutdown is not
		// reported as a failure, and a cancelled rev-parse that arrived here
		// carrying only ErrBuildNotInCheckout would be logged as a claim about
		// how the binary was built.
		return Skew{}, classifySkewErr(ErrBuildNotInCheckout, err, "%s: %v", build, err)
	}

	skew := Skew{BuildSHA: buildFull, HeadSHA: head, Branch: branch}
	if buildFull == head {
		return skew, nil
	}

	if out, err := c.git(ctx, "merge-base", "--is-ancestor", buildFull, head); err != nil {
		// Same reasoning as the rev-parse above: the sentinel classifies, the
		// carried cause is what lets a cancellation or a timeout be recognised
		// as one.
		return skew, classifySkewErr(ErrBuildNotAncestor, err, "build %s, %s/%s at %s: %v: %s",
			shortSkewSHA(buildFull), originRemote, branch, shortSkewSHA(head), err,
			gitfail.Sanitize(firstNonEmpty(out, err.Error()), maxEvidenceBytes))
	}

	countOut, err := c.git(ctx, "rev-list", "--count", buildFull+".."+head)
	if err != nil {
		return skew, fmt.Errorf("selfdeploy: counting commits since %s: %w: %s", shortSkewSHA(buildFull), err,
			gitfail.Sanitize(firstNonEmpty(countOut, err.Error()), maxEvidenceBytes))
	}
	n, err := strconv.Atoi(strings.TrimSpace(countOut))
	if err != nil {
		return skew, fmt.Errorf("selfdeploy: unreadable commit count %q: %w", countOut, err)
	}
	skew.Commits = n
	skew.Since = c.oldestCommitTime(ctx, buildFull, head, n)
	return skew, nil
}

// oldestCommitTime dates the first commit the running build is missing, which is
// when it fell behind. Best-effort by construction: the count and the tip are
// the measurement, and a range too long to walk (or a git that will not walk it)
// costs the age alone.
func (c *SkewChecker) oldestCommitTime(ctx context.Context, build, head string, commits int) time.Time {
	if commits <= 0 || commits > skewAgeMaxCommits {
		return time.Time{}
	}
	out, err := c.git(ctx, "log", "--format=%ct", build+".."+head)
	if err != nil {
		return time.Time{}
	}
	// git log is newest-first, so the oldest undeployed commit is the last line.
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(lines[len(lines)-1], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// revParse resolves a revision to a full commit object name, failing when it
// names anything that is not a commit in this checkout.
func (c *SkewChecker) revParse(ctx context.Context, rev string) (string, error) {
	out, err := c.git(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w: %s", rev, err,
			gitfail.Sanitize(firstNonEmpty(out, err.Error()), maxEvidenceBytes))
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", fmt.Errorf("git rev-parse %s resolved nothing", rev)
	}
	return sha, nil
}

func (c *SkewChecker) git(ctx context.Context, args ...string) (string, error) {
	out, err := c.cmd.Run(ctx, c.cfg.RepoPath, "git", args...)
	return strings.TrimSpace(string(out)), err
}

// normalizeBuildSHA turns a build stamp into a revision git can be asked about,
// reporting false for one that is not an object name at all ("unknown", "dev",
// a version string, an empty value).
func normalizeBuildSHA(build string) (string, bool) {
	s := strings.TrimSpace(build)
	s = strings.TrimSuffix(s, "-dirty")
	if !buildSHAPattern.MatchString(s) {
		return "", false
	}
	return s, true
}

// shortSkewSHA abbreviates an object name for human-facing text, leaving a value
// shorter than the abbreviation alone.
func shortSkewSHA(sha string) string {
	const n = 12
	if len(sha) > n {
		return sha[:n]
	}
	return sha
}
