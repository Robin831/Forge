package anvilhealth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRows_LeadingPreamble(t *testing.T) {
	const rows = `[{"conflict_table":"issues","conflict_count":2}]`
	tests := []struct {
		name    string
		out     string
		wantErr bool
		wantLen int
	}{
		{
			name:    "preamble is skipped",
			out:     "connecting to dolt...\n" + rows,
			wantLen: 1,
		},
		{
			// A preamble that itself contains '[' truncates at the wrong bracket.
			// That must surface as a parse error — Check then reports "unknown"
			// and leaves the previous flag untouched — never as bogus rows.
			name:    "preamble containing a bracket errors rather than misparsing",
			out:     "WARN [db] reconnecting\n" + rows,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRows([]byte(tt.out))
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Len(t, got, tt.wantLen)
		})
	}
}

func TestCheck_PreambleWithBracketIsUnknownNotHealthy(t *testing.T) {
	// End-to-end version of the case above: a misparsed result must not be read
	// as "no conflicts", which would clear a real wedge.
	f := &fakeRunner{replies: map[string]string{
		"dolt_conflicts": "WARN [db] reconnecting\n" + `[{"conflict_table":"issues","conflict_count":2}]`,
	}}
	_, err := f.checker().Check(context.Background(), "/anvil")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing dolt_conflicts result")
}

func TestCheck_ControlCharactersInTableNameAreStripped(t *testing.T) {
	// Table names come from a beads database that syncs over a git remote, and
	// coerceString fully JSON-unquotes them — so an escaped ESC would otherwise
	// reach the operator's terminal (forge status, Hearth, the WARN line) as a
	// live ANSI sequence, and an escaped newline could forge a log line.
	replies := divergenceReplies()
	replies["dolt_conflicts"] = `[{"conflict_table":"iss\u001b[31mues\nfake","conflict_count":1}]`
	f := &fakeRunner{replies: replies}

	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.NoError(t, err)
	require.True(t, rep.Wedged())
	for _, sink := range []string{rep.TableNames()[0], rep.TablesSummary(), rep.Detail()} {
		assert.NotContains(t, sink, "\x1b", "an escape byte must never reach an operator-facing sink")
		assert.NotContains(t, sink, "\n", "a newline must never reach an operator-facing sink")
	}
	assert.Contains(t, rep.TablesSummary(), "iss?[31mues?fake")
}

func TestQueryScalarString_SelectsByColumnName(t *testing.T) {
	// Positional selection ("the first value in the row map") is random under Go
	// map iteration; the value must be picked by name.
	f := &fakeRunner{replies: map[string]string{
		"dolt_remotes": `[{"other":"wrong","remote":"origin","also":"wrong"}]`,
	}}
	got := queryScalarString(context.Background(), f.run, "/anvil",
		"SELECT name AS remote FROM dolt_remotes", "remote", "name")
	assert.Equal(t, "origin", got)

	// An absent column yields "" rather than an arbitrary neighbour.
	assert.Empty(t, queryScalarString(context.Background(), f.run, "/anvil",
		"SELECT name AS remote FROM dolt_remotes", "missing"))
}
