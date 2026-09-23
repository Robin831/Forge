package vcs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestThreadsResolved(t *testing.T) {
	assert.True(t, (&PRStatus{}).ThreadsResolved(), "counted, none open")
	assert.False(t, (&PRStatus{UnresolvedThreads: 1}).ThreadsResolved(), "counted, one open")
	assert.False(t, (&PRStatus{UnresolvedThreadsUnknown: true}).ThreadsResolved(),
		"an uncounted zero must not read as thread-free")
}

func TestMergeabilityFromStatus_UncountedThreadsFailClosed(t *testing.T) {
	m := MergeabilityFromStatus(&PRStatus{UnresolvedThreadsUnknown: true})
	assert.True(t, m.HasUnresolvedThreads, "an uncounted thread state must block mergeability")
}
