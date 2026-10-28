package schematic

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/poller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func createArgsOf(fake *fakeRunner) []string {
	for _, c := range fake.calls {
		if len(c) > 0 && c[0] == "create" {
			return c
		}
	}
	return nil
}

func TestCreateSubBeads_InheritTheDecomposedBeadsEpic(t *testing.T) {
	logs := captureLog(t)
	fake := newFakeRunner()
	parent := poller.Bead{ID: "Munin-big", Title: "Big", Priority: 2, Parent: "Fhi.Metadata-eeuey"}

	_, err := createSubBeads(context.Background(), parent, []subTaskVerdict{{Title: "A", Description: "a"}}, "/tmp", fake.run)
	require.NoError(t, err)
	args := strings.Join(createArgsOf(fake), " ")
	assert.Contains(t, args, "--parent Fhi.Metadata-eeuey --no-inherit-labels")
	assert.Contains(t, args, "--deps blocks:Munin-big")
	assert.NotContains(t, logs.String(), "no parent epic")
}

func TestCreateSubBeads_NoEpicWarns(t *testing.T) {
	for name, parent := range map[string]poller.Bead{
		"no parent":     {ID: "Munin-big", Title: "Big", Priority: 2},
		"crucible epic": {ID: "Munin-big", Title: "Big", Priority: 2, Parent: "Munin-epic", EpicBranch: "feature/Munin-epic"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLog(t)
			fake := newFakeRunner()
			_, err := createSubBeads(context.Background(), parent, []subTaskVerdict{{Title: "A", Description: "a"}}, "/tmp", fake.run)
			require.NoError(t, err)
			assert.NotContains(t, strings.Join(createArgsOf(fake), " "), "--parent")
			assert.Contains(t, logs.String(), "no parent epic")
		})
	}
}
