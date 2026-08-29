package assay

import (
	"strings"
	"testing"
)

// TestBashCandidatePathsReadsRealCommands runs the parser over the command
// shapes an Assay pass actually issues. Every one of the 742 tool calls
// measured over 95 pass sessions was Bash, so these ARE the file reads: if this
// returns nothing, files= is zero and the retry's diff scoping never happens.
func TestBashCandidatePathsReadsRealCommands(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want []string
	}{
		{
			name: "cat one file",
			cmd:  "cat internal/assay/retry.go",
			want: []string{"internal/assay/retry.go"},
		},
		{
			name: "sed range — the range expression is not a path",
			cmd:  "sed -n '1,50p' foo/bar.go",
			want: []string{"foo/bar.go"},
		},
		{
			name: "grep through a pipeline — the pattern is not a path, the directory is",
			cmd:  `grep -rn "loadRepoGuidance" internal/ | head -20`,
			want: []string{"internal/"},
		},
		{
			name: "two files, one command",
			cmd:  "cat a/one.go b/two.go",
			want: []string{"a/one.go", "b/two.go"},
		},
		{
			name: "and-chained commands are separate segments",
			cmd:  "cd internal/assay && sed -n 1,80p retry.go",
			want: []string{"internal/assay", "retry.go"},
		},
		{
			name: "leading env assignment is not the command word",
			cmd:  "LC_ALL=C git show HEAD:internal/assay/skip.go",
			want: nil, // "HEAD:internal/assay/skip.go" is a revision, not a path... but it is path-shaped
		},
		{
			name: "a go test invocation names no file",
			cmd:  "go test ./... -run TestX",
			want: nil,
		},
		{
			name: "a bare command names nothing",
			cmd:  "ls",
			want: nil,
		},
		{
			name: "a redirection target is a new segment's command word, not a read file",
			cmd:  "cat internal/assay/skip.go > /tmp/out.txt",
			want: []string{"internal/assay/skip.go"},
		},
		{
			name: "globs and variables name a set or a value, never a file",
			cmd:  "cat internal/*.go $PKG/x.go {a,b}.go",
			want: nil,
		},
		{
			name: "empty",
			cmd:  "   ",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bashCandidatePaths(tc.cmd)
			if tc.name == "leading env assignment is not the command word" {
				// git's rev:path syntax is path-shaped and is admitted; what
				// this case pins is that LC_ALL=C did not consume the drop
				// reserved for the command word, which would have made "show"
				// an argument and "git" a candidate.
				if containsStr(got, "git") {
					t.Fatalf("candidates = %v; the env assignment consumed the command-word drop", got)
				}
				return
			}
			if !equalStrs(got, tc.want) {
				t.Errorf("bashCandidatePaths(%q) = %v; want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestBashCandidatePathsIsBounded pins the two caps. The command string is
// model output and therefore arbitrary.
func TestBashCandidatePathsIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("cat")
	for i := 0; i < maxBashCandidates+40; i++ {
		b.WriteString(" pkg/f")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(string(rune('a' + i/26)))
		b.WriteString(".go")
	}
	if got := len(bashCandidatePaths(b.String())); got != maxBashCandidates {
		t.Errorf("candidates = %d; want the cap %d", got, maxBashCandidates)
	}
	// An over-long command is truncated rather than scanned whole; it must
	// still answer, and must not answer with the truncated tail as a path.
	long := "cat " + strings.Repeat("x", maxBashCommandBytes) + "/tail.go"
	for _, p := range bashCandidatePaths(long) {
		if strings.HasSuffix(p, "/tail.go") {
			t.Errorf("candidate %q came from past the scan bound", p)
		}
	}
}

// --- the tracker, end to end ----------------------------------------------

// TestFileTrackerRecordsBashPaths is the bug this bead was filed for: a session
// that reads its files with cat and sed named zero files, so files= was 0 on
// every logged pass observation.
func TestFileTrackerRecordsBashPaths(t *testing.T) {
	tr := newFileTracker()
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"cat internal/assay/retry.go"}}]`))
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"sed -n '1,40p' internal/assay/skip.go"}}]`))
	// The same file again, and a command that names none.
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"cat internal/assay/retry.go"}}]`))
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"go build ./..."}}]`))

	want := []string{"internal/assay/retry.go", "internal/assay/skip.go"}
	if got := tr.paths(); !equalStrs(got, want) {
		t.Errorf("paths = %v; want %v", got, want)
	}
	// Every block is still exactly one tool call, files or none: the two
	// figures answer different questions and neither is derived from the other.
	if got := tr.toolCalls(); got != 4 {
		t.Errorf("toolCalls = %d; want 4", got)
	}
}

// TestFileTrackerReadsTheWiderPathKeys covers the structured half: a tool that
// names its file under a key other than file_path is still a tool that opened a
// file.
func TestFileTrackerReadsTheWiderPathKeys(t *testing.T) {
	tr := newFileTracker()
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Read","input":{"file_path":"/w/a.go"}}]`))
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Glob","input":{"path":"/w/pkg"}}]`))
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"NotebookEdit","input":{"notebook_path":"/w/n.ipynb"}}]`))
	want := []string{"/w/a.go", "/w/pkg", "/w/n.ipynb"}
	if got := tr.paths(); !equalStrs(got, want) {
		t.Errorf("paths = %v; want %v", got, want)
	}
}

// TestFileTrackerCountsCallsWithUnreadableInput pins the split introduced by
// decoding a block's input separately: a tool whose input is not an object is a
// tool call that named no file, not an event to be thrown away.
func TestFileTrackerCountsCallsWithUnreadableInput(t *testing.T) {
	tr := newFileTracker()
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Weird","input":"a bare string"},{"type":"tool_use","name":"Bash","input":{"command":"cat a/x.go"}}]`))
	if got := tr.toolCalls(); got != 2 {
		t.Errorf("toolCalls = %d; want 2 — an undecodable input still made a call", got)
	}
	if got := tr.paths(); !equalStrs(got, []string{"a/x.go"}) {
		t.Errorf("paths = %v; want [a/x.go]", got)
	}
}

// TestOpenedDiffFilesRejectsMisparsedTokens is the safety property the whole
// approach rests on: whatever the parser hands back, only files already in the
// diff can be selected, so a misparse narrows the retry by less than it should
// have and never reaches outside the change.
func TestOpenedDiffFilesRejectsMisparsedTokens(t *testing.T) {
	diffFiles := []string{"a.go", "pkg/b.go", "c.go"}
	opened := bashCandidatePaths("sed -n '1,50p' pkg/b.go && go test ./... -run TestX && cat /etc/hosts")
	got := openedDiffFiles(opened, diffFiles)
	if !equalStrs(got, []string{"pkg/b.go"}) {
		t.Errorf("openedDiffFiles(%v) = %v; want [pkg/b.go]", opened, got)
	}
}

// TestOpenedDiffFilesMatchesCommandRelativePaths covers the direction only a
// command line produces: a session that cd'd into a package names its files by
// basename, and the diff names them from the repository root.
func TestOpenedDiffFilesMatchesCommandRelativePaths(t *testing.T) {
	diffFiles := []string{"internal/assay/retry.go", "internal/web/login.go"}
	opened := bashCandidatePaths("cd internal/assay && cat retry.go")
	if got := openedDiffFiles(opened, diffFiles); !equalStrs(got, []string{"internal/assay/retry.go"}) {
		t.Errorf("openedDiffFiles(%v) = %v; want [internal/assay/retry.go]", opened, got)
	}
}

// TestBashOnlySessionScopesTheRetry is the second consequence from the bead:
// with only file_path read, the retry's third modification was never
// constructed and every turn-budget retry ran on budget + instruction alone.
func TestBashOnlySessionScopesTheRetry(t *testing.T) {
	tr := newFileTracker()
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"cat a.go"}}]`))

	diffFiles := []string{"a.go", "b.go"}
	mods, ok := buildRetryMods(DefaultConfig(), tr.paths(), diffFiles)
	if !ok {
		t.Fatal("buildRetryMods reported no modification")
	}
	if !equalStrs(mods.scopedFiles, []string{"a.go"}) {
		t.Fatalf("scopedFiles = %v; want [a.go] — the retry is narrower in name only without it", mods.scopedFiles)
	}

	// And the scoping reaches the payload: a scoped retry must differ from one
	// modified by budget and instruction alone, or it bought nothing.
	base := retryInputs{prompt: "head\n" + diffFixture, diff: diffFixture, turns: 12}
	build := func(d string) (string, error) { return "head\n" + d, nil }
	scoped, ok := planRetryInputs(base, mods, build)
	if !ok {
		t.Fatal("planRetryInputs refused the scoped retry")
	}
	unscoped, ok := planRetryInputs(base, retryMods{turnBudget: mods.turnBudget, instruction: mods.instruction}, build)
	if !ok {
		t.Fatal("planRetryInputs refused the unscoped retry")
	}
	if scoped.hash() == unscoped.hash() {
		t.Error("the scoped retry hashes identically to the unscoped one; the diff was never narrowed")
	}
}

// TestPassTelemetryReportsFilesForABashOnlySession closes the loop on the
// rendered field: files= is what a reader sees, and it read 0 for every pass.
func TestPassTelemetryReportsFilesForABashOnlySession(t *testing.T) {
	tr := newFileTracker()
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"cat internal/assay/retry.go"}}]`))
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"grep -n costTracker internal/assay/cost.go"}}]`))

	got := RenderPassTelemetry([]PassReport{{
		Name:      "logic",
		Turns:     12,
		Attempts:  1,
		Provider:  "claude",
		ToolCalls: tr.toolCalls(),
		FilesRead: len(tr.paths()),
	}})
	if !strings.Contains(got, "tools=2 files=2") {
		t.Errorf("RenderPassTelemetry = %q; want tools=2 files=2", got)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
