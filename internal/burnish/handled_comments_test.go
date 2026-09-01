package burnish

// Tests for the handled-comment filter.
//
// A review thread carries its handled state on the platform: GetReviewComments
// drops resolved threads, so the thread half of the actionable set drains as
// Burnish resolves them. A review-level or PR-level comment belongs to no
// thread and can never be resolved, and nothing distinguished one Burnish had
// already worked through from one that had just arrived — so every Copilot
// review body and every Assay summary stayed actionable for the life of the PR
// and was re-sent to the model on every round.
//
// Munin#5423 measured that residue growing 3 -> 4 -> 5 -> 6 across four rounds
// while the resolvable thread count fell 9 -> 4 -> 1: round 3's Assay returned
// findings=0, yet five stale summaries kept Burnish running.

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
)

func handledTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// reviewLevel is a comment with no thread — the kind that has no platform
// resolution state and so needs the handled set.
func reviewLevel(id, body string) vcs.ReviewComment {
	return vcs.ReviewComment{Author: "copilot-pull-request-reviewer", Body: body, ID: id, State: "COMMENTED"}
}

// threadComment is resolvable on the platform, so the handled set must leave it
// alone: resolution, not this filter, is what removes it.
func threadComment(threadID, body string) vcs.ReviewComment {
	return vcs.ReviewComment{Author: "Robin831", Body: body, ThreadID: threadID, Path: "a.go", Line: 7}
}

func TestFilterActionableCommentsDropsHandledNonThreadComments(t *testing.T) {
	summary := reviewLevel("R_1", "Copilot review summary, no request in it")
	fresh := reviewLevel("R_2", "A newly posted review body that needs action")
	thread := threadComment("PRRT_1", "please rename this")

	all := []vcs.ReviewComment{summary, fresh, thread}

	// Nothing recorded yet: everything is actionable, as before.
	require.Len(t, filterActionableComments(all, nil), 3)

	db := handledTestDB(t)
	require.NoError(t, db.MarkCommentsHandled("munin", 5423, nonThreadHandled([]vcs.ReviewComment{summary})))

	handled, err := db.HandledComments("munin", 5423)
	require.NoError(t, err)

	got := filterActionableComments(all, handled)
	require.Len(t, got, 2, "the handled review-level comment should be dropped")
	for _, c := range got {
		require.NotEqual(t, summary.ID, c.ID, "handled comment came back as actionable")
	}
}

// TestFilterActionableCommentsKeepsHandledThreadComments pins the division of
// labour: a thread is removed by being resolved, never by this filter. Dropping
// one here on an ID collision would hide a thread the platform still shows as
// unresolved, which is what keeps the PR blocked.
func TestFilterActionableCommentsKeepsHandledThreadComments(t *testing.T) {
	thread := vcs.ReviewComment{Author: "Robin831", Body: "same body", ThreadID: "PRRT_1", ID: "R_1"}
	db := handledTestDB(t)
	require.NoError(t, db.MarkCommentsHandled("munin", 5423, []state.HandledComment{
		{ID: "R_1", BodyHash: state.CommentBodyHash("same body")},
	}))
	handled, err := db.HandledComments("munin", 5423)
	require.NoError(t, err)

	require.Len(t, filterActionableComments([]vcs.ReviewComment{thread}, handled), 1)
}

// TestFilterActionableCommentsResurfacesEditedComment is why the body hash is
// part of the identity: an edit keeps the comment's ID, and matching on ID
// alone would suppress a new request forever.
func TestFilterActionableCommentsResurfacesEditedComment(t *testing.T) {
	original := reviewLevel("R_1", "original body of the review comment")
	db := handledTestDB(t)
	require.NoError(t, db.MarkCommentsHandled("munin", 5423, nonThreadHandled([]vcs.ReviewComment{original})))
	handled, err := db.HandledComments("munin", 5423)
	require.NoError(t, err)

	require.Empty(t, filterActionableComments([]vcs.ReviewComment{original}, handled))

	edited := reviewLevel("R_1", "original body of the review comment, plus a new request")
	require.Len(t, filterActionableComments([]vcs.ReviewComment{edited}, handled), 1,
		"an edited comment must become actionable again")
}

// TestNonThreadHandledSelectsOnlyRecordableComments guards the two exclusions.
// A comment with no ID cannot be recorded under anything stable, and recording
// it would hash a shared empty ID into one key that suppresses every other
// ID-less comment on the PR.
func TestNonThreadHandledSelectsOnlyRecordableComments(t *testing.T) {
	got := nonThreadHandled([]vcs.ReviewComment{
		reviewLevel("R_1", "recordable: no thread, has an id"),
		threadComment("PRRT_1", "resolved on the platform instead"),
		{Author: "someone", Body: "no id, nothing stable to record it under"},
	})
	require.Len(t, got, 1)
	require.Equal(t, "R_1", got[0].ID)
	require.Equal(t, state.CommentBodyHash("recordable: no thread, has an id"), got[0].BodyHash)
}

// TestHandledCommentsAreScopedToTheirPR keeps one PR's residue from silencing
// another's: IDs are unique in practice, but the query must not rely on that.
func TestHandledCommentsAreScopedToTheirPR(t *testing.T) {
	db := handledTestDB(t)
	c := reviewLevel("R_1", "a review body")
	require.NoError(t, db.MarkCommentsHandled("munin", 5423, nonThreadHandled([]vcs.ReviewComment{c})))

	other, err := db.HandledComments("munin", 9999)
	require.NoError(t, err)
	require.Len(t, filterActionableComments([]vcs.ReviewComment{c}, other), 1)

	otherAnvil, err := db.HandledComments("heimdall", 5423)
	require.NoError(t, err)
	require.Len(t, filterActionableComments([]vcs.ReviewComment{c}, otherAnvil), 1)
}

// TestMarkCommentsHandledIsIdempotent — a retried round re-records the same
// comment, and that must not error on the primary key.
func TestMarkCommentsHandledIsIdempotent(t *testing.T) {
	db := handledTestDB(t)
	cs := nonThreadHandled([]vcs.ReviewComment{reviewLevel("R_1", "a review body")})
	require.NoError(t, db.MarkCommentsHandled("munin", 5423, cs))
	require.NoError(t, db.MarkCommentsHandled("munin", 5423, cs))

	handled, err := db.HandledComments("munin", 5423)
	require.NoError(t, err)
	require.Len(t, handled, 1)
}

// TestTheMuninResidueDrains is the regression this whole change exists for:
// the residue that grew every round must now fall to nothing once handled,
// while an unresolved thread and a genuinely new review body still get through.
func TestTheMuninResidueDrains(t *testing.T) {
	db := handledTestDB(t)

	// Round 1's actionable set: three non-thread comments (a Copilot review
	// body, an Assay summary, a github-actions comment) plus a live thread.
	residue := []vcs.ReviewComment{
		reviewLevel("R_copilot_1", "Copilot's review of the first head"),
		reviewLevel("R_assay_summary", "Assay summary comment, edited in place each run"),
		reviewLevel("R_gha", "github-actions changelog reminder"),
	}
	round1 := append(append([]vcs.ReviewComment{}, residue...), threadComment("PRRT_1", "rename this"))
	require.Len(t, filterActionableComments(round1, nil), 4)

	require.NoError(t, db.MarkCommentsHandled("munin", 5423, nonThreadHandled(round1)))
	handled, err := db.HandledComments("munin", 5423)
	require.NoError(t, err)

	// Round 2: the thread was resolved (so GetReviewComments no longer returns
	// it) and Copilot reviewed the new head. Only the new body survives —
	// previously all three of the old ones came back too.
	round2 := append(append([]vcs.ReviewComment{}, residue...),
		reviewLevel("R_copilot_2", "Copilot's review of the second head"))
	got := filterActionableComments(round2, handled)
	require.Len(t, got, 1)
	require.Equal(t, "R_copilot_2", got[0].ID)
}
