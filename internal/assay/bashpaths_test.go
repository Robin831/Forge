package assay

import (
	"slices"
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
			// The command word is dropped exactly once per segment. If the
			// leading assignment consumed that drop, the script itself — which
			// IS path-shaped — would be reported as a file the session read.
			name: "a leading env assignment does not consume the command-word drop",
			cmd:  "LC_ALL=C ./scripts/check.sh internal/assay/skip.go",
			want: []string{"internal/assay/skip.go"},
		},
		{
			// git's rev:path syntax names a file the session read, and the
			// revision prefix is not part of any path a diff can name: left on,
			// the candidate matched nothing and the file was dropped from the
			// retry's diff (see TestGitRevisionReadsReachTheRetryScope).
			name: "a git revision path is reduced to the path it names",
			cmd:  "LC_ALL=C git show HEAD:internal/assay/skip.go",
			want: []string{"internal/assay/skip.go"},
		},
		{
			// A colon after a separator belongs to the filename, and a drive
			// letter is not a revision.
			name: "a colon that is not a revision prefix is left alone",
			cmd:  "cat C:/w/a.go dir/od:d.go",
			want: []string{"C:/w/a.go", "dir/od:d.go"},
		},
		{
			// The one redirection whose target the command READS. Treated as a
			// segment separator it became the next command word and was lost.
			name: "an input redirection names a file the command read",
			cmd:  "cat < internal/assay/f.go",
			want: []string{"internal/assay/f.go"},
		},
		{
			// A shell's quoted argument is a command line, not data.
			name: "a quoted sub-command is followed one level down",
			cmd:  `bash -c "sed -n 1,20p a/b.go && cat c/d.go"`,
			want: []string{"a/b.go", "c/d.go"},
		},
		{
			// Without backslash handling the escaped quote closed the string,
			// after which `;` split a segment inside what the model intended as
			// quoted text and the tokens after it landed in the wrong roles.
			name: "an escaped quote does not desynchronise the rest of the line",
			cmd:  `echo "a \" b" ; cat x/y.go`,
			want: []string{"x/y.go"},
		},
		{
			// A backslash that escapes nothing is the character it is: a
			// Windows path unescaped to `C:wa.go` would match nothing, and the
			// comparison is normalized for that spelling.
			name: "a backslash that escapes nothing stays in the path",
			cmd:  `cat C:\w\a.go`,
			want: []string{`C:\w\a.go`},
		},
		{
			// An escaped space makes one token rather than two, which is what
			// keeps the rest of the line's roles right; the token itself is
			// still dropped, since embedded whitespace is far more often a
			// quoted message than a filename.
			name: "an escaped space does not split the token",
			cmd:  `cat internal/assay/my\ file.go x/y.go`,
			want: []string{"x/y.go"},
		},
		{
			// An empty quoted argument used to emit a zero-length token, which
			// segmentArguments then consumed as the command word — promoting
			// the real one, path-shaped, to a file the session read.
			name: "an empty quoted argument does not shift the command word",
			cmd:  "grep '' ./scripts/x.sh",
			want: []string{"./scripts/x.sh"},
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
			if !slices.Equal(got, tc.want) {
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
	// The cap holds across the nested parse too: a quoted sub-command must not
	// be a way to hand back more candidates than one command may yield.
	nested := "bash -c \"cat" + strings.Repeat(" pkg/x.go pkg/y.go", maxBashCandidates) + "\""
	if got := len(bashCandidatePaths(nested)); got > maxBashCandidates {
		t.Errorf("nested candidates = %d; cap is %d", got, maxBashCandidates)
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
	if got := tr.paths(); !slices.Equal(got, want) {
		t.Errorf("paths = %v; want %v", got, want)
	}
	// Every block is still exactly one tool call, files or none: the two
	// figures answer different questions and neither is derived from the other.
	if got := tr.toolCalls(); got != 4 {
		t.Errorf("toolCalls = %d; want 4", got)
	}
}

// TestFilesReadCountsFilesNotEverythingNamed is the honesty of the telemetry
// field: files= is read as "this pass went and looked at code", so a directory
// argument or a fragment of a quoted script must not raise it. Both stay in the
// tracked list, where the retry's scoping is free to match them against nothing.
func TestFilesReadCountsFilesNotEverythingNamed(t *testing.T) {
	tr := newFileTracker()
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"cd internal/assay && go test ./..."}}]`))
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"grep -rn costTracker internal/"}}]`))
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"python3 -c \"print(open('internal/assay/g.go').read())\""}}]`))
	if got := countFilesRead(tr.paths()); got != 0 {
		t.Errorf("countFilesRead(%v) = %d; want 0 — none of those commands opened a file", tr.paths(), got)
	}

	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"cat internal/assay/retry.go"}}]`))
	if got := countFilesRead(tr.paths()); got != 1 {
		t.Errorf("countFilesRead(%v) = %d; want 1", tr.paths(), got)
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
	if got := tr.paths(); !slices.Equal(got, []string{"a/x.go"}) {
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
	if !slices.Equal(got, []string{"pkg/b.go"}) {
		t.Errorf("openedDiffFiles(%v) = %v; want [pkg/b.go]", opened, got)
	}
}

// TestGitRevisionReadsReachTheRetryScope carries the `git show <rev>:<path>`
// form all the way to the retry's diff, which is the only place its handling
// can be judged. As a bare candidate the revspec looked admitted and harmless;
// against a diff it matched nothing — the character before the path is a colon
// rather than a separator — so the retry's third modification was still dead
// for a session that read its files the way this one does.
func TestGitRevisionReadsReachTheRetryScope(t *testing.T) {
	diffFiles := []string{"internal/assay/skip.go", "internal/assay/retry.go", "x.go"}
	opened := bashCandidatePaths("LC_ALL=C git show HEAD:internal/assay/skip.go")
	if got := openedDiffFiles(opened, diffFiles); !slices.Equal(got, []string{"internal/assay/skip.go"}) {
		t.Fatalf("openedDiffFiles(%v) = %v; want [internal/assay/skip.go]", opened, got)
	}
	mods, ok := buildRetryMods(DefaultConfig(), opened, diffFiles)
	if !ok {
		t.Fatal("buildRetryMods reported no modification")
	}
	if !slices.Equal(mods.scopedFiles, []string{"internal/assay/skip.go"}) {
		t.Errorf("scopedFiles = %v; want [internal/assay/skip.go]", mods.scopedFiles)
	}
}

// TestOpenedDiffFilesMatchesCommandRelativePaths covers the direction only a
// command line produces: a session that cd'd into a package names its files by
// basename, and the diff names them from the repository root.
func TestOpenedDiffFilesMatchesCommandRelativePaths(t *testing.T) {
	diffFiles := []string{"internal/assay/retry.go", "internal/web/login.go"}
	opened := bashCandidatePaths("cd internal/assay && cat retry.go")
	if got := openedDiffFiles(opened, diffFiles); !slices.Equal(got, []string{"internal/assay/retry.go"}) {
		t.Errorf("openedDiffFiles(%v) = %v; want [internal/assay/retry.go]", opened, got)
	}
}

// TestOpenedDiffFilesRefusesAnAmbiguousBasename is the other half of that arm,
// and the one that decides what it may cost. A bare basename carries no
// directory, and basenames repeat across packages in this repository alone
// (retry.go, cost.go, skip.go, url.go), so a token matching two diff paths
// identifies neither. Selecting both would scope the retry to a file the
// session never opened — the narrowing silently pointed at the wrong code — so
// an ambiguous token selects nothing, and a session with no other evidence
// loses its scoping instead.
func TestOpenedDiffFilesRefusesAnAmbiguousBasename(t *testing.T) {
	opened := bashCandidatePaths("cd internal/assay && cat retry.go")

	collide := []string{"internal/assay/retry.go", "internal/web/retry.go", "internal/assay/skip.go"}
	if got := openedDiffFiles(opened, collide); got != nil {
		t.Errorf("openedDiffFiles(%v, %v) = %v; want none — neither retry.go is identified", opened, collide, got)
	}

	// And where the ambiguous pair IS the whole diff, the answer is the same
	// one an empty selection has always produced: no scoping, so the retry
	// keeps the diff it was given.
	both := []string{"internal/assay/retry.go", "internal/web/retry.go"}
	if got := openedDiffFiles(opened, both); got != nil {
		t.Errorf("openedDiffFiles(%v, %v) = %v; want none", opened, both, got)
	}

	// An unambiguous token in the same session is unaffected: ambiguity is
	// judged per candidate, not per session.
	opened = append(opened, "internal/web/login.go")
	if got := openedDiffFiles(opened, append(collide, "internal/web/login.go")); !slices.Equal(got, []string{"internal/web/login.go"}) {
		t.Errorf("openedDiffFiles(%v) = %v; want [internal/web/login.go]", opened, got)
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
	if !slices.Equal(mods.scopedFiles, []string{"a.go"}) {
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
		FilesRead: countFilesRead(tr.paths()),
	}})
	if !strings.Contains(got, "tools=2 files=2") {
		t.Errorf("RenderPassTelemetry = %q; want tools=2 files=2", got)
	}
}
