package temper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// nodeDepsCase makes an anvil whose client/node_modules was installed at installed, and a worktree under
// <anvil>/.workers whose client/package-lock.json locks locked; npm is a fake that records where it ran.
func nodeDepsCase(t *testing.T, installed, locked string, npmExit int) (anvil, wt string, ran func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as npm")
	}
	anvil = t.TempDir()
	wt = filepath.Join(anvil, ".workers", "B-1")
	nm := filepath.Join(anvil, "client", "node_modules")
	for _, dir := range []string{nm, filepath.Join(wt, "client")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lock := func(v string) string { return fmt.Sprintf(`{"packages":{"":{},"node_modules/a":{"version":"%s"}}}`, v) }
	must(t, os.WriteFile(filepath.Join(nm, ".package-lock.json"), []byte(`{"packages":{"node_modules/a":{"version":"`+installed+`"}}}`), 0o644))
	must(t, os.WriteFile(filepath.Join(wt, "client", "package.json"), []byte(`{"name":"client"}`), 0o644))
	must(t, os.WriteFile(filepath.Join(wt, "client", "package-lock.json"), []byte(lock(locked)), 0o644))
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "npm.log")
	must(t, os.WriteFile(filepath.Join(bin, "npm"), []byte(fmt.Sprintf("#!/bin/sh\necho \"$PWD $*\" >> '%s'\nexit %d\n", log, npmExit)), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return anvil, wt, func() []string {
		raw, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func installStep() Step {
	return Step{Name: "client-install", Command: "npm", Args: []string{"ci", "--prefer-offline"}, Dir: "client"}
}

func TestTemperReinstallsAStaleLinkBeforeVerifying(t *testing.T) {
	anvil, wt, ran := nodeDepsCase(t, "1.0.0", "2.0.0", 0)
	must(t, os.Symlink(filepath.Join(anvil, "client", "node_modules"), filepath.Join(wt, "client", "node_modules")))
	res := Run(context.Background(), wt, Config{Steps: []Step{installStep()}}, nil, "B-1", "a")
	if !res.Passed {
		t.Fatalf("temper failed: %+v", res)
	}
	if linked := isNodeModulesLinked(filepath.Join(wt, "client")); linked {
		t.Error("the stale link must be gone")
	}
	if _, err := os.Stat(filepath.Join(anvil, "client", "node_modules", ".package-lock.json")); err != nil {
		t.Errorf("the anvil's tree must stay: %v", err)
	}
	if calls := ran(); len(calls) != 1 || !strings.HasSuffix(strings.Fields(calls[0])[0], filepath.Join("B-1", "client")) {
		t.Errorf("npm ran %q, want once in the worktree's client", calls)
	}
	if res.Steps[0].Name != "node-install:client" || res.Steps[1].SkipReason != SkipReasonFreshInstall {
		t.Errorf("steps = %+v", res.Steps)
	}
}

func TestTemperLeavesAMatchingLinkAlone(t *testing.T) {
	anvil, wt, ran := nodeDepsCase(t, "2.0.0", "2.0.0", 0)
	must(t, os.Symlink(filepath.Join(anvil, "client", "node_modules"), filepath.Join(wt, "client", "node_modules")))
	Run(context.Background(), wt, Config{}, nil, "B-1", "a")
	if !isNodeModulesLinked(filepath.Join(wt, "client")) || ran()[0] != "" {
		t.Errorf("a matching link must stay and nothing install: %q", ran())
	}
}

func TestTemperInstallsAMissingTreeOnlyWhereTheAnvilHasOne(t *testing.T) {
	_, wt, ran := nodeDepsCase(t, "1.0.0", "2.0.0", 0)
	Run(context.Background(), wt, Config{}, nil, "B-1", "a")
	if len(ran()) != 1 || ran()[0] == "" {
		t.Errorf("a missing tree where the anvil has one is installed: %q", ran())
	}
	anvil2, wt2, ran2 := nodeDepsCase(t, "1.0.0", "2.0.0", 0)
	must(t, os.RemoveAll(filepath.Join(anvil2, "client", "node_modules")))
	Run(context.Background(), wt2, Config{}, nil, "B-1", "a")
	if ran2()[0] != "" {
		t.Errorf("no install where the anvil's project has none: %q", ran2())
	}
}

func TestTemperStopsWhenTheInstallFails(t *testing.T) {
	_, wt, _ := nodeDepsCase(t, "1.0.0", "2.0.0", 1)
	res := Run(context.Background(), wt, Config{Steps: []Step{{Name: "lint", Command: "true"}}}, nil, "B-1", "a")
	if res.Passed || res.FailedStep != "node-install:client" || len(res.Steps) != 1 {
		t.Errorf("an install that fails stops verification: %+v", res)
	}
}
