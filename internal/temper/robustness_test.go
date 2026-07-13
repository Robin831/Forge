package temper

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Bounded output (head+tail truncation) -------------------------------

func TestHeadTailBuffer_VerbatimUnderCap(t *testing.T) {
	b := newHeadTailBuffer(1000)
	_, _ = b.Write([]byte("hello world"))
	assert.Equal(t, "hello world", b.String(), "output under cap must be returned verbatim")
	assert.NotContains(t, b.String(), "elided")
}

func TestHeadTailBuffer_ExactlyCapNotTruncated(t *testing.T) {
	b := newHeadTailBuffer(100) // head 50, tail 50
	data := bytes.Repeat([]byte("x"), 100)
	_, _ = b.Write(data)
	out := b.String()
	assert.Equal(t, 100, len(out), "exactly-cap output must be retained in full")
	assert.NotContains(t, out, "elided")
}

func TestHeadTailBuffer_TruncatesHeadAndTailWithMarker(t *testing.T) {
	b := newHeadTailBuffer(100) // head 50, tail 50
	head := bytes.Repeat([]byte("H"), 50)
	mid := bytes.Repeat([]byte("M"), 200)
	tail := bytes.Repeat([]byte("T"), 50)
	_, _ = b.Write(head)
	_, _ = b.Write(mid)
	_, _ = b.Write(tail)

	out := b.String()
	assert.True(t, strings.HasPrefix(out, strings.Repeat("H", 50)), "head must be preserved")
	assert.True(t, strings.HasSuffix(out, strings.Repeat("T", 50)), "tail must be preserved")
	assert.Contains(t, out, "[200 bytes elided]", "marker must name the number of elided bytes")
	// The dropped middle ('M') must not survive.
	assert.NotContains(t, out, "M")
	// Retained content is bounded to cap + the small marker.
	assert.LessOrEqual(t, len(out), 100+40)
}

func TestHeadTailBuffer_ChunkedWritesPreserveTail(t *testing.T) {
	b := newHeadTailBuffer(20) // head 10, tail 10
	for i := 0; i < 100; i++ {
		_, _ = b.Write([]byte("0123456789"))
	}
	out := b.String()
	assert.True(t, strings.HasPrefix(out, "0123456789"), "head from the first writes must survive")
	assert.True(t, strings.HasSuffix(out, "0123456789"), "tail must reflect the most recent bytes")
	assert.Contains(t, out, "elided")
}

func TestRun_BoundsStepOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh + seq; skip on Windows")
	}
	dir := t.TempDir()
	cfg := Config{
		OutputCap: 2000,
		Steps: []Step{{
			Name:    "noisy",
			Command: "sh",
			Args:    []string{"-c", "seq 1 100000; exit 1"},
			Timeout: 30 * time.Second,
		}},
	}
	res := Run(context.Background(), dir, cfg, nil, "Forge-test", "test")
	require.NotNil(t, res)
	require.False(t, res.Passed)
	require.Len(t, res.Steps, 1)
	out := res.Steps[0].Output
	assert.LessOrEqual(t, len(out), 2200, "output must be bounded near the configured cap")
	assert.Contains(t, out, "bytes elided", "truncated output must carry the elision marker")
	assert.True(t, strings.HasPrefix(out, "1\n"), "the head of the output must be preserved")
}

// --- Failure classification ----------------------------------------------

func TestClassification_IsRetryableWithoutSmith(t *testing.T) {
	assert.True(t, ClassificationTimeout.IsRetryableWithoutSmith())
	assert.True(t, ClassificationInfra.IsRetryableWithoutSmith())
	assert.False(t, ClassificationTestFailure.IsRetryableWithoutSmith())
	assert.False(t, ClassificationBuildError.IsRetryableWithoutSmith())
	assert.False(t, Classification("").IsRetryableWithoutSmith())
}

func TestClassifyFailure(t *testing.T) {
	ctx := context.Background()

	// A fired deadline classifies as timeout regardless of output.
	deadlineCtx, cancel := context.WithTimeout(ctx, time.Nanosecond)
	defer cancel()
	<-deadlineCtx.Done()
	assert.Equal(t, ClassificationTimeout, classifyFailure(deadlineCtx, nil, ""))

	// Infra markers (Go OOM / signal death) classify as infra.
	assert.Equal(t, ClassificationInfra, classifyFailure(ctx, nil, "fatal error: runtime: out of memory"))
	assert.Equal(t, ClassificationInfra, classifyFailure(ctx, nil, "signal: killed"))
	assert.Equal(t, ClassificationInfra, classifyFailure(ctx, nil, "SIGSEGV: segmentation violation\nsignal: segmentation fault"))
	assert.Equal(t, ClassificationInfra, classifyFailure(ctx, nil, "Test host process crashed : Out of memory"))

	// A genuine test failure wins even when a host-crash marker is also present.
	assert.Equal(t, ClassificationTestFailure, classifyFailure(ctx, nil,
		"Failed!  - Failed:     2, Passed: 3\nTest host process crashed : Out of memory"))

	// A plain non-zero exit with ordinary test output is a test failure.
	assert.Equal(t, ClassificationTestFailure, classifyFailure(ctx, nil, "--- FAIL: TestFoo (0.01s)"))
}

func TestRun_Timeout_ClassifiesAsTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep; skip on Windows")
	}
	dir := t.TempDir()
	cfg := Config{Steps: []Step{{
		Name:    "slow",
		Command: "sleep",
		Args:    []string{"5"},
		Timeout: 100 * time.Millisecond,
	}}}
	res := Run(context.Background(), dir, cfg, nil, "Forge-test", "test")
	require.NotNil(t, res)
	assert.False(t, res.Passed)
	assert.Equal(t, "slow", res.FailedStep)
	assert.Equal(t, ClassificationTimeout, res.Classification)
	require.Len(t, res.Steps, 1)
	assert.Equal(t, ClassificationTimeout, res.Steps[0].Classification)
}

func TestRun_StepTimeoutFromConfigDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep; skip on Windows")
	}
	dir := t.TempDir()
	// No per-step Timeout: the Config.StepTimeout must be applied and fire.
	cfg := Config{
		StepTimeout: 100 * time.Millisecond,
		Steps:       []Step{{Name: "slow", Command: "sleep", Args: []string{"5"}}},
	}
	res := Run(context.Background(), dir, cfg, nil, "Forge-test", "test")
	require.NotNil(t, res)
	assert.False(t, res.Passed)
	assert.Equal(t, ClassificationTimeout, res.Classification)
}

func TestRun_SignalDeath_ClassifiesAsInfra(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal model does not apply; skip on Windows")
	}
	dir := t.TempDir()
	cfg := Config{Steps: []Step{{
		Name:    "crash",
		Command: "sh",
		Args:    []string{"-c", "kill -9 $$"},
		Timeout: 10 * time.Second,
	}}}
	res := Run(context.Background(), dir, cfg, nil, "Forge-test", "test")
	require.NotNil(t, res)
	assert.False(t, res.Passed)
	assert.Equal(t, ClassificationInfra, res.Classification)
}

func TestRun_RealTestFailure_ClassifiesAsTestFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh; skip on Windows")
	}
	dir := t.TempDir()
	cfg := Config{Steps: []Step{{
		Name:    "test",
		Command: "sh",
		Args:    []string{"-c", "echo '--- FAIL: TestFoo'; exit 1"},
		Timeout: 10 * time.Second,
	}}}
	res := Run(context.Background(), dir, cfg, nil, "Forge-test", "test")
	require.NotNil(t, res)
	assert.False(t, res.Passed)
	assert.Equal(t, ClassificationTestFailure, res.Classification)
}
