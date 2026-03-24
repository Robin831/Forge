package wicket

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
)

// reGitHubSSH matches git@github.com:owner/repo.git URLs.
var reGitHubSSH = regexp.MustCompile(`(?i)git@github\.com[:/]([^/]+/[^/]+?)(?:\.git)?$`)

// reGitHubHTTPS matches https://github.com/owner/repo URLs.
var reGitHubHTTPS = regexp.MustCompile(`(?i)https?://github\.com/([^/]+/[^/]+?)(?:\.git)?$`)

// RepoResolver resolves the list of GitHub repositories ("owner/repo") for a
// given anvil configuration. When WicketRepos is explicitly set it is returned
// as-is; otherwise the repository is derived from the anvil's git origin remote.
type RepoResolver struct {
	// gitRemoteFunc returns the raw git remote URL for the given directory.
	// When nil, the real git CLI is invoked. Tests inject a stub here.
	gitRemoteFunc func(ctx context.Context, dir string) (string, error)
}

// NewRepoResolver returns a RepoResolver that uses the real git CLI.
func NewRepoResolver() *RepoResolver {
	return &RepoResolver{}
}

// ResolveRepos returns the list of "owner/repo" strings for the given anvil.
//
// When anvil.WicketRepos is non-empty, those values are returned directly and
// no git subprocess is spawned.  Otherwise the repository is inferred from the
// origin remote URL of anvil.Path.
func (r *RepoResolver) ResolveRepos(ctx context.Context, anvil config.AnvilConfig) ([]string, error) {
	if len(anvil.WicketRepos) > 0 {
		return anvil.WicketRepos, nil
	}

	gitRemoteFn := r.gitRemoteFunc
	if gitRemoteFn == nil {
		gitRemoteFn = gitRemoteURL
	}

	raw, err := gitRemoteFn(ctx, anvil.Path)
	if err != nil {
		return nil, err
	}
	repo, err := parseGitHubRepo(raw)
	if err != nil {
		return nil, err
	}
	return []string{repo}, nil
}

// gitRemoteURL invokes `git remote get-url origin` in dir and returns the
// trimmed stdout. A 10-second timeout is applied so a slow or hung git process
// cannot block the scan loop indefinitely.
func gitRemoteURL(ctx context.Context, dir string) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(tctx, "git", "remote", "get-url", "origin"))
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// parseGitHubRepo extracts the "owner/repo" slug from a GitHub remote URL.
// Both SSH (git@github.com:owner/repo.git) and HTTPS
// (https://github.com/owner/repo) formats are accepted.
func parseGitHubRepo(rawURL string) (string, error) {
	if m := reGitHubSSH.FindStringSubmatch(rawURL); m != nil {
		return m[1], nil
	}
	if m := reGitHubHTTPS.FindStringSubmatch(rawURL); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("cannot parse GitHub owner/repo from remote URL %q", rawURL)
}
