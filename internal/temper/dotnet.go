package temper

import (
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// .NET detection used to ask filepath.Glob for `**/*.csproj`, which does not
// implement doublestar: `**` there is an ordinary `*`, so the pattern matched
// exactly one directory level and a repository laid out as
// `src/Api/Api.csproj` with no root solution was not detected as .NET at all.
// Temper then fell through to the "No build system detected" echo step and
// reported PASS over code it never built.
//
// The tree is walked instead, which is also what makes the steps runnable:
// `dotnet build` with no argument resolves a project or solution in its
// working directory and fails with MSB1003 when there is none, so a target
// found deeper has to be named on the command line.

// dotnetProjectExts are the project file extensions `dotnet build` accepts.
// F# and VB projects are included because `defaultDotnetPaths` already gates
// the .NET steps on them: detecting only C# would gate a build on a file that
// can never trigger one.
var dotnetProjectExts = map[string]bool{
	".csproj": true,
	".vbproj": true,
	".fsproj": true,
}

// dotnetSolutionExts are the solution formats `dotnet build` accepts.
var dotnetSolutionExts = map[string]bool{
	".sln":  true,
	".slnx": true,
}

// dotnetScanSkipDirs are directories never descended into while looking for
// .NET projects: build output (which holds copies of project files and
// generated MSBuild fragments), dependency trees, and Forge's own preview
// checkouts. A project found in any of them names a build nobody asked for.
var dotnetScanSkipDirs = map[string]bool{
	".git":         true,
	"bin":          true,
	"obj":          true,
	"node_modules": true,
	"vendor":       true,
	"packages":     true,
	".previews":    true,
}

// dotnetScanMaxDepth bounds how deep the scan descends, counting the worktree
// root as depth 0. Real solution layouts nest a handful of levels
// (`src/Services/Billing/Api/Billing.Api.csproj` is 4); the cap is what keeps
// the walk proportional to a repository rather than to whatever happens to be
// checked out beneath it.
const dotnetScanMaxDepth = 8

// maxProjectFileScanSize bounds the read of a project file while classifying
// it as a test project. Project files are small XML documents; anything past
// this is not one, and is treated as a non-test project rather than read.
const maxProjectFileScanSize = 1 << 20 // 1 MiB

// testProjectMarkers are the strings (matched lowercased) that identify a
// project `dotnet test` can run. A project carrying none of them is built but
// never tested: `dotnet test` on a project with no test framework exits
// non-zero, so testing every discovered project would turn a detected .NET
// repository into a guaranteed Temper failure.
var testProjectMarkers = []string{
	"<istestproject>true",
	"microsoft.net.test.sdk",
	"xunit",
	"nunit",
	"mstest",
	"tunit",
}

// dotnetLayout is what a scan of the worktree found: solution files and
// project files, both as slash-separated paths relative to the worktree root,
// each sorted shallowest-first so a choice among them is deterministic.
type dotnetLayout struct {
	solutions []string
	projects  []string
}

// empty reports whether the worktree holds no .NET project at all.
func (l dotnetLayout) empty() bool {
	return len(l.solutions) == 0 && len(l.projects) == 0
}

// scanDotnet walks worktreePath for solution and project files.
//
// It is best-effort: an unreadable directory is skipped rather than aborting
// the scan. Unlike the //go:embed scan — where a partial list silently gates
// a step on the wrong files — a partial list here can only mean a project
// that is not built, which is the behaviour every .NET repo had before this
// existed, and the alternative (dropping detection entirely because one
// directory could not be read) is strictly worse.
func scanDotnet(worktreePath string) dotnetLayout {
	var layout dotnetLayout
	if worktreePath == "" {
		return layout
	}

	err := filepath.WalkDir(worktreePath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read is skipped; the walk continues
			// with its siblings.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(worktreePath, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			if dotnetScanSkipDirs[strings.ToLower(d.Name())] {
				return fs.SkipDir
			}
			if strings.Count(rel, "/")+1 >= dotnetScanMaxDepth {
				return fs.SkipDir
			}
			return nil
		}
		// Only regular files are considered: a symlinked or device-node
		// `Api.csproj` is not something to hand to `dotnet build`, and the
		// classification below would otherwise open it.
		if !d.Type().IsRegular() {
			return nil
		}
		switch ext := strings.ToLower(filepath.Ext(rel)); {
		case dotnetSolutionExts[ext]:
			layout.solutions = append(layout.solutions, rel)
		case dotnetProjectExts[ext]:
			layout.projects = append(layout.projects, rel)
		}
		return nil
	})
	if err != nil {
		log.Printf("[temper] WARN could not scan %s for .NET projects (%v)", worktreePath, err)
	}

	sortShallowestFirst(layout.solutions)
	sortShallowestFirst(layout.projects)
	return layout
}

// sortShallowestFirst orders paths by depth and then lexically, so that the
// entry point chosen from a set is the same on every run and on every host.
func sortShallowestFirst(paths []string) {
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})
}

// dotnetSteps returns the auto-detected .NET build/test steps for a worktree,
// or nil when it holds no .NET project.
//
// The target is named explicitly unless it sits at the root, because that is
// the only place `dotnet` finds it on its own:
//
//   - a solution (the shallowest one) covers every project it references, so
//     it is preferred over the projects whenever one exists;
//   - failing that, each discovered project is built in its own step, and
//     only the ones carrying a test framework are also tested.
func dotnetSteps(worktreePath string) []Step {
	layout := scanDotnet(worktreePath)
	if layout.empty() {
		return nil
	}

	if len(layout.solutions) > 0 {
		return dotnetTargetSteps("", layout.solutions[0], true)
	}

	// A project at the root is the one case `dotnet build` resolves by
	// itself, and the whole worktree is its scope — so it is built alone
	// even if projects sit deeper too, exactly as before this scan existed.
	if isRootPath(layout.projects[0]) {
		return dotnetTargetSteps("", layout.projects[0], isTestProject(worktreePath, layout.projects[0]))
	}

	var steps []Step
	for _, proj := range layout.projects {
		label := strings.TrimSuffix(proj, filepath.Ext(proj))
		steps = append(steps, dotnetTargetSteps(label+":", proj, isTestProject(worktreePath, proj))...)
	}
	if len(layout.projects) > 1 {
		log.Printf("[temper] %s has %d .NET projects and no solution file — building each one separately",
			worktreePath, len(layout.projects))
	}
	return steps
}

// dotnetTargetSteps builds the build (and, when the target is testable, test)
// steps for one solution or project.
//
// The target is passed as an argument rather than by setting Step.Dir, since
// a directory holding two project files is ambiguous to `dotnet build` while
// a named file never is. A target at the worktree root is passed no argument
// at all, which keeps the steps of an ordinary single-solution repository
// byte-identical to the ones Temper has always generated.
func dotnetTargetSteps(prefix, target string, testable bool) []Step {
	var arg []string
	if !isRootPath(target) {
		arg = []string{filepath.FromSlash(target)}
	}

	steps := []Step{{
		Name:    prefix + "build",
		Command: "dotnet",
		Args:    append([]string{"build", "--no-restore"}, arg...),
		Timeout: 3 * time.Minute,
		Paths:   defaultDotnetPaths,
	}}
	if !testable {
		return steps
	}
	return append(steps, Step{
		Name:    prefix + "test",
		Command: "dotnet",
		Args:    append([]string{"test", "--no-build"}, arg...),
		Timeout: 5 * time.Minute,
		Paths:   defaultDotnetPaths,
	})
}

// isRootPath reports whether a relative slash path names a file in the
// worktree root.
func isRootPath(rel string) bool {
	return !strings.Contains(rel, "/")
}

// isTestProject reports whether a project file references a test framework.
//
// The question is asked of the project file rather than of its name because
// the answer decides whether `dotnet test` runs against it, and `dotnet test`
// on a project with no test framework fails. A file that cannot be read is
// reported as a non-test project: not testing a test project loses coverage
// the build step still partly covers, while testing a library guarantees a
// failed verification on every run.
func isTestProject(worktreePath, rel string) bool {
	f, err := os.Open(filepath.Join(worktreePath, filepath.FromSlash(rel)))
	if err != nil {
		return false
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxProjectFileScanSize))
	if err != nil {
		return false
	}
	content := strings.ToLower(string(data))
	for _, marker := range testProjectMarkers {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}
