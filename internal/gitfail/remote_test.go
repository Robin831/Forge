package gitfail

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// The measured fault: an anvil's origin pointing at a path inside one of its own
// worker worktrees, which was deleted when the worker finished.
func TestSelfReferentialRemote_WorkerWorktreeInsideTheAnvil(t *testing.T) {
	anvil := filepath.FromSlash("/home/forge/anvils/fhi.munin.explorer")
	origin := filepath.FromSlash("/home/forge/anvils/fhi.munin.explorer/.workers/Fhi.Metadata-l9l2n.44/.git")

	if !SelfReferentialRemote(origin, anvil) {
		t.Errorf("SelfReferentialRemote(%q, %q) = false, want true", origin, anvil)
	}
}

func TestSelfReferentialRemote_TheRepositoryItself(t *testing.T) {
	anvil := filepath.FromSlash("/home/forge/anvils/explorer")
	for _, origin := range []string{
		filepath.FromSlash("/home/forge/anvils/explorer"),
		filepath.FromSlash("/home/forge/anvils/explorer/.git"),
	} {
		if !SelfReferentialRemote(origin, anvil) {
			t.Errorf("SelfReferentialRemote(%q, %q) = false, want true", origin, anvil)
		}
	}
}

// A remote that is simply somewhere else on disk is not this condition. A local
// path origin is unusual but legitimate — a mirror, a test fixture — and
// reporting one as the fault would send an operator to repoint a working remote.
func TestSelfReferentialRemote_AnotherLocalPathIsNot(t *testing.T) {
	anvil := filepath.FromSlash("/home/forge/anvils/explorer")
	for _, origin := range []string{
		filepath.FromSlash("/home/forge/anvils/munin/.git"),
		filepath.FromSlash("/srv/mirrors/explorer.git"),
		// A sibling whose name merely starts with the anvil's: string-prefix
		// containment would call this inside, filepath.Rel does not.
		filepath.FromSlash("/home/forge/anvils/explorer-fork/.git"),
	} {
		if SelfReferentialRemote(origin, anvil) {
			t.Errorf("SelfReferentialRemote(%q, %q) = true, want false", origin, anvil)
		}
	}
}

// The normal case, and the one that must never be touched: a remote with a
// transport on it names no path on this machine.
func TestSelfReferentialRemote_TransportURLsAreNot(t *testing.T) {
	anvil := filepath.FromSlash("/home/forge/anvils/explorer")
	for _, origin := range []string{
		"https://github.com/FHIDev/Fhi.Munin.Explorer.git",
		"ssh://git@github.com/FHIDev/Fhi.Munin.Explorer.git",
		"git://github.com/FHIDev/Fhi.Munin.Explorer.git",
		"git@github.com:FHIDev/Fhi.Munin.Explorer.git",
		"",
		"   ",
	} {
		if SelfReferentialRemote(origin, anvil) {
			t.Errorf("SelfReferentialRemote(%q, %q) = true, want false", origin, anvil)
		}
	}
}

// file:// names a local path with a scheme on it, so it is unwrapped rather than
// dismissed as a transport.
func TestSelfReferentialRemote_FileURL(t *testing.T) {
	anvil := filepath.FromSlash("/home/forge/anvils/explorer")
	if !SelfReferentialRemote("file:///home/forge/anvils/explorer/.workers/bd-1/.git", anvil) {
		t.Error("a file:// URL inside the anvil must be recognised")
	}
	if SelfReferentialRemote("file:///srv/mirrors/explorer.git", anvil) {
		t.Error("a file:// URL outside the anvil must not be")
	}
}

// A drive letter is a volume root on a host that has volumes, and a colon in
// position 1 — git's scp-like separator — on one that does not. The assertion
// is therefore different per platform, which is the whole point: the branch
// used to fire everywhere, so on Linux a drive path was accepted as local,
// found not-rooted, JOINED onto the anvil, and came back inside it.
func TestSelfReferentialRemote_DriveLetterPaths(t *testing.T) {
	const anvil = `C:\source\anvils\explorer`
	inside := `C:\source\anvils\explorer\.workers\bd-1\.git`
	outside := `C:\source\anvils\munin\.git`

	if filepath.Separator == '\\' {
		if !SelfReferentialRemote(inside, anvil) {
			t.Error("with volumes, a drive path inside the anvil must be recognised")
		}
		if SelfReferentialRemote(outside, anvil) {
			t.Error("with volumes, a drive path outside the anvil must not be")
		}
		return
	}

	// No volumes here, so neither string names a path on this host at all —
	// git reads "C:" as scp-like host "C". Reporting either would be a false
	// positive, and the second one was: it sends an operator to repoint a
	// remote that is fine.
	if SelfReferentialRemote(inside, anvil) {
		t.Error("without volumes a drive path is not a local path")
	}
	if SelfReferentialRemote(outside, anvil) {
		t.Error("a drive path outside the anvil must never be reported as inside")
	}
}

// TestSelfReferentialRemote_ColonAfterASeparatorIsALocalPath: git reads a colon
// as the scp-like separator only when it comes before the first separator, so a
// directory whose NAME contains one is an ordinary local path. A bare
// Contains(url, ":") dismissed it as a transport, which would leave a genuinely
// self-referential remote under such a directory unreported.
//
// Built with the host's own separator so it is a real local path on both.
func TestSelfReferentialRemote_ColonAfterASeparatorIsALocalPath(t *testing.T) {
	sep := string(filepath.Separator)
	anvil := filepath.Join(sep+"home", "forge", "anvils", "ex:plorer")
	inside := filepath.Join(anvil, ".workers", "bd-1", ".git")
	sibling := filepath.Join(sep+"home", "forge", "anvils", "mu:nin", ".git")

	if !SelfReferentialRemote(inside, anvil) {
		t.Error("a colon inside a path segment does not make the path a transport URL")
	}
	if SelfReferentialRemote(sibling, anvil) {
		t.Error("a sibling is still outside, colon or no colon")
	}
}

// A relative remote path is git's own spelling for one, and is what a shell run
// inside the worktree would most naturally produce. It resolves against the
// repository, not against the daemon's working directory — resolved against the
// latter it lands somewhere unrelated and the fault reads as healthy.
func TestSelfReferentialRemote_RelativePathsResolveAgainstTheRepo(t *testing.T) {
	anvil := filepath.FromSlash("/home/forge/anvils/explorer")

	for _, origin := range []string{".workers/bd-1/.git", ".", ".git"} {
		if !SelfReferentialRemote(origin, anvil) {
			t.Errorf("SelfReferentialRemote(%q, %q) = false, want true", origin, anvil)
		}
	}
	for _, origin := range []string{"../munin/.git", "../../elsewhere.git"} {
		if SelfReferentialRemote(origin, anvil) {
			t.Errorf("SelfReferentialRemote(%q, %q) = true, want false", origin, anvil)
		}
	}
}

// A UNC path names another machine's share and is never inside the anvil.
func TestSelfReferentialRemote_UNCPath(t *testing.T) {
	if SelfReferentialRemote(`\\server\share\repo.git`, `C:\anvils\explorer`) {
		t.Error("a UNC path is not inside the anvil")
	}
}

func TestSelfReferentialRemote_NoRepoPath(t *testing.T) {
	if SelfReferentialRemote(filepath.FromSlash("/anywhere/.git"), "") {
		t.Error("with no repository to compare against there is no invariant to violate")
	}
}

func TestOriginURL(t *testing.T) {
	run := func(_ context.Context, dir string, args ...string) ([]byte, error) {
		if dir != "/anvil" || args[0] != "config" {
			t.Fatalf("unexpected call: %s %v", dir, args)
		}
		return []byte("https://github.com/FHIDev/x.git\n"), nil
	}
	got, err := OriginURL(context.Background(), "/anvil", run)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://github.com/FHIDev/x.git" {
		t.Errorf("got %q", got)
	}
}

func TestOriginURL_NoRunnerOrNoRepo(t *testing.T) {
	got, err := OriginURL(context.Background(), "", nil)
	if err != nil || got != "" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestOriginURL_Failure(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 1")
	}
	got, err := OriginURL(context.Background(), "/anvil", run)
	if err == nil {
		t.Error("expected the error to reach the caller, which decides it is not fatal")
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// The classification this fault turned on. git prints both lines; the transient
// one used to win because the blocked set had no pattern for the other.
func TestClassify_RemoteThatIsNotARepositoryIsBlocked(t *testing.T) {
	stderr := "fatal: '/home/forge/anvils/fhi.munin.explorer/.workers/Fhi.Metadata-l9l2n.44/.git' " +
		"does not appear to be a git repository\n" +
		"fatal: Could not read from remote repository.\n\n" +
		"Please make sure you have the correct access rights\nand the repository exists."

	if got := Classify(stderr, nil); got != Blocked {
		t.Errorf("Classify = %v, want Blocked — a deleted directory does not become a repository on retry", got)
	}
	if got := CauseOf(stderr); got != CauseBadRemote {
		t.Errorf("CauseOf = %v, want CauseBadRemote", got)
	}
}

// The local-checkout message keeps its own cause: the remedies differ.
func TestCauseOf_LocalNotARepoIsUnchanged(t *testing.T) {
	stderr := "fatal: not a git repository (or any of the parent directories): .git"
	if got := Classify(stderr, nil); got != Blocked {
		t.Errorf("Classify = %v, want Blocked", got)
	}
	if got := CauseOf(stderr); got != CauseNotARepo {
		t.Errorf("CauseOf = %v, want CauseNotARepo", got)
	}
}

// A genuine outage must stay transient — the new pattern is specific to the
// remote not being a repository, not to failing to reach one.
func TestClassify_RealOutageStaysTransient(t *testing.T) {
	for _, stderr := range []string{
		"fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com",
		"fatal: Could not read from remote repository.\nssh: connect to host github.com port 22: Connection timed out",
	} {
		if got := Classify(stderr, nil); got != Transient {
			t.Errorf("Classify(%q) = %v, want Transient", stderr, got)
		}
	}
}
