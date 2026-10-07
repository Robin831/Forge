package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/vcs/github"
)

// ghRepo fakes gh for one repo: queue says whether main has a merge queue.
type ghRepo struct {
	mu    sync.Mutex
	queue bool
	calls [][]string
}

func (g *ghRepo) run(_ context.Context, _ string, name string, args ...string) ([]byte, []byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, append([]string{name}, args...))
	if name == "git" {
		return []byte("https://github.com/FHIDev/Repo.git\n"), nil, nil
	}
	joined := strings.Join(args, " ")
	switch {
	case args[0] == "pr" && args[1] == "merge":
		return nil, nil, nil
	case args[0] == "pr" && args[1] == "view" && strings.Contains(joined, "baseRefName"):
		return []byte(`{"baseRefName":"main"}`), nil, nil
	case strings.Contains(joined, "mergeQueue(branch:"):
		if g.queue {
			return []byte(`{"data":{"repository":{"mergeQueue":{"id":"MQ"}}}}`), nil, nil
		}
		return []byte(`{"data":{"repository":{"mergeQueue":null}}}`), nil, nil
	case strings.Contains(joined, "isInMergeQueue"):
		return []byte(`{"data":{"repository":{"pullRequest":{"state":"OPEN","isInMergeQueue":true,"autoMergeRequest":null,"mergeQueueEntry":{"state":"QUEUED"}}}}}`), nil, nil
	}
	return nil, []byte("unexpected call"), errors.New("exit status 1")
}

func (g *ghRepo) mergeArgv() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out [][]string
	for _, c := range g.calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			out = append(out, c)
		}
	}
	return out
}

// Two anvils side by side — Heimdall without a queue, Munin with one — merged
// by the same daemon through the real GitHub provider.
func newQueueDaemon(t *testing.T) (*Daemon, *ghRepo, *ghRepo) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	plain, queued := &ghRepo{}, &ghRepo{queue: true}
	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vcsProviders: map[string]vcs.Provider{
			"heimdall": github.NewWithRunner(db, plain.run, nil),
			"munin":    github.NewWithRunner(db, queued.run, nil),
		},
		prRetryBackoff: &github.RetryBackoff{},
		bellowsMonitor: &rearmSpy{},
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"heimdall": {AutoMerge: true, Path: "/heimdall"},
			"munin":    {AutoMerge: true, Path: "/munin"},
		},
		Settings: config.SettingsConfig{MergeStrategy: "squash"},
	})
	return d, plain, queued
}

func insertPR(t *testing.T, db *state.DB, anvil string, n int) state.PR {
	t.Helper()
	pr := state.PR{Number: n, Anvil: anvil, BeadID: "Bead-" + anvil, Branch: "forge/Bead-" + anvil, BaseBranch: "main", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(&pr))
	return pr
}

func eventTypes(t *testing.T, db *state.DB, anvil string) []state.EventType {
	t.Helper()
	evs, err := db.EventsByBead("Bead-"+anvil, anvil, 50)
	require.NoError(t, err)
	var out []state.EventType
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func TestDoAutoMerge_QueueAndNonQueueSideBySide(t *testing.T) {
	d, plain, queued := newQueueDaemon(t)
	hPR := insertPR(t, d.db, "heimdall", 11)
	mPR := insertPR(t, d.db, "munin", 22)

	d.doAutoMerge(context.Background(), "heimdall", "/heimdall", hPR)
	d.doAutoMerge(context.Background(), "munin", "/munin", mPR)

	// Non-queue: main's argv, and merged = done, exactly as before.
	assert.Equal(t, [][]string{{"gh", "pr", "merge", "11", "--squash", "--delete-branch=false"}}, plain.mergeArgv())
	assert.Contains(t, eventTypes(t, d.db, "heimdall"), state.EventPRAutoMerged)
	assert.NotContains(t, eventTypes(t, d.db, "heimdall"), state.EventPRMergeQueued)
	h, err := d.db.GetPRByID(hPR.ID)
	require.NoError(t, err)
	assert.Nil(t, h.MergeQueuedAt)

	// Queue: enqueued with --auto and no strategy; queued, not merged.
	assert.Equal(t, [][]string{{"gh", "pr", "merge", "22", "--auto"}}, queued.mergeArgv())
	types := eventTypes(t, d.db, "munin")
	assert.Contains(t, types, state.EventPRMergeQueued)
	assert.NotContains(t, types, state.EventPRAutoMerged, "an enqueue must not be logged as merged")
	assert.NotContains(t, types, state.EventPRMerged)
	m, err := d.db.GetPRByID(mPR.ID)
	require.NoError(t, err)
	require.NotNil(t, m.MergeQueuedAt, "the queued state is persisted for a restart")
	assert.Equal(t, state.PROpen, m.Status, "not merged, so not closed out")
}

// Neither repo kind is ever merged with --admin, which would bypass the queue
// and the required checks — on the auto-merge path, the queue-aware call and
// the plain MergePR alike.
func TestMerge_NeverPassesAdmin(t *testing.T) {
	d, plain, queued := newQueueDaemon(t)
	for _, tc := range []struct {
		anvil string
		gh    *ghRepo
	}{{"heimdall", plain}, {"munin", queued}} {
		pr := insertPR(t, d.db, tc.anvil, 11)
		d.doAutoMerge(context.Background(), tc.anvil, "/"+tc.anvil, pr)
		_, err := vcs.Merge(context.Background(), d.vcsForAnvil(tc.anvil), vcs.MergeRequest{WorktreePath: "/" + tc.anvil, PRNumber: 11, Strategy: "squash"})
		require.NoError(t, err)
		_ = d.vcsForAnvil(tc.anvil).MergePR(context.Background(), "/"+tc.anvil, 11, "merge")

		argvs := tc.gh.mergeArgv()
		require.Len(t, argvs, 3, tc.anvil)
		for _, argv := range argvs {
			for _, a := range argv {
				assert.False(t, strings.HasPrefix(a, "--admin"), "%s: --admin passed: %v", tc.anvil, argv)
			}
			if tc.gh.queue {
				assert.True(t, reflect.DeepEqual(argv, []string{"gh", "pr", "merge", "11", "--auto"}), "queue argv: %v", argv)
			}
		}
	}
}
