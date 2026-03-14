package schematic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/poller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	output := "```\n" + `{"action":"decompose","sub_tasks":["Task A","Task B"],"reason":"Too large"}` + "\n```"

	v, err := parseVerdict(output)
	require.NoError(t, err)
	assert.Equal(t, "decompose", v.Action)
	assert.Equal(t, []string{"Task A", "Task B"}, v.SubTasks)
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
	assert.Equal(t, 10, cfg.MaxTurns)
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

// newFakeRunner builds a runner whose "bd create" calls return sequential IDs
// and all other calls succeed.
func newFakeRunner() *fakeRunner {
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
			// dep add, update, etc. succeed silently.
			return []byte("ok"), nil
		},
	}
}

func TestCreateSubBeads_SequentialDepsAdded(t *testing.T) {
	fake := newFakeRunner()
	parent := poller.Bead{ID: "parent-1", Title: "Big feature", Priority: 2}
	tasks := []string{"Task A", "Task B", "Task C"}

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
	tasks := []string{"Task A", "Task B"}

	subs, err := createSubBeads(context.Background(), parent, tasks, "/tmp", fake.run)
	// Must return an error so the caller can escalate to ActionClarify.
	require.Error(t, err, "dep add failure must be fatal")
	assert.Contains(t, err.Error(), "adding sequential dependency")
	// Partial sub-beads should be returned for operator visibility.
	assert.NotEmpty(t, subs, "partial sub-beads should be returned even on dep add failure")
}

func TestCreateSubBeads_SingleTaskNoDep(t *testing.T) {
	fake := newFakeRunner()
	parent := poller.Bead{ID: "parent-1", Title: "Simple task", Priority: 1}

	subs, err := createSubBeads(context.Background(), parent, []string{"Only task"}, "/tmp", fake.run)
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
