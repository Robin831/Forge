package github

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPRBodyReadsTheBodyOfOnePR(t *testing.T) {
	var got []string
	p := &Provider{runner: func(_ context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		got = append([]string{dir, name}, args...)
		return []byte(`{"body":"## Changes\n\n- did X\n\nCloses-Bead: Fhi.Metadata-abc12"}`), nil, nil
	}}
	body, err := p.PRBody(context.Background(), "/wt", 948)
	require.NoError(t, err)
	assert.Equal(t, "## Changes\n\n- did X\n\nCloses-Bead: Fhi.Metadata-abc12", body)
	assert.Equal(t, []string{"/wt", "gh", "pr", "view", "948", "--json", "body"}, got)

	failing := &Provider{runner: func(context.Context, string, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("not found"), errors.New("exit 1")
	}}
	_, err = failing.PRBody(context.Background(), "/wt", 1)
	assert.ErrorContains(t, err, "not found")
}
