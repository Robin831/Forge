package wicket

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func createWithFakeBd(t *testing.T, parent string) []string {
	t.Helper()
	original := bdRunner
	t.Cleanup(func() { bdRunner = original })
	var got []string
	bdRunner = func(_ context.Context, args []string, _ string) (string, error) {
		got = args
		return "Munin-x1\n", nil
	}
	decision := TriageDecision{Action: ActionCreateBead, BeadTitle: "Fix it", BeadDescription: "Details."}
	_, err := CreateBead(context.Background(), nil, decision, Issue{Repo: "org/repo", Number: 3}, 2, "munin", "", parent)
	require.NoError(t, err)
	return got
}

func TestCreateBead_PassesParent(t *testing.T) {
	logs := captureSlog(t)
	args := strings.Join(createWithFakeBd(t, "Fhi.Metadata-eeuey"), " ")
	assert.Contains(t, args, "--parent Fhi.Metadata-eeuey --no-inherit-labels")
	assert.Contains(t, args, "--type task")
	assert.Empty(t, logs.String())
}

func TestCreateBead_NoParentWarns(t *testing.T) {
	logs := captureSlog(t)
	args := strings.Join(createWithFakeBd(t, ""), " ")
	assert.NotContains(t, args, "--parent")
	assert.Contains(t, logs.String(), "no parent epic")
	assert.Contains(t, logs.String(), "kind=wicket")
}
