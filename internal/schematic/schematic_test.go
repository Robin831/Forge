package schematic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disableDepRetrySleep replaces the real sleep between dep-add retries with a
// no-op for the duration of the test and restores the original function via
// t.Cleanup.
func disableDepRetrySleep(t *testing.T) {
	t.Helper()
	orig := depRetrySleep
	depRetrySleep = func(time.Duration) {}
	t.Cleanup(func() { depRetrySleep = orig })
}

// exitError1 returns a real *exec.ExitError with exit code 1 by running "false".
func exitError1(t *testing.T) *exec.ExitError {
	t.Helper()
	err := exec.Command("false").Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	return exitErr
}

func TestShouldRun_DisabledConfig(t *testing.T) {
	cfg := Config{Enabled: false, WordThreshold: 10}
	bead := poller.Bead{Description: "a long description with many many many many many many words here"}
	assert.False(t, ShouldRun(cfg, bead))
}

func TestShouldRun_BelowThreshold(t *testing.T) {
	cfg := Config{Enabled: true, WordThreshold: 100}
	bead := poller.Bead{Description: "Short description"}
	assert.False(t, ShouldRun(cfg, bead))
}

func TestShouldRun_AboveThreshold(t *testing.T) {
	cfg := Config{Enabled: true, WordThreshold: 5}
	bead := poller.Bead{Description: "This is a description with more than five words in it"}
	assert.True(t, ShouldRun(cfg, bead))
}

func TestShouldRun_DecomposeTag(t *testing.T) {
	cfg := Config{Enabled: true, WordThreshold: 1000} // high threshold
	bead := poller.Bead{
		Description: "Short",
		Labels:      []string{"feature", "decompose", "urgent"},
	}
	assert.True(t, ShouldRun(cfg, bead), "decompose tag should override threshold")
}

func TestShouldRun_DecomposeTagCaseInsensitive(t *testing.T) {
	cfg := Config{Enabled: true, WordThreshold: 1000}
	bead := poller.Bead{
		Description: "Short",
		Labels:      []string{"Decompose"},
	}
	assert.True(t, ShouldRun(cfg, bead))
}

func TestParseVerdict_JSONFence(t *testing.T) {
	output := `Here is my analysis:

` + "```json" + `
{
  "action": "plan",
  "plan": "1. Create foo.go\n2. Add tests",
  "reason": "Single focused task"
}
` + "```" + `

That's my verdict.`

	v, err := parseVerdict(output)
	require.NoError(t, err)
	assert.Equal(t, "plan", v.Action)
	assert.Contains(t, v.Plan, "Create foo.go")
	assert.Equal(t, "Single focused task", v.Reason)
}

func TestParseVerdict_PlainFence(t *testing.T) {
	output := "```\n" + `{"action":"decompose","sub_tasks":[{"title":"Task A","description":"Detailed desc A"},{"title":"Task B","description":"Detailed desc B"}],"reason":"Too large"}` + "\n```"

	v, err := parseVerdict(output)
	require.NoError(t, err)
	assert.Equal(t, "decompose", v.Action)
	require.Len(t, v.SubTasks, 2)
	assert.Equal(t, "Task A", v.SubTasks[0].Title)
	assert.Equal(t, "Detailed desc A", v.SubTasks[0].Description)
	assert.Equal(t, "Task B", v.SubTasks[1].Title)
	assert.Equal(t, "Detailed desc B", v.SubTasks[1].Description)
}

func TestParseVerdict_LegacySubTasksStringArray(t *testing.T) {
	output := "```\n" + `{"action":"decompose","sub_tasks":["Task A","Task B"],"reason":"Too large"}` + "\n```"

	v, err := parseVerdict(output)
	require.NoError(t, err)
	assert.Equal(t, "decompose", v.Action)
	require.Len(t, v.SubTasks, 2)
	assert.Equal(t, "Task A", v.SubTasks[0].Title)
	assert.Equal(t, "", v.SubTasks[0].Description)
	assert.Equal(t, "Task B", v.SubTasks[1].Title)
	assert.Equal(t, "", v.SubTasks[1].Description)
}

func TestParseVerdict_RawJSON(t *testing.T) {
	output := `I think this needs decomposition.
{"action":"clarify","reason":"Missing acceptance criteria"}
That's all.`

	v, err := parseVerdict(output)
	require.NoError(t, err)
	assert.Equal(t, "clarify", v.Action)
	assert.Equal(t, "Missing acceptance criteria", v.Reason)
}

func TestParseVerdict_NoJSON(t *testing.T) {
	output := "I couldn't determine the right approach for this bead."
	_, err := parseVerdict(output)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no valid schematic verdict")
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.False(t, cfg.Enabled)
	assert.Equal(t, 100, cfg.WordThreshold)
	assert.Equal(t, 5, cfg.MaxTurns)
}

func TestBuildPrompt_ContainsBeadInfo(t *testing.T) {
	bead := poller.Bead{
		ID:          "test-123",
		Title:       "Add login feature",
		IssueType:   "feature",
		Priority:    2,
		Description: "Implement OAuth login flow",
	}

	p := buildPrompt(bead)
	assert.Contains(t, p, "test-123")
	assert.Contains(t, p, "Add login feature")
	assert.Contains(t, p, "Implement OAuth login flow")
	assert.Contains(t, p, "plan|decompose|clarify")
	assert.Contains(t, p, "Do NOT use any tools", "prompt must instruct AI not to use tools")
	assert.Contains(t, p, "FIRST response", "prompt must require JSON in first response")
}

func TestParseCrucibleVerdict_JSONFence(t *testing.T) {
	output := "```json\n" + `{"needs_crucible": true, "reason": "Children modify same files"}` + "\n```"
	v, err := parseCrucibleVerdict(output)
	require.NoError(t, err)
	assert.True(t, v.NeedsCrucible)
	assert.Equal(t, "Children modify same files", v.Reason)
}

func TestParseCrucibleVerdict_False(t *testing.T) {
	output := `{"needs_crucible": false, "reason": "Independent tasks"}`
	v, err := parseCrucibleVerdict(output)
	require.NoError(t, err)
	assert.False(t, v.NeedsCrucible)
}

func TestParseCrucibleVerdict_NoJSON(t *testing.T) {
	_, err := parseCrucibleVerdict("No structured output here")
	assert.Error(t, err)
}

func TestBuildCruciblePrompt(t *testing.T) {
	parent := poller.Bead{
		ID:          "parent-1",
		Title:       "Auth system",
		IssueType:   "feature",
		Description: "Implement full auth",
	}
	children := []ChildBead{
		{ID: "child-1", Title: "Login page", Description: "Build login UI"},
		{ID: "child-2", Title: "Session mgmt", Description: "Cookie handling"},
	}
	p := buildCruciblePrompt(parent, children)
	assert.Contains(t, p, "parent-1")
	assert.Contains(t, p, "Auth system")
	assert.Contains(t, p, "child-1")
	assert.Contains(t, p, "Login page")
	assert.Contains(t, p, "child-2")
	assert.Contains(t, p, "needs_crucible")
}

// fakeRunner records bd invocations and returns pre-configured responses.
// It is safe to use from parallel tests.
type fakeRunner struct {
	mu       sync.Mutex
	calls    [][]string // each entry is the args slice for one call
	response func(args []string) ([]byte, error)
}

func (f *fakeRunner) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
	return f.response(args)
}

// newFakeRunner builds a runner whose "bd create" calls return sequential IDs,
// "bd show --json" calls return dependency data, and all other calls succeed.
func newFakeRunner() *fakeRunner {
	return newFakeRunnerWithDeps(nil, nil)
}

// newFakeRunnerWithDeps builds a fake runner that returns the given upstream
// (dependencies) and downstream (dependents) IDs when "bd show --json" is called.
func newFakeRunnerWithDeps(upstreamIDs, downstreamIDs []string) *fakeRunner {
	var idCounter int
	var mu sync.Mutex
	return &fakeRunner{
		response: func(args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "create" {
				mu.Lock()
				idCounter++
				id := fmt.Sprintf("test-%d", idCounter)
				mu.Unlock()
				return []byte(fmt.Sprintf(`{"id":%q}`, id)), nil
			}
			// bd show <id> --json → return deps/dependents JSON.
			//
			// The dependents half is withheld unless the call carries
			// --include-dependents, which is what bd itself does (verified
			// against 1.1.2): unflagged it reports a dependent_count and omits
			// the array. Without that discrimination a re-fetch that lost the
			// flag would still look correct here while silently dropping the
			// parent's downstream blocks in production.
			if len(args) >= 2 && args[0] == "show" && slices.Contains(args, "--json") {
				var deps, dependents string
				for _, id := range upstreamIDs {
					if deps != "" {
						deps += ","
					}
					deps += fmt.Sprintf(`{"id":%q}`, id)
				}
				if slices.Contains(args, executil.BdIncludeDependentsFlag) {
					for _, id := range downstreamIDs {
						if dependents != "" {
							dependents += ","
						}
						dependents += fmt.Sprintf(`{"id":%q}`, id)
					}
					return []byte(fmt.Sprintf(`[{"dependencies":[%s],"dependents":[%s]}]`, deps, dependents)), nil
				}
				return []byte(fmt.Sprintf(`[{"dependencies":[%s],"dependent_count":%d}]`,
					deps, len(downstreamIDs))), nil
			}
			// dep add, update, close, etc. succeed silently.
			return []byte("ok"), nil
		},
	}
}

func TestCreateSubBeads_SequentialDepsAdded(t *testing.T) {
	fake := newFakeRunner()
	parent := poller.Bead{ID: "parent-1", Title: "Big feature", Priority: 2}
	tasks := []subTaskVerdict{
		{Title: "Task A", Description: "Detailed description for Task A"},
		{Title: "Task B", Description: "Detailed description for Task B"},
		{Title: "Task C", Description: "Detailed description for Task C"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err)
	require.Len(t, subs, 3)

	// Verify the IDs are set.
	assert.NotEmpty(t, subs[0].ID)
	assert.NotEmpty(t, subs[1].ID)
	assert.NotEmpty(t, subs[2].ID)

	// Count dep add calls: expect N-1 = 2 for 3 tasks.
	depCalls := 0
	for _, call := range fake.calls {
		if len(call) >= 1 && call[0] == "dep" {
			depCalls++
		}
	}
	assert.Equal(t, 2, depCalls, "expected one bd dep add per consecutive pair")

	// Verify ordering: dep add <child2> <child1>, dep add <child3> <child2>.
	depArgs := [][]string{}
	for _, call := range fake.calls {
		if len(call) >= 1 && call[0] == "dep" {
			depArgs = append(depArgs, call)
		}
	}
	require.Len(t, depArgs, 2)
	// depArgs[0] = ["dep", "add", <child2-id>, <child1-id>]
	assert.Equal(t, subs[1].ID, depArgs[0][2], "second child should depend on first")
	assert.Equal(t, subs[0].ID, depArgs[0][3])
	assert.Equal(t, subs[2].ID, depArgs[1][2], "third child should depend on second")
	assert.Equal(t, subs[1].ID, depArgs[1][3])
}

func TestCreateSubBeads_DepAddFailureIsFatal(t *testing.T) {
	var idCounter int
	var mu sync.Mutex
	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "create" {
				mu.Lock()
				idCounter++
				id := fmt.Sprintf("test-%d", idCounter)
				mu.Unlock()
				return []byte(fmt.Sprintf(`{"id":%q}`, id)), nil
			}
			if len(args) > 0 && args[0] == "dep" {
				return nil, errors.New("bd dep add: connection refused")
			}
			return []byte("ok"), nil
		},
	}

	parent := poller.Bead{ID: "parent-1", Title: "Feature", Priority: 2}
	tasks := []subTaskVerdict{
		{Title: "Task A", Description: "Desc A"},
		{Title: "Task B", Description: "Desc B"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	// Must return an error so the caller can escalate to ActionClarify.
	require.Error(t, err, "dep add failure must be fatal")
	assert.Contains(t, err.Error(), "sequential dependency chaining failed")
	// Partial sub-beads should be returned for operator visibility.
	assert.NotEmpty(t, subs, "partial sub-beads should be returned even on dep add failure")
}

func TestCreateSubBeads_DepAddNonZeroButAdded(t *testing.T) {
	depErr := exitError1(t) // real *exec.ExitError with exit code 1
	var idCounter int
	var mu sync.Mutex
	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "create" {
				mu.Lock()
				idCounter++
				id := fmt.Sprintf("test-%d", idCounter)
				mu.Unlock()
				return []byte(fmt.Sprintf(`{"id":%q}`, id)), nil
			}
			if len(args) > 0 && args[0] == "dep" {
				// bd dep add exits non-zero but stdout confirms the dep was added.
				return []byte("✓ Added dependency: test-2 depends on test-1 (blocks)"), depErr
			}
			return []byte("ok"), nil
		},
	}

	parent := poller.Bead{ID: "parent-1", Title: "Feature", Priority: 2}
	tasks := []subTaskVerdict{
		{Title: "Task A", Description: "Desc A"},
		{Title: "Task B", Description: "Desc B"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	// Should succeed because the output confirms the dependency was added.
	require.NoError(t, err, "dep add with success marker in output should not be fatal")
	assert.Len(t, subs, 2)
}

func TestCreateSubBeads_SingleTaskNoDep(t *testing.T) {
	fake := newFakeRunner()
	parent := poller.Bead{ID: "parent-1", Title: "Simple task", Priority: 1}

	subs, err := createSubBeads(context.Background(), parent, []subTaskVerdict{{Title: "Only task", Description: "Single task desc"}}, "/tmp", fake.run)
	require.NoError(t, err)
	require.Len(t, subs, 1)

	// No dep add should be issued for a single task.
	for _, call := range fake.calls {
		if len(call) > 0 && call[0] == "dep" {
			t.Errorf("unexpected dep add call for single-task decomposition: %v", call)
		}
	}
}

func TestCreateSubBeads_NoTasks(t *testing.T) {
	fake := newFakeRunner()
	_, err := createSubBeads(context.Background(), poller.Bead{ID: "p"}, nil, "/tmp", fake.run)
	require.Error(t, err)
}

func TestCreateSubBeads_BlocksTransferToLastSubBead(t *testing.T) {
	fake := newFakeRunnerWithDeps(nil, []string{"downstream-c"})
	parent := poller.Bead{
		ID:       "parent-1",
		Title:    "Feature B",
		Priority: 2,
		Blocks:   []string{"downstream-c"},
	}
	tasks := []subTaskVerdict{
		{Title: "B1", Description: "First sub-task"},
		{Title: "B2", Description: "Second sub-task"},
		{Title: "B3", Description: "Third sub-task"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err)
	require.Len(t, subs, 3)

	lastSubID := subs[2].ID

	// Find the dep add call that transfers the Blocks relationship:
	// dep add downstream-c <last-sub-id>
	found := false
	for _, call := range fake.calls {
		if len(call) == 4 && call[0] == "dep" && call[1] == "add" &&
			call[2] == "downstream-c" && call[3] == lastSubID {
			found = true
			break
		}
	}
	assert.True(t, found, "expected dep add downstream-c %s (Blocks transfer to last sub-bead)", lastSubID)
}

func TestCreateSubBeads_DependsOnTransferToFirstSubBead(t *testing.T) {
	fake := newFakeRunnerWithDeps([]string{"upstream-a"}, nil)
	parent := poller.Bead{
		ID:        "parent-1",
		Title:     "Feature B",
		Priority:  2,
		DependsOn: []string{"upstream-a"},
	}
	tasks := []subTaskVerdict{
		{Title: "B1", Description: "First sub-task"},
		{Title: "B2", Description: "Second sub-task"},
		{Title: "B3", Description: "Third sub-task"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err)
	require.Len(t, subs, 3)

	firstSubID := subs[0].ID

	// Find the dep add call that transfers the DependsOn relationship:
	// dep add <first-sub-id> upstream-a
	found := false
	for _, call := range fake.calls {
		if len(call) == 4 && call[0] == "dep" && call[1] == "add" &&
			call[2] == firstSubID && call[3] == "upstream-a" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected dep add %s upstream-a (DependsOn transfer to first sub-bead)", firstSubID)
}

func TestCreateSubBeads_FullChainTransfer(t *testing.T) {
	fake := newFakeRunnerWithDeps([]string{"upstream-a"}, []string{"downstream-c"})
	parent := poller.Bead{
		ID:        "parent-b",
		Title:     "Feature B (middle of chain)",
		Priority:  2,
		DependsOn: []string{"upstream-a"},
		Blocks:    []string{"downstream-c"},
	}
	tasks := []subTaskVerdict{
		{Title: "B1", Description: "First"},
		{Title: "B2", Description: "Second"},
		{Title: "B3", Description: "Third"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err)
	require.Len(t, subs, 3)

	firstSubID := subs[0].ID
	lastSubID := subs[2].ID

	// Verify DependsOn transfer: dep add <B1> upstream-a
	foundDependsOn := false
	// Verify Blocks transfer: dep add downstream-c <B3>
	foundBlocks := false
	for _, call := range fake.calls {
		if len(call) == 4 && call[0] == "dep" && call[1] == "add" {
			if call[2] == firstSubID && call[3] == "upstream-a" {
				foundDependsOn = true
			}
			if call[2] == "downstream-c" && call[3] == lastSubID {
				foundBlocks = true
			}
		}
	}

	assert.True(t, foundDependsOn, "DependsOn should transfer to first sub-bead %s", firstSubID)
	assert.True(t, foundBlocks, "Blocks should transfer to last sub-bead %s", lastSubID)
}

func TestCreateSubBeads_ParentClosedAfterDecomposition(t *testing.T) {
	fake := newFakeRunner()
	parent := poller.Bead{ID: "parent-close", Title: "Parent to decompose", Priority: 2}
	tasks := []subTaskVerdict{
		{Title: "Sub A", Description: "First sub-task"},
		{Title: "Sub B", Description: "Second sub-task"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err)
	require.Len(t, subs, 2)

	// Verify that bd close was called for the parent with --force and an appropriate --reason.
	foundClose := false
	var closeArgs []string
	for _, call := range fake.calls {
		if len(call) >= 3 && call[0] == "close" && call[1] == parent.ID {
			foundClose = true
			closeArgs = call[2:]
			break
		}
	}
	require.True(t, foundClose, "parent bead should be closed after successful decomposition")

	// The close command should be forced.
	assert.Contains(t, closeArgs, "--force", "parent bead should be closed with --force after successful decomposition")

	// The close reason should be provided and should mention the created sub-beads.
	reasonIdx := -1
	for i, arg := range closeArgs {
		if arg == "--reason" {
			reasonIdx = i
			break
		}
	}
	require.NotEqual(t, -1, reasonIdx, "bd close should be called with --reason for parent bead")
	require.Greater(t, len(closeArgs), reasonIdx+1, "bd close --reason flag should have an accompanying value")
	reason := closeArgs[reasonIdx+1]

	// Reason should include each sub-bead ID.
	for _, sub := range subs {
		assert.Contains(t, reason, sub.ID, "close reason should mention sub-bead ID %q", sub.ID)
	}

	// Reason should also mention the number of created sub-beads.
	assert.Contains(t, reason, fmt.Sprintf("%d", len(subs)), "close reason should mention the count of created sub-beads")
}

func TestCreateSubBeads_ParentCloseFailureDoesNotFailDecomposition(t *testing.T) {
	var idCounter int
	var mu sync.Mutex
	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "create" {
				mu.Lock()
				idCounter++
				id := fmt.Sprintf("test-%d", idCounter)
				mu.Unlock()
				return []byte(fmt.Sprintf(`{"id":%q}`, id)), nil
			}
			if len(args) > 0 && args[0] == "close" {
				return []byte("error: cannot close"), fmt.Errorf("bd close failed")
			}
			return []byte("ok"), nil
		},
	}
	parent := poller.Bead{ID: "parent-close-fail", Title: "Parent that fails to close", Priority: 2}
	tasks := []subTaskVerdict{
		{Title: "Sub A", Description: "First sub-task"},
	}

	// Decomposition should succeed even if closing the parent fails.
	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err, "decomposition should succeed even when bd close fails")
	require.Len(t, subs, 1)
}

func TestCreateSubBeads_SingleSubBeadInheritsChain(t *testing.T) {
	fake := newFakeRunnerWithDeps([]string{"upstream-a"}, []string{"downstream-c"})
	// When only one sub-bead is created, it is both first and last,
	// so it should inherit both DependsOn and Blocks from the parent.
	parent := poller.Bead{
		ID:        "parent-b",
		Priority:  2,
		DependsOn: []string{"upstream-a"},
		Blocks:    []string{"downstream-c"},
	}
	tasks := []subTaskVerdict{
		{Title: "Only task", Description: "Single sub-task"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err)
	require.Len(t, subs, 1)

	subID := subs[0].ID

	foundDependsOn := false
	foundBlocks := false
	for _, call := range fake.calls {
		if len(call) == 4 && call[0] == "dep" && call[1] == "add" {
			if call[2] == subID && call[3] == "upstream-a" {
				foundDependsOn = true
			}
			if call[2] == "downstream-c" && call[3] == subID {
				foundBlocks = true
			}
		}
	}

	assert.True(t, foundDependsOn, "single sub-bead should inherit parent DependsOn")
	assert.True(t, foundBlocks, "single sub-bead should inherit parent Blocks")
}

// runnerWithDepScript returns a fakeRunner that synthesizes create responses
// and scripts dep responses (one per dep-add attempt). Other commands return
// "ok" and are not validated through the script.
func runnerWithDepScript(script []struct {
	out []byte
	err error
}) (*fakeRunner, func() int) {
	var idCounter int
	var idMu sync.Mutex
	var depCount int
	var depMu sync.Mutex
	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "create" {
				idMu.Lock()
				idCounter++
				id := fmt.Sprintf("test-%d", idCounter)
				idMu.Unlock()
				return []byte(fmt.Sprintf(`{"id":%q}`, id)), nil
			}
			if len(args) > 0 && args[0] == "dep" {
				depMu.Lock()
				depCount++
				idx := depCount - 1
				depMu.Unlock()
				if idx >= len(script) {
					idx = len(script) - 1
				}
				return script[idx].out, script[idx].err
			}
			return []byte("ok"), nil
		},
	}
	return fake, func() int {
		depMu.Lock()
		defer depMu.Unlock()
		return depCount
	}
}

func TestCreateSubBeads_TransientRetryThenSuccess(t *testing.T) {
	disableDepRetrySleep(t)

	fake, depCount := runnerWithDepScript([]struct {
		out []byte
		err error
	}{
		{nil, errors.New("[mysql] dial tcp 10.0.0.1:3306: i/o timeout")},
		{[]byte("ok"), nil},
	})

	parent := poller.Bead{ID: "parent-1", Title: "Feature", Priority: 2}
	tasks := []subTaskVerdict{
		{Title: "Task A", Description: "Desc A"},
		{Title: "Task B", Description: "Desc B"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err, "transient mysql i/o timeout should be retried and succeed on second attempt")
	require.Len(t, subs, 2)
	assert.Equal(t, 2, depCount(), "expected exactly one retry before success")
}

func TestCreateSubBeads_TransientExhaustionClarifies(t *testing.T) {
	disableDepRetrySleep(t)

	fake, depCount := runnerWithDepScript([]struct {
		out []byte
		err error
	}{
		{nil, errors.New("[mysql] invalid connection")},
		{nil, errors.New("[mysql] invalid connection")},
		{nil, errors.New("[mysql] invalid connection")},
	})

	parent := poller.Bead{ID: "parent-1", Title: "Feature", Priority: 2}
	tasks := []subTaskVerdict{
		{Title: "Task A", Description: "Desc A"},
		{Title: "Task B", Description: "Desc B"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.Error(t, err, "exhausting the transient retry budget must surface the error so the caller clarifies")
	assert.Contains(t, err.Error(), "sequential dependency chaining failed")
	assert.NotEmpty(t, subs, "partial sub-beads should still be returned")
	assert.Equal(t, depRetryAttempts, depCount(), "should have used the full retry budget")
}

func TestCreateSubBeads_PermanentDepErrorNoRetry(t *testing.T) {
	disableDepRetrySleep(t)

	fake, depCount := runnerWithDepScript([]struct {
		out []byte
		err error
	}{
		{[]byte("error: cycle detected between test-2 and test-1"), errors.New("bd dep add: cycle detected")},
	})

	parent := poller.Bead{ID: "parent-1", Title: "Feature", Priority: 2}
	tasks := []subTaskVerdict{
		{Title: "Task A", Description: "Desc A"},
		{Title: "Task B", Description: "Desc B"},
	}

	_, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.Error(t, err, "permanent errors (cycle detection) must still surface as failure")
	assert.Equal(t, 1, depCount(), "permanent errors must NOT trigger retries — operators need to see them immediately")
}

func TestCreateSubBeads_ExitOneWithAddedDependencyNoRetry(t *testing.T) {
	disableDepRetrySleep(t)

	exitErr := exitError1(t)
	fake, depCount := runnerWithDepScript([]struct {
		out []byte
		err error
	}{
		{[]byte("✓ Added dependency: test-2 depends on test-1 (blocks)"), exitErr},
	})

	parent := poller.Bead{ID: "parent-1", Title: "Feature", Priority: 2}
	tasks := []subTaskVerdict{
		{Title: "Task A", Description: "Desc A"},
		{Title: "Task B", Description: "Desc B"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err, "exit 1 with success marker in stdout should be treated as success on the first attempt")
	require.Len(t, subs, 2)
	assert.Equal(t, 1, depCount(), "the bd exit-1 quirk must not cause a retry")
}

func TestSchematicChildLabel(t *testing.T) {
	assert.Equal(t, "schematic:Forge-abc1", schematicChildLabel("Forge-abc1"))
	assert.Equal(t, SchematicChildLabelPrefix+"parent-1", schematicChildLabel("parent-1"))
}

func TestConfig_EmitEvent(t *testing.T) {
	// Nil-safe: emitting with no callback configured is a no-op.
	Config{}.emitEvent(EventKindParseFailed, "no callback")

	var gotKind, gotMsg string
	var calls int
	cfg := Config{OnEvent: func(kind, message string) {
		calls++
		gotKind, gotMsg = kind, message
	}}
	cfg.emitEvent(EventKindDecomposeFailed, "boom")
	assert.Equal(t, 1, calls)
	assert.Equal(t, EventKindDecomposeFailed, gotKind)
	assert.Equal(t, "boom", gotMsg)
}

func TestDepEdgeAlreadyExists(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"already exists", "error: dependency already exists", true},
		{"already depends", "test-2 already depends on test-1", true},
		{"duplicate", "duplicate dependency edge", true},
		{"mixed case", "Dependency Already Exists", true},
		{"unrelated", "connection refused", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, depEdgeAlreadyExists([]byte(tc.out)))
		})
	}
}

func TestFindExistingChildren_ParsesAndSkipsClosed(t *testing.T) {
	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "list" {
				return []byte(`[
					{"id":"c1","title":"Task A","status":"open"},
					{"id":"c2","title":"Task B","status":"in_progress"},
					{"id":"c3","title":"Task C","status":"closed"},
					{"id":"","title":"No ID","status":"open"}
				]`), nil
			}
			return []byte("ok"), nil
		},
	}
	got := findExistingChildren(context.Background(), "/tmp", "schematic:p", fake.run)
	assert.Equal(t, map[string]string{"Task A": "c1", "Task B": "c2"}, got,
		"closed children and entries without an ID must be excluded")
}

func TestFindExistingChildren_QueryErrorReturnsEmpty(t *testing.T) {
	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			return nil, errors.New("bd list: connection refused")
		},
	}
	got := findExistingChildren(context.Background(), "/tmp", "schematic:p", fake.run)
	assert.Empty(t, got, "a failed query must yield an empty map so decomposition proceeds")
}

func TestCreateSubBeads_AppliesMarkerLabel(t *testing.T) {
	fake := newFakeRunner()
	parent := poller.Bead{ID: "parent-1", Title: "Feature", Priority: 2}
	tasks := []subTaskVerdict{{Title: "Only task", Description: "Desc"}}

	_, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err)

	marker := schematicChildLabel(parent.ID)
	foundLabeledCreate := false
	for _, call := range fake.calls {
		if len(call) == 0 || call[0] != "create" {
			continue
		}
		hasLabelsFlag := false
		hasMarker := false
		for i, a := range call {
			if a == "--labels" && i+1 < len(call) && call[i+1] == marker {
				hasLabelsFlag = true
			}
			if a == marker {
				hasMarker = true
			}
		}
		if hasLabelsFlag && hasMarker {
			foundLabeledCreate = true
		}
	}
	assert.True(t, foundLabeledCreate, "every created sub-bead must carry --labels %q", marker)
}

func TestCreateSubBeads_ReusesExistingMarkedChildren(t *testing.T) {
	disableDepRetrySleep(t)
	parent := poller.Bead{ID: "parent-1", Title: "Big feature", Priority: 2}

	var createCount int
	var mu sync.Mutex
	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "list" {
				// "Task A" already exists from a prior partial pass.
				return []byte(`[{"id":"existing-A","title":"Task A","status":"open"}]`), nil
			}
			if len(args) > 0 && args[0] == "create" {
				mu.Lock()
				createCount++
				id := fmt.Sprintf("new-%d", createCount)
				mu.Unlock()
				return []byte(fmt.Sprintf(`{"id":%q}`, id)), nil
			}
			return []byte("ok"), nil
		},
	}
	tasks := []subTaskVerdict{
		{Title: "Task A", Description: "Desc A"},
		{Title: "Task B", Description: "Desc B"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err)
	require.Len(t, subs, 2)

	assert.Equal(t, "existing-A", subs[0].ID, "Task A should reuse the pre-existing marked child")
	assert.Equal(t, "new-1", subs[1].ID, "Task B should be freshly created")
	assert.Equal(t, 1, createCount, "only the missing task should be created (no duplicate for Task A)")
}

// TestCreateSubBeads_PartialFailureThenRedecomposeNoDuplicates exercises the
// core bug fix: a mid-loop failure leaves marker-labeled children behind, and a
// subsequent re-decomposition reuses them instead of creating a second set.
func TestCreateSubBeads_PartialFailureThenRedecomposeNoDuplicates(t *testing.T) {
	disableDepRetrySleep(t)
	parent := poller.Bead{ID: "parent-1", Title: "Feature", Priority: 2}

	type child struct{ id, title string }
	var created []child
	var idc int
	var mu sync.Mutex
	failDep := true

	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			switch args[0] {
			case "list":
				var b strings.Builder
				b.WriteString("[")
				for i, c := range created {
					if i > 0 {
						b.WriteString(",")
					}
					b.WriteString(fmt.Sprintf(`{"id":%q,"title":%q,"status":"open"}`, c.id, c.title))
				}
				b.WriteString("]")
				return []byte(b.String()), nil
			case "create":
				idc++
				id := fmt.Sprintf("bead-%d", idc)
				title := ""
				for _, a := range args {
					if strings.HasPrefix(a, "--title=") {
						title = strings.TrimPrefix(a, "--title=")
					}
				}
				created = append(created, child{id, title})
				return []byte(fmt.Sprintf(`{"id":%q}`, id)), nil
			case "dep":
				if failDep {
					// Permanent (non-transient) error → fails immediately.
					return nil, errors.New("bd dep add: connection refused")
				}
				return []byte("ok"), nil
			default:
				return []byte("ok"), nil
			}
		},
	}

	tasks := []subTaskVerdict{
		{Title: "Task A", Description: "A"},
		{Title: "Task B", Description: "B"},
	}

	// Pass 1: both children created, then the sequential dep add fails.
	subs1, err1 := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.Error(t, err1, "pass 1 must fail on the dep-add error")
	require.Len(t, created, 2, "both children should exist after the partial pass")

	// Pass 2: dep add now succeeds; re-decompose the same parent.
	failDep = false
	createdBefore := len(created)
	subs2, err2 := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err2, "pass 2 should succeed")
	require.Len(t, subs2, 2)

	assert.Equal(t, createdBefore, len(created),
		"re-decomposition must reuse the marked children, not create duplicates")
	assert.Equal(t, subs1[0].ID, subs2[0].ID, "Task A should keep the same ID across passes")
	assert.Equal(t, subs1[1].ID, subs2[1].ID, "Task B should keep the same ID across passes")
}

// TestCreateSubBeads_ClosesSupersededOrphansOnTitleChange covers the safety net
// for the case where a re-decomposition produces different task titles than the
// pass that partially ran: the old marker-labeled children are closed so bd is
// left with exactly one coherent set (no orphans).
func TestCreateSubBeads_ClosesSupersededOrphansOnTitleChange(t *testing.T) {
	disableDepRetrySleep(t)
	parent := poller.Bead{ID: "parent-1", Title: "Feature", Priority: 2}

	var closed []string
	var idc int
	var mu sync.Mutex
	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			switch args[0] {
			case "list":
				// A prior pass left two children with the OLD titles.
				return []byte(`[
					{"id":"old-A","title":"Old Task A","status":"open"},
					{"id":"old-B","title":"Old Task B","status":"open"}
				]`), nil
			case "create":
				idc++
				return []byte(fmt.Sprintf(`{"id":"new-%d"}`, idc)), nil
			case "close":
				closed = append(closed, args[1])
				return []byte("ok"), nil
			default:
				return []byte("ok"), nil
			}
		},
	}

	// The new decomposition uses DIFFERENT titles.
	tasks := []subTaskVerdict{
		{Title: "New Task X", Description: "X"},
		{Title: "New Task Y", Description: "Y"},
	}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err)
	require.Len(t, subs, 2)
	assert.Equal(t, "new-1", subs[0].ID)
	assert.Equal(t, "new-2", subs[1].ID)

	// The two orphaned children with the old titles must be closed.
	assert.Contains(t, closed, "old-A", "superseded orphan should be closed")
	assert.Contains(t, closed, "old-B", "superseded orphan should be closed")
}

func TestChainSequentialDep_AlreadyExistsTolerated(t *testing.T) {
	disableDepRetrySleep(t)
	parent := poller.Bead{ID: "parent-1"}
	subs := []SubBead{{ID: "c1", Title: "A"}, {ID: "c2", Title: "B"}}

	fake := &fakeRunner{
		response: func(args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "dep" {
				return []byte("error: dependency already exists"), errors.New("bd dep add failed")
			}
			return []byte("ok"), nil
		},
	}

	err := chainSequentialDep(context.Background(), "/tmp", parent, subs, fake.run)
	assert.NoError(t, err, "an already-existing dependency edge must be treated as success")
}

func TestIsTransientDepErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		out  []byte
		want bool
	}{
		{"nil error", nil, nil, false},
		{"mysql i/o timeout", errors.New("[mysql] 10.0.0.1:3306: i/o timeout"), nil, true},
		{"invalid connection", errors.New("[mysql] invalid connection"), nil, true},
		{"transient marker in stdout", errors.New("bd dep add failed"), []byte("[mysql] i/o timeout"), true},
		{"cycle detection", errors.New("cycle detected"), nil, false},
		{"missing parent", errors.New("parent not found"), nil, false},
		{"mixed case", errors.New("MySQL I/O Timeout"), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isTransientDepErr(tc.err, tc.out)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The parent re-fetch falls back to struct fields bd may never have populated,
// so a failure there silently drops the parent's downstream blocks. The warning
// is all that separates that from a parent with no blocks — so it has to carry
// bd's own output, and name the flag when an old bd is the reason.
func TestCreateSubBeads_ReFetchFailureNamesTheFlagAndBdsOutput(t *testing.T) {
	disableDepRetrySleep(t)

	fake := &fakeRunner{response: func(args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "create" {
			return []byte(`{"id":"test-1"}`), nil
		}
		if len(args) > 0 && args[0] == "show" {
			// run() hands back the combined buffer, which is where a cobra
			// rejection lands.
			return []byte("Error: unknown flag: " + executil.BdIncludeDependentsFlag),
				errors.New("exit status 1")
		}
		return []byte("ok"), nil
	}}

	var logged bytes.Buffer
	restore := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(restore) })

	parent := poller.Bead{ID: "parent-1", Title: "Big feature", Priority: 2}
	tasks := []subTaskVerdict{{Title: "Task A", Description: "Detailed description for Task A"}}

	_, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	require.NoError(t, err, "an unreadable re-fetch is a warning, not a failed decomposition")

	out := logged.String()
	assert.Contains(t, out, executil.BdIncludeDependentsFlag,
		"the warning must name the flag an old bd is missing")
	assert.Contains(t, out, "unknown flag", "the warning must carry bd's own output")
}
