package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
)

// The per-child opt-out has to survive the poll it is discovered in: the label
// is durable, ForceIndependent is not (json:"-"), so pollAnvil re-derives the
// flag on every pass. Driven through a real `bd ready` subprocess rather than a
// re-implementation of the loop, since the derivation being in the *eligible*
// path — after the assignee and clarification filters — is part of what is
// being asserted.
func TestPollAnvil_DerivesForceIndependentFromTheLabel(t *testing.T) {
	withFakeBd(t, `cat <<'JSON'
[
  {"id":"child-1","status":"open","parent":"parent-1","labels":["independent"]},
  {"id":"child-2","status":"open","parent":"parent-1","labels":["forgeReady"]}
]
JSON`)

	p := New(map[string]config.AnvilConfig{"repo": {Path: t.TempDir()}})
	beads, err := p.PollSingle(context.Background(), "repo")
	require.NoError(t, err)
	require.Len(t, beads, 2)

	byID := map[string]Bead{}
	for _, b := range beads {
		byID[b.ID] = b
	}
	assert.True(t, byID["child-1"].ForceIndependent, "the labeled child dispatches standalone")
	assert.False(t, byID["child-2"].ForceIndependent, "its sibling is orchestrated as usual")
}

// The label is matched the way every other epic label is: trimmed and
// case-insensitively, so a padded or capitalised value cannot opt out through
// one code path and not another.
func TestPollAnvil_ForceIndependentLabelIsNormalised(t *testing.T) {
	withFakeBd(t, `cat <<'JSON'
[{"id":"child-1","status":"open","labels":[" Independent "]}]
JSON`)

	p := New(map[string]config.AnvilConfig{"repo": {Path: t.TempDir()}})
	beads, err := p.PollSingle(context.Background(), "repo")
	require.NoError(t, err)
	require.Len(t, beads, 1)

	assert.True(t, beads[0].ForceIndependent)
}

// Blocks is the "children to orchestrate" signal every epic gate reads, so an
// opted-out child must not appear in it — otherwise a parent whose only ready
// child is independent starts a Crucible with nothing to put on its branch.
func TestPollAnvil_IndependentChildIsNotInTheParentsBlocks(t *testing.T) {
	withFakeBd(t, `cat <<'JSON'
[
  {"id":"parent-1","status":"open","labels":["crucible"]},
  {"id":"child-1","status":"open","parent":"parent-1","labels":["independent"]},
  {"id":"child-2","status":"open","dependencies":[{"depends_on_id":"parent-1","type":"blocks"}]}
]
JSON`)

	p := New(map[string]config.AnvilConfig{"repo": {Path: t.TempDir()}})
	beads, err := p.PollSingle(context.Background(), "repo")
	require.NoError(t, err)

	var parent Bead
	for _, b := range beads {
		if b.ID == "parent-1" {
			parent = b
		}
	}
	sort.Strings(parent.Blocks)
	assert.Equal(t, []string{"child-2"}, parent.Blocks,
		"an independent child is not counted among the children the epic orchestrates")
}

// The regression the json:"-" tag invites: a bead that has been through the
// queue cache (or any other JSON round-trip) arrives with the flag cleared, so
// nothing may depend on the flag alone. IsIndependentBead reads the label too,
// which is what every epic gate calls.
func TestIsIndependentBead_SurvivesAJSONRoundTrip(t *testing.T) {
	original := Bead{ID: "child-1", Labels: []string{"independent"}, ForceIndependent: true}

	encoded, err := json.Marshal(original)
	require.NoError(t, err)
	var restored Bead
	require.NoError(t, json.Unmarshal(encoded, &restored))

	assert.False(t, restored.ForceIndependent, "the flag is json:\"-\" — it does not survive")
	assert.True(t, IsIndependentBead(restored), "but the label does, and that is what is read")
}

// The two forms reach a bead by different routes and neither implies the other:
// a force-run bead carries no label, a labeled bead restored from cache carries
// no flag.
func TestIsIndependentBead(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want bool
	}{
		{"neither", Bead{ID: "b"}, false},
		{"label only", Bead{ID: "b", Labels: []string{"independent"}}, true},
		{"flag only (manual run independently)", Bead{ID: "b", ForceIndependent: true}, true},
		{"both", Bead{ID: "b", Labels: []string{"INDEPENDENT"}, ForceIndependent: true}, true},
		{"unrelated labels", Bead{ID: "b", Labels: []string{"forgeReady", "crucible"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsIndependentBead(tt.bead))
		})
	}
}

// The stamp is what routes a bead onto the epic branch, so the whole of the
// opt-out on this side is refusing to stamp it: worktree from main, PR to main.
func TestResolveEpicBranches_IndependentChildIsNotStamped(t *testing.T) {
	restore := SetEpicBranchLookupForTest(func(_ context.Context, parentID, _ string) string {
		if parentID == "parent-1" {
			return "feature/parent-1"
		}
		return ""
	})
	defer restore()

	beads := []Bead{
		{ID: "child-1", Anvil: "repo", Parent: "parent-1", Labels: []string{"independent"}},
		{ID: "child-2", Anvil: "repo", Parent: "parent-1"},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Empty(t, beads[0].EpicBranch, "the opted-out child PRs to main")
	assert.Empty(t, beads[0].EpicParent)
	assert.Equal(t, "feature/parent-1", beads[1].EpicBranch, "its sibling is routed as usual")
}

// An open independent child says nothing about whether the epic still has work
// for its feature branch, so it must not hold the parent: OpenChildren is what
// decides between dispatching an opted-in parent and escalating it.
//
// Driven through the real subprocess rather than the parser, because the labels
// are not in the payload the parser reads: `bd show --json` reports a dependent
// as an edge summary (id/title/status/priority/issue_type/dependency_type) and
// puts labels only on a bead's own record. The second lookup that fetches them
// is the thing under test.
func TestOpenChildren_IndependentChildDoesNotCount(t *testing.T) {
	withFakeBdShow(t, map[string]string{
		"parent-1": `{"id":"parent-1","dependents":[
			{"id":"child-1","dependency_type":"blocks","status":"open"},
			{"id":"child-2","dependency_type":"blocks","status":"open"}]}`,
		"child-1": `{"id":"child-1","status":"open","labels":["independent"]}`,
		"child-2": `{"id":"child-2","status":"open","labels":["forgeReady"]}`,
	})

	open, err := OpenChildren(context.Background(), "parent-1", t.TempDir())

	require.NoError(t, err)
	assert.Equal(t, []string{"child-2"}, open)
}

// The whole family opted out: the epic has nothing left to orchestrate, which
// is the answer that lets the parent run the ordinary pipeline to main.
func TestOpenChildren_AllIndependentIsEmpty(t *testing.T) {
	withFakeBdShow(t, map[string]string{
		"parent-1": `{"id":"parent-1","dependents":[
			{"id":"child-1","dependency_type":"blocks","status":"open"},
			{"id":"child-2","dependency_type":"parent-child","status":"open"}]}`,
		"child-1": `{"id":"child-1","status":"open","labels":["independent"]}`,
		"child-2": `{"id":"child-2","status":"open","labels":[" Independent "]}`,
	})

	open, err := OpenChildren(context.Background(), "parent-1", t.TempDir())

	require.NoError(t, err)
	assert.Empty(t, open)
}

// A child bd reports without any labels is read as an ordinary child. That is
// the conservative direction: it holds the parent for an operator instead of
// closing an epic whose children are still open.
func TestOpenChildren_MissingLabelsAreAnOrdinaryChild(t *testing.T) {
	withFakeBdShow(t, map[string]string{
		"parent-1": `{"id":"parent-1","dependents":[{"id":"child-1","dependency_type":"blocks","status":"open"}]}`,
		"child-1":  `{"id":"child-1","status":"open"}`,
	})

	open, err := OpenChildren(context.Background(), "parent-1", t.TempDir())

	require.NoError(t, err)
	assert.Equal(t, []string{"child-1"}, open)
}

// The label lookup is a second subprocess and can fail on its own. Failing it
// must not invent an opt-out that was never there: the children are kept, so
// the parent is held for an operator rather than dispatched to main past an
// epic that still has work.
func TestOpenChildren_UnreadableLabelsAreOrdinaryChildren(t *testing.T) {
	withFakeBd(t, `case "$*" in
  "show parent-1 --json") echo '[{"id":"parent-1","dependents":[`+
		`{"id":"child-1","dependency_type":"blocks","status":"open"},`+
		`{"id":"child-2","dependency_type":"blocks","status":"open"}]}]' ;;
  *) echo "bd exploded" >&2; exit 3 ;;
esac`)

	open, err := OpenChildren(context.Background(), "parent-1", t.TempDir())

	require.NoError(t, err, "an unreadable label is not an unreadable epic")
	assert.Equal(t, []string{"child-1", "child-2"}, open)
}

// The parser is the other half of the same answer, and the labels are no longer
// its business: whatever a dependents entry claims about them, parseOpenChildren
// reports every open child and leaves the opt-out to dropIndependent. Pinned so
// a future bd that does emit labels there cannot quietly grow a second,
// divergent filter.
func TestParseOpenChildren_DoesNotFilterOnLabels(t *testing.T) {
	output := `{"dependents":[
		{"id":"child-1","dependency_type":"blocks","status":"open","labels":["independent"]},
		{"id":"child-2","dependency_type":"blocks","status":"open"}]}`

	open, err := parseOpenChildren([]byte(output))

	require.NoError(t, err)
	assert.Equal(t, []string{"child-1", "child-2"}, open)
}

// lookupBlocks feeds the parent's Blocks field, which every orchestration gate
// reads as "children to orchestrate" — IsCrucibleCandidate, crucibleOwnedChildren
// and the Crucible's own child count. An opted-out child in there re-registers
// it as orchestrable through a path nothing else guards, so the exclusion is
// pinned at this site too and not only at parseOpenChildren's.
func TestLookupBlocks_ExcludesIndependentChildren(t *testing.T) {
	withFakeBdShow(t, map[string]string{
		"parent-1": `{"id":"parent-1","dependents":[
			{"id":"child-1","dependency_type":"blocks","status":"open"},
			{"id":"child-2","dependency_type":"blocks","status":"open"}]}`,
		"child-1": `{"id":"child-1","status":"open","labels":["Independent"]}`,
		"child-2": `{"id":"child-2","status":"open","labels":["forgeReady"]}`,
	})

	blocks := lookupBlocks(context.Background(), "parent-1", t.TempDir())

	assert.Equal(t, []string{"child-2"}, blocks)
}

// The same conservative direction parseOpenChildren's side documents, asserted
// here because the two functions filter at structurally different points: a
// child whose labels bd does not report is an ordinary child, so a parent keeps
// looking like a Crucible candidate rather than silently losing a child that
// never opted out.
func TestLookupBlocks_MissingLabelsAreAnOrdinaryChild(t *testing.T) {
	withFakeBdShow(t, map[string]string{
		"parent-1": `{"id":"parent-1","dependents":[{"id":"child-1","dependency_type":"blocks","status":"open"}]}`,
		"child-1":  `{"id":"child-1","status":"open"}`,
	})

	blocks := lookupBlocks(context.Background(), "parent-1", t.TempDir())

	assert.Equal(t, []string{"child-1"}, blocks)
}

// And the same when the label lookup itself fails.
func TestLookupBlocks_UnreadableLabelsAreOrdinaryChildren(t *testing.T) {
	withFakeBd(t, `case "$*" in
  "show parent-1 --json") echo '[{"id":"parent-1","dependents":[{"id":"child-1","dependency_type":"blocks","status":"open"}]}]' ;;
  *) echo "bd exploded" >&2; exit 3 ;;
esac`)

	blocks := lookupBlocks(context.Background(), "parent-1", t.TempDir())

	assert.Equal(t, []string{"child-1"}, blocks)
}

// Both readers of a dependents payload must read bd's terminal status the same
// way. lookupBlocks used to compare `!= "closed"` verbatim while
// parseOpenChildren trimmed and folded, so a dependent reported as "Closed" was
// a child to one gate and not to the other — the exact per-path divergence the
// label handling works to avoid.
func TestLookupBlocks_ClosedStatusIsNormalisedLikeParseOpenChildren(t *testing.T) {
	dependents := `{"id":"parent-1","dependents":[
		{"id":"child-1","dependency_type":"blocks","status":"closed"},
		{"id":"child-2","dependency_type":"blocks","status":"CLOSED"},
		{"id":"child-3","dependency_type":"blocks","status":" closed "},
		{"id":"child-4","dependency_type":"blocks","status":"open"}]}`
	withFakeBdShow(t, map[string]string{
		"parent-1": dependents,
		"child-4":  `{"id":"child-4","status":"open"}`,
	})

	blocks := lookupBlocks(context.Background(), "parent-1", t.TempDir())
	assert.Equal(t, []string{"child-4"}, blocks)

	open, err := parseOpenChildren([]byte(dependents))
	require.NoError(t, err)
	assert.Equal(t, blocks, open, "the two readers must agree on what closed means")
}

// The other half of the shared payload vocabulary. lookupBlocks used to count
// only "blocks" edges while parseOpenChildren counted "blocks" and
// "parent-child", so a family linked purely by parent-child edges was held open
// by OpenChildren — the parent escalates instead of dispatching — while its
// Blocks stayed empty, IsCrucibleCandidate never fired, and the epic had no way
// to orchestrate its way out of the hold. The wider pair is what pollAnvil
// already reconstructs Blocks from when parent and child share a poll batch, so
// the fallback lookup now agrees with both the batch path and the other reader.
// "depends_on" stays excluded at every site: it is sequencing, not parenthood.
func TestLookupBlocks_CountsParentChildEdgesLikeParseOpenChildren(t *testing.T) {
	dependents := `{"id":"parent-1","dependents":[
		{"id":"child-1","dependency_type":"blocks","status":"open"},
		{"id":"child-2","dependency_type":"parent-child","status":"open"},
		{"id":"other-1","dependency_type":"depends_on","status":"open"}]}`
	withFakeBdShow(t, map[string]string{
		"parent-1": dependents,
		"child-1":  `{"id":"child-1","status":"open"}`,
		"child-2":  `{"id":"child-2","status":"open"}`,
	})

	blocks := lookupBlocks(context.Background(), "parent-1", t.TempDir())
	assert.Equal(t, []string{"child-1", "child-2"}, blocks,
		"a parent-child edge is a child here too, and a sequencing edge is not")

	open, err := parseOpenChildren([]byte(dependents))
	require.NoError(t, err)
	assert.Equal(t, blocks, open, "the two readers must agree on what a child is")
}

// The label lookup answers a multi-id `bd show`, which returns an array; a
// single-id show returns an array of one, and a bare object is accepted for the
// same case. A bead absent from the answer is not in the map, which is what
// makes an incomplete answer read as "ordinary child" rather than "opted out".
func TestParseIndependent(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   map[string]bool
	}{
		{
			name:   "array of records",
			output: `[{"id":"a","labels":["independent"]},{"id":"b","labels":["forgeReady"]}]`,
			want:   map[string]bool{"a": true},
		},
		{
			name:   "bare object",
			output: `{"id":"a","labels":["independent"]}`,
			want:   map[string]bool{"a": true},
		},
		{
			name:   "labels are trimmed and case-folded",
			output: `[{"id":"a","labels":[" INDEPENDENT "]}]`,
			want:   map[string]bool{"a": true},
		},
		{
			name:   "no labels field at all",
			output: `[{"id":"a"},{"id":"b","labels":[]}]`,
			want:   map[string]bool{},
		},
		{
			name:   "near miss is not the label",
			output: `[{"id":"a","labels":["independent-ish"]}]`,
			want:   map[string]bool{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIndependent([]byte(tt.output))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// withFakeBdShow puts a `bd` on PATH that answers `bd show <id...> --json` with
// the record registered for each requested id, in the JSON array bd actually
// emits. Ids with no record are omitted from the answer, which is how "bd does
// not report this bead" is expressed.
//
// A record is written to the script through json.Compact and single-quote
// escaping, never through a blanket whitespace flatten: the fixtures are
// multi-line for readability, but the shell form below needs one line, and
// collapsing every whitespace run cannot tell an indentation newline from a
// space inside a string value. It used to do exactly that, which silently
// rewrote a `" Independent "` label fixture into `"Independent"` — turning an
// end-to-end assertion about trimming into one about case-folding alone, with
// the test still green. json.Compact only removes whitespace *between* JSON
// tokens, so what the fixture says is what the code under test reads (and an
// invalid fixture now fails loudly rather than being reshaped).
func withFakeBdShow(t *testing.T, records map[string]string) {
	t.Helper()

	var cases strings.Builder
	for id, record := range records {
		var compact bytes.Buffer
		require.NoError(t, json.Compact(&compact, []byte(record)), "fixture for %s is not valid JSON", id)
		fmt.Fprintf(&cases, "    %s) rec='%s' ;;\n", id, shellSingleQuoted(compact.String()))
	}

	withFakeBd(t, `ids=""
for a in "$@"; do
  case "$a" in show|--json|-*) ;; *) ids="$ids $a" ;; esac
done
out=""
for id in $ids; do
  rec=""
  case "$id" in
`+cases.String()+`  esac
  [ -z "$rec" ] && continue
  if [ -z "$out" ]; then out="$rec"; else out="$out,$rec"; fi
done
printf '[%s]\n' "$out"`)
}

// shellSingleQuoted escapes a value for embedding inside a single-quoted /bin/sh
// string, where the single quote is the only character with meaning: close the
// quote, emit an escaped one, reopen. Everything else — including whitespace
// inside a JSON string value — passes through byte for byte.
func shellSingleQuoted(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// The opt-in that loses to "independent" is one an operator deliberately added,
// and nothing else in the system says it went inert — the failure they would
// otherwise see is children merging to main out of order, long after the fact.
// So the poll loop names the bead. Once: the label pair is static, and a WARN
// repeated every poll cycle buries the rest of the log.
func TestPollAnvil_WarnsOnceAboutConflictingEpicLabels(t *testing.T) {
	withFakeBd(t, `cat <<'JSON'
[
  {"id":"parent-1","status":"open","labels":["crucible","independent"]},
  {"id":"parent-2","status":"open","labels":["crucible"]},
  {"id":"child-1","status":"open","labels":["independent"]}
]
JSON`)

	var logged bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	p := New(map[string]config.AnvilConfig{"repo": {Path: t.TempDir()}})
	for i := 0; i < 3; i++ {
		_, err := p.PollSingle(context.Background(), "repo")
		require.NoError(t, err)
	}

	warnings := strings.Count(logged.String(), "the opt-in is ignored")
	assert.Equal(t, 1, warnings, "three polls, one warning: %s", logged.String())
	assert.Contains(t, logged.String(), "parent-1")
	assert.NotContains(t, logged.String(), "parent-2", "an opt-in with no opt-out is not a conflict")
	assert.NotContains(t, logged.String(), "child-1", "an opt-out with no opt-in is not a conflict")
}

// The ids come out of a dolt database that syncs through the git remote, and
// they are passed to bd positionally. One shaped like a flag is dropped from the
// query rather than handed over — which leaves it counted as an ordinary child,
// the same direction every other unreadable case takes.
func TestResolveIndependent_DropsFlagShapedIDs(t *testing.T) {
	withFakeBd(t, `printf '%s\n' "$*" >&2
case "$*" in
  *--force*) echo "bd read an argument as a flag" >&2; exit 2 ;;
esac
echo '[{"id":"child-1","labels":["independent"]}]'`)

	got, err := resolveIndependent(context.Background(), []string{"--force", "child-1", ""}, t.TempDir())

	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"child-1": true}, got)
}

// Every id screened out leaves nothing to ask about, and asking bd for nothing
// would return every bead in the anvil.
func TestResolveIndependent_NoUsableIDsAsksNothing(t *testing.T) {
	withFakeBd(t, `echo "bd must not be called" >&2; exit 9`)

	got, err := resolveIndependent(context.Background(), []string{"--force"}, t.TempDir())

	require.NoError(t, err)
	assert.Empty(t, got)
}
