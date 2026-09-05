package state

import (
	"strings"
	"testing"
	"time"
)

func rollbackFixture(anvil string) DeployFailure {
	return DeployFailure{
		Anvil:        anvil,
		Unit:         "forge",
		Reason:       DeployReasonRestartFailed,
		Detail:       "restart failed: signal: killed",
		AttemptedSHA: "cafebabe0123456789abcdef",
		RestoredSHA:  "deadbeef9876543210fedcba",
		RolledBack:   true,
		FailedAt:     time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
	}
}

func TestRecordAndClearDeployFailures(t *testing.T) {
	db := openTestDB(t)

	if err := db.RecordDeployFailure(rollbackFixture("forge")); err != nil {
		t.Fatalf("RecordDeployFailure: %v", err)
	}
	rows, err := db.DeployFailures()
	if err != nil {
		t.Fatalf("DeployFailures: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want one failure, got %+v", rows)
	}
	got := rows[0]
	if got.Reason != DeployReasonRestartFailed || !got.RolledBack {
		t.Fatalf("round-trip lost the reason/rollback flag: %+v", got)
	}
	if got.AttemptedSHA != "cafebabe0123456789abcdef" || got.RestoredSHA != "deadbeef9876543210fedcba" {
		t.Fatalf("round-trip lost the build SHAs: %+v", got)
	}
	if !got.FailedAt.Equal(rollbackFixture("forge").FailedAt) {
		t.Fatalf("FailedAt = %s, want the recorded time", got.FailedAt)
	}

	// A second reason is its own row: a later deferral must not erase the record
	// of an earlier rollback, which is the more serious of the two.
	deferred := DeployFailure{
		Anvil:  "forge",
		Reason: DeployReasonDrainTimeout,
		Detail: "1 worker(s) still active after 30m0s (max 30m0s): Forge-w0",
	}
	if err := db.RecordDeployFailure(deferred); err != nil {
		t.Fatalf("RecordDeployFailure: %v", err)
	}
	if rows, _ = db.DeployFailures(); len(rows) != 2 {
		t.Fatalf("want both reasons listed, got %+v", rows)
	}

	// Re-recording the same reason refreshes rather than duplicates.
	if err := db.RecordDeployFailure(deferred); err != nil {
		t.Fatalf("RecordDeployFailure: %v", err)
	}
	if rows, _ = db.DeployFailures(); len(rows) != 2 {
		t.Fatalf("re-recording must overwrite the same anvil+reason, got %+v", rows)
	}

	// A targeted clear resolves only the reason it names.
	n, err := db.ClearDeployFailures("forge", DeployReasonDrainTimeout)
	if err != nil {
		t.Fatalf("ClearDeployFailures: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared %d rows, want 1", n)
	}
	rows, _ = db.DeployFailures()
	if len(rows) != 1 || rows[0].Reason != DeployReasonRestartFailed {
		t.Fatalf("the rollback row must survive a drain-timeout clear, got %+v", rows)
	}

	// An unqualified clear resolves everything for the anvil.
	if n, err = db.ClearDeployFailures("forge"); err != nil || n != 1 {
		t.Fatalf("ClearDeployFailures(all) = %d, %v", n, err)
	}
	if rows, _ = db.DeployFailures(); len(rows) != 0 {
		t.Fatalf("want no failures left, got %+v", rows)
	}
}

func TestDeployFailureRendering(t *testing.T) {
	f := rollbackFixture("forge")
	title := f.Title()
	if !strings.Contains(title, "rolled back") || !strings.Contains(title, "restart failed") {
		t.Errorf("Title = %q, want it to name the rollback and its cause", title)
	}
	summary := f.Summary()
	for _, want := range []string{"attempted cafebabe0123", "restored deadbeef9876", "signal: killed", "2026-08-06T12:00:00Z"} {
		if !strings.Contains(summary, want) {
			t.Errorf("Summary = %q, missing %q", summary, want)
		}
	}

	// A deferral has nothing to restore and says so as a deferral, not a failure.
	deferred := DeployFailure{Anvil: "forge", Reason: DeployReasonDrainTimeout, Detail: "workers busy"}
	if got := deferred.Title(); !strings.Contains(got, "deferred") {
		t.Errorf("Title = %q, want it to read as a deferral", got)
	}
	if got := deferred.Summary(); strings.Contains(got, "restored") {
		t.Errorf("Summary = %q, must not claim a restore", got)
	}

	// A failed rollback is called out as the worst state, not as a plain rollback.
	broken := DeployFailure{Anvil: "forge", Reason: DeployReasonRollbackFailed, Detail: "permission denied"}
	if got := broken.Title(); !strings.Contains(got, "rollback failed") {
		t.Errorf("Title = %q, want it to name the failed rollback", got)
	}

	// The two pull-side outcomes are not "failed" deploys in the sense the
	// others are: nothing was built and nothing was swapped, so the headline has
	// to say what an operator actually has to deal with — a checkout that cannot
	// be fast-forwarded, or work sitting in a stash.
	blocked := DeployFailure{Anvil: "forge", Reason: DeployReasonPullBlocked, Detail: "unmerged paths"}
	if got := blocked.Title(); !strings.Contains(got, "fast-forwarded") {
		t.Errorf("Title = %q, want it to name the blocked checkout", got)
	}
	stashed := DeployFailure{Anvil: "forge", Reason: DeployReasonStashRetained, Detail: "pop conflicted"}
	if got := stashed.Title(); !strings.Contains(got, "stash") {
		t.Errorf("Title = %q, want it to name the stash", got)
	}
	if got := stashed.Title(); strings.Contains(got, "failed") {
		t.Errorf("Title = %q, must not read as a failed build — nothing was built", got)
	}

	// Every reason a deploy can raise must render as prose, so a row added here
	// without a label does not surface as a bare snake_case token.
	for _, reason := range []string{
		DeployReasonDrainTimeout, DeployReasonSwapFailed, DeployReasonRestartFailed,
		DeployReasonRollbackFailed, DeployReasonPullBlocked, DeployReasonStashRetained,
	} {
		if _, ok := deployReasonLabels[reason]; !ok {
			t.Errorf("reason %q has no human-readable label", reason)
		}
	}
}

func TestNeedsAttentionBeads_IncludesDeployFailures(t *testing.T) {
	db := openTestDB(t)

	items, err := db.NeedsAttentionBeads(5, 5, 3, StalenessParams{})
	if err != nil {
		t.Fatalf("NeedsAttentionBeads: %v", err)
	}
	for _, it := range items {
		if it.Kind == AttentionKindDeploy {
			t.Fatalf("a forge with no failed deploy must produce no deploy entry: %+v", it)
		}
	}

	if err := db.RecordDeployFailure(rollbackFixture("forge")); err != nil {
		t.Fatalf("RecordDeployFailure: %v", err)
	}

	items, err = db.NeedsAttentionBeads(5, 5, 3, StalenessParams{})
	if err != nil {
		t.Fatalf("NeedsAttentionBeads: %v", err)
	}
	var found *NeedsAttentionBead
	for i := range items {
		if items[i].Kind == AttentionKindDeploy {
			found = &items[i]
		}
	}
	if found == nil {
		t.Fatal("a rolled-back self-deploy must appear in needs-attention")
	}
	if found.Anvil != "forge" {
		t.Fatalf("unexpected anvil: %+v", found)
	}
	if found.BeadID != "" || found.PRID != 0 {
		t.Fatalf("a deploy entry is not bead- or PR-scoped: %+v", found)
	}
	if !found.NeedsHuman {
		t.Fatal("a rolled-back deploy needs human intervention")
	}
	if !strings.Contains(found.Title, "rolled back") || !strings.Contains(found.Title, "unit forge") {
		t.Fatalf("title %q must say what happened and to which unit", found.Title)
	}
	// The row has to carry both builds and the time, which is what tells an
	// operator whether the merged fix is actually running.
	for _, want := range []string{"attempted cafebabe0123", "restored deadbeef9876", "2026-08-06T12:00:00Z"} {
		if !strings.Contains(found.Reason, want) {
			t.Fatalf("reason %q missing %q", found.Reason, want)
		}
	}

	// A later successful deploy clears the entry with no operator action.
	if _, err := db.ClearDeployFailures("forge"); err != nil {
		t.Fatalf("ClearDeployFailures: %v", err)
	}
	items, _ = db.NeedsAttentionBeads(5, 5, 3, StalenessParams{})
	for _, it := range items {
		if it.Kind == AttentionKindDeploy {
			t.Fatalf("the entry must clear itself once resolved: %+v", it)
		}
	}
}
