package prompt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func followupCtx(t *testing.T, parent string) BeadContext {
	return BeadContext{
		BeadID: "Munin-1", Title: "T", Description: "D", IssueType: "task",
		Branch: "forge/Munin-1", AnvilName: "munin", AnvilPath: t.TempDir(), WorktreePath: t.TempDir(),
		FollowupParent: parent,
	}
}

func TestBuild_FollowupBeadsGoUnderTheConfiguredEpic(t *testing.T) {
	out, err := NewBuilder().Build(followupCtx(t, "Fhi.Metadata-eeuey"))
	require.NoError(t, err)
	assert.Contains(t, out, "--deps discovered-from:Munin-1 --parent=Fhi.Metadata-eeuey --no-inherit-labels")
	assert.Contains(t, out, "Always file it under the epic")
}

func TestBuild_NoFollowupParentKeepsOldCommand(t *testing.T) {
	out, err := NewBuilder().Build(followupCtx(t, ""))
	require.NoError(t, err)
	assert.Contains(t, out, "--deps discovered-from:Munin-1\n")
	assert.NotContains(t, out, "--parent")
}
