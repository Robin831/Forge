package schematic

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/poller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issueSyncRunner simulates bd for the decomposition issue-sync path: `bd
// show` reports each bead's external_ref from refs, and `bd github push`
// invokes onPush (which typically populates refs, the way a real push links
// issues).
type issueSyncRunner struct {
	mu     sync.Mutex
	calls  [][]string
	refs   map[string]string
	onPush func(ids []string) ([]byte, error)
}

func (r *issueSyncRunner) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.mu.Unlock()

	switch {
	case len(args) >= 2 && args[0] == "show":
		id := args[1]
		return []byte(fmt.Sprintf(`[{"id":%q,"external_ref":%q}]`, id, r.refs[id])), nil
	case len(args) >= 2 && args[0] == "github" && args[1] == "push":
		return r.onPush(args[2:])
	}
	return []byte("ok"), nil
}

func (r *issueSyncRunner) pushCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var pushes [][]string
	for _, c := range r.calls {
		if len(c) >= 2 && c[0] == "github" && c[1] == "push" {
			pushes = append(pushes, c[2:])
		}
	}
	return pushes
}

func syncEventRecorder() (*[]string, Config) {
	var events []string
	cfg := Config{OnEvent: func(kind, message string) {
		events = append(events, kind+": "+message)
	}}
	return &events, cfg
}

// Regression (Forge-jhf1): a decomposition creates N beads in one pass, and
// bd's per-command auto-sync cannot cover them — every child must be pushed
// to GitHub explicitly, in one bd github push naming all of the issueless
// ones. This test drives the multi-bead path the defect only appears on.
func TestSyncSubBeadIssues_DecompositionPushesAllIssuelessChildren(t *testing.T) {
	runner := &issueSyncRunner{refs: map[string]string{
		"child-1": "", "child-2": "", "child-3": "",
	}}
	runner.onPush = func(ids []string) ([]byte, error) {
		// The real push creates the GitHub issues and stores external_refs.
		for _, id := range ids {
			runner.refs[id] = "https://github.com/org/repo/issues/9" + id[len(id)-1:]
		}
		return []byte("pushed"), nil
	}
	events, cfg := syncEventRecorder()

	subs := []SubBead{{ID: "child-1"}, {ID: "child-2"}, {ID: "child-3"}}
	syncSubBeadIssues(context.Background(), cfg, poller.Bead{ID: "parent-1"}, subs, "/tmp", runner.run)

	pushes := runner.pushCalls()
	require.Len(t, pushes, 1, "expected exactly one bd github push for the decomposition")
	for _, id := range []string{"child-1", "child-2", "child-3"} {
		assert.True(t, slices.Contains(pushes[0], id), "push must name %s", id)
	}
	assert.Empty(t, *events, "a successful sync emits no failure event")
}

func TestSyncSubBeadIssues_AlreadySyncedChildrenAreNotPushed(t *testing.T) {
	runner := &issueSyncRunner{refs: map[string]string{
		"child-1": "https://github.com/org/repo/issues/1",
		"child-2": "https://github.com/org/repo/issues/2",
	}}
	runner.onPush = func(ids []string) ([]byte, error) { return []byte("pushed"), nil }
	events, cfg := syncEventRecorder()

	subs := []SubBead{{ID: "child-1"}, {ID: "child-2"}}
	syncSubBeadIssues(context.Background(), cfg, poller.Bead{ID: "parent-1"}, subs, "/tmp", runner.run)

	assert.Empty(t, runner.pushCalls(), "children with refs need no push")
	assert.Empty(t, *events)
}

func TestSyncSubBeadIssues_PartialMissingPushesOnlyMissing(t *testing.T) {
	runner := &issueSyncRunner{refs: map[string]string{
		"child-1": "https://github.com/org/repo/issues/1",
		"child-2": "",
	}}
	runner.onPush = func(ids []string) ([]byte, error) {
		for _, id := range ids {
			runner.refs[id] = "https://github.com/org/repo/issues/2"
		}
		return []byte("pushed"), nil
	}
	events, cfg := syncEventRecorder()

	subs := []SubBead{{ID: "child-1"}, {ID: "child-2"}}
	syncSubBeadIssues(context.Background(), cfg, poller.Bead{ID: "parent-1"}, subs, "/tmp", runner.run)

	pushes := runner.pushCalls()
	require.Len(t, pushes, 1)
	assert.Equal(t, []string{"child-2"}, pushes[0])
	assert.Empty(t, *events)
}

func TestSyncSubBeadIssues_PushFailureEmitsEvent(t *testing.T) {
	runner := &issueSyncRunner{refs: map[string]string{"child-1": ""}}
	runner.onPush = func(ids []string) ([]byte, error) {
		return []byte("api error"), errors.New("exit status 1")
	}
	events, cfg := syncEventRecorder()

	subs := []SubBead{{ID: "child-1"}}
	syncSubBeadIssues(context.Background(), cfg, poller.Bead{ID: "parent-1"}, subs, "/tmp", runner.run)

	require.Len(t, *events, 1)
	assert.Contains(t, (*events)[0], EventKindIssueSyncFailed)
	assert.Contains(t, (*events)[0], "child-1")
}

func TestSyncSubBeadIssues_UnconfiguredGitHubSyncIsSilent(t *testing.T) {
	runner := &issueSyncRunner{refs: map[string]string{"child-1": ""}}
	runner.onPush = func(ids []string) ([]byte, error) {
		return []byte("Error: github.token is not configured. Set via 'bd config set github.token <token>' or GITHUB_TOKEN environment variable"), nil
	}
	events, cfg := syncEventRecorder()

	subs := []SubBead{{ID: "child-1"}}
	syncSubBeadIssues(context.Background(), cfg, poller.Bead{ID: "parent-1"}, subs, "/tmp", runner.run)

	assert.Empty(t, *events, "an anvil without GitHub sync is not a failure")
}

func TestSyncSubBeadIssues_RefStillMissingAfterPushEmitsEvent(t *testing.T) {
	runner := &issueSyncRunner{refs: map[string]string{"child-1": ""}}
	runner.onPush = func(ids []string) ([]byte, error) {
		// Push "succeeds" but links nothing — the silence this sync breaks.
		return []byte("pushed 0 issues"), nil
	}
	events, cfg := syncEventRecorder()

	subs := []SubBead{{ID: "child-1"}}
	syncSubBeadIssues(context.Background(), cfg, poller.Bead{ID: "parent-1"}, subs, "/tmp", runner.run)

	require.Len(t, *events, 1)
	assert.Contains(t, (*events)[0], EventKindIssueSyncFailed)
	assert.Contains(t, (*events)[0], "still have no external_ref")
}
