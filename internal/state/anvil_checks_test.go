package state

import (
	"strings"
	"testing"
	"time"
)

// thresholds is the shape every test here judges against: one checker, one age.
func thresholds(checker string, d time.Duration) map[string]time.Duration {
	return map[string]time.Duration{checker: d}
}

// TestStaleChecks_SilentFailureIsVisible is the fault this exists for. A checker
// that runs and fails every cycle writes no success, so the row ages — and it
// does so without anything here reading how the failure was classified, which
// is the whole point: the incident's failure was classified transient and
// retried quietly for a day.
func TestStaleChecks_SilentFailureIsVisible(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{
		Thresholds: thresholds(CheckerDepcheck, time.Hour),
		Now:        time.Now().Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("got %d stale checks, want 1", len(stale))
	}
	if stale[0].EverSucceeded {
		t.Error("a checker that has never completed must not report as having succeeded")
	}
	if stale[0].Anvil != "explorer" || stale[0].Checker != CheckerDepcheck {
		t.Errorf("got %+v", stale[0])
	}
}

// A completed cycle withdraws the entry by moving the timestamp — there is no
// row to clear, which is the property that makes this safe to leave running.
func TestStaleChecks_SuccessWithdrawsTheEntry(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCheckSuccess("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{
		Thresholds: thresholds(CheckerDepcheck, time.Hour),
		Now:        time.Now().Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Errorf("a checker that completed within its threshold is not stale, got %+v", stale)
	}
}

// TestStaleChecks_StoppedAfterWorking separates the two cases the message has to
// tell apart: this one HAS succeeded before, so it is a regression rather than a
// misconfiguration.
func TestStaleChecks_StoppedAfterWorking(t *testing.T) {
	db := openTestDB(t)
	if err := db.RecordCheckSuccess("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{
		Thresholds: thresholds(CheckerDepcheck, time.Hour),
		Now:        time.Now().Add(5 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("got %d stale checks, want 1", len(stale))
	}
	if !stale[0].EverSucceeded {
		t.Error("a checker that has completed before must report as having succeeded")
	}
	if got := stale[0].Title(); got == "" || !strings.Contains(got, "has not completed in") {
		t.Errorf("title should describe a lapse, got %q", got)
	}
}

// A checker with no threshold is never judged. That is what keeps a DISABLED
// scanner from being reported as a broken one.
func TestStaleChecks_UnjudgedCheckerIsIgnored(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("explorer", CheckerVulncheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{
		Thresholds: thresholds(CheckerDepcheck, time.Hour), // vulncheck absent
		Now:        time.Now().Add(100 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Errorf("a checker with no threshold must not be judged, got %+v", stale)
	}
}

// A deregistered anvil's rows would otherwise be stale forever with nothing an
// operator could do to clear them.
func TestStaleChecks_UnknownAnvilIsIgnored(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("removed", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCheck("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{
		Thresholds:  thresholds(CheckerDepcheck, time.Hour),
		KnownAnvils: map[string]bool{"explorer": true},
		Now:         time.Now().Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].Anvil != "explorer" {
		t.Errorf("only the still-registered anvil should be judged, got %+v", stale)
	}
}

// BeginCheck must not move the clock on a checker that has already succeeded —
// otherwise every cycle would refresh the row and nothing could ever go stale.
func TestBeginCheck_DoesNotResetAWorkingChecker(t *testing.T) {
	db := openTestDB(t)
	if err := db.RecordCheckSuccess("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCheck("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{
		Thresholds: thresholds(CheckerDepcheck, time.Hour),
		Now:        time.Now().Add(5 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("got %d stale checks, want 1", len(stale))
	}
	if !stale[0].EverSucceeded {
		t.Error("BeginCheck must not erase the recorded success")
	}
}

// Two anvils sharing one checker keep separate histories: the rows are keyed
// (anvil, checker), so one anvil completing a cycle says nothing about another.
// Both are overdue here — what must not bleed is which of them has ever
// succeeded, since that decides which sentence the operator is shown.
func TestStaleChecks_AnvilsKeepSeparateHistories(t *testing.T) {
	db := openTestDB(t)
	if err := db.RecordCheckSuccess("munin", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCheck("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{
		Thresholds: thresholds(CheckerDepcheck, time.Hour),
		Now:        time.Now().Add(90 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 2 {
		t.Fatalf("got %d stale checks, want 2", len(stale))
	}
	byAnvil := map[string]StaleCheck{}
	for _, sc := range stale {
		byAnvil[sc.Anvil] = sc
	}
	if !byAnvil["munin"].EverSucceeded {
		t.Error("munin completed a cycle and must be reported as a lapse")
	}
	if byAnvil["explorer"].EverSucceeded {
		t.Error("explorer never completed one and must be reported as never having run")
	}
}

// The ordering is tested on constructed values: the stored timestamps are
// written with time.Now(), so rows driven through the table are all the same
// age and could never distinguish the comparison.
func TestSortStaleChecks_MostOverdueFirst(t *testing.T) {
	stale := []StaleCheck{
		{Anvil: "b", Checker: CheckerDepcheck, Age: time.Hour},
		{Anvil: "a", Checker: CheckerDepcheck, Age: 10 * time.Hour},
		{Anvil: "c", Checker: CheckerVulncheck, Age: 5 * time.Hour},
	}
	sortStaleChecks(stale)

	if stale[0].Anvil != "a" || stale[1].Anvil != "c" || stale[2].Anvil != "b" {
		t.Errorf("wrong order: %v %v %v", stale[0].Anvil, stale[1].Anvil, stale[2].Anvil)
	}
}

// Equal ages fall back to anvil then checker, so the panel does not reshuffle
// between refreshes for rows that are equally overdue.
func TestSortStaleChecks_StableOnEqualAges(t *testing.T) {
	stale := []StaleCheck{
		{Anvil: "b", Checker: CheckerDepcheck, Age: time.Hour},
		{Anvil: "a", Checker: CheckerVulncheck, Age: time.Hour},
		{Anvil: "a", Checker: CheckerDepcheck, Age: time.Hour},
	}
	sortStaleChecks(stale)

	if stale[0].Anvil != "a" || stale[0].Checker != CheckerDepcheck {
		t.Errorf("first = %s/%s", stale[0].Anvil, stale[0].Checker)
	}
	if stale[1].Anvil != "a" || stale[1].Checker != CheckerVulncheck {
		t.Errorf("second = %s/%s", stale[1].Anvil, stale[1].Checker)
	}
	if stale[2].Anvil != "b" {
		t.Errorf("third = %s", stale[2].Anvil)
	}
}

func TestStaleChecks_NoThresholdsJudgesNothing(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{Now: time.Now().Add(1000 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Errorf("staleness_check: false must judge nothing, got %+v", stale)
	}
}

func TestCheckRecords_RejectsEmptyKeys(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("", CheckerDepcheck); err == nil {
		t.Error("expected an error for an empty anvil")
	}
	if err := db.RecordCheckSuccess("explorer", ""); err == nil {
		t.Error("expected an error for an empty checker")
	}
}

// StalenessThresholds turns intervals into ages, and must leave a disabled
// checker (interval 0) out entirely rather than treating it as instantaneous.
func TestStalenessThresholds(t *testing.T) {
	got := StalenessThresholds(StalenessIntervals{
		Multiplier: 3,
		Depcheck:   24 * time.Hour,
		Vulncheck:  0, // disabled
		Questgiver: 24 * time.Hour,
		Poll:       5 * time.Minute,
	})

	if got[CheckerDepcheck] != 72*time.Hour {
		t.Errorf("depcheck threshold = %v, want 72h", got[CheckerDepcheck])
	}
	if _, ok := got[CheckerVulncheck]; ok {
		t.Error("a disabled checker must not be judged")
	}
	// Reconcile rides every ReconcilePollDivisor-th poll.
	if want := 3 * 5 * time.Minute * ReconcilePollDivisor; got[CheckerPRReconcile] != want {
		t.Errorf("reconcile threshold = %v, want %v", got[CheckerPRReconcile], want)
	}
}

func TestStalenessThresholds_ZeroMultiplierFallsBackToTheDefault(t *testing.T) {
	got := StalenessThresholds(StalenessIntervals{Depcheck: time.Hour})
	if got[CheckerDepcheck] != time.Duration(DefaultStalenessMultiplier)*time.Hour {
		t.Errorf("got %v", got[CheckerDepcheck])
	}
}

// The two sentences are different claims, and the panel has to make the
// difference visible: one is a regression, the other is usually a checker that
// was never able to run at all.
func TestStaleCheckTitles(t *testing.T) {
	never := StaleCheck{Anvil: "explorer", Checker: CheckerDepcheck, Age: 3 * 24 * time.Hour}
	if got := never.Title(); !strings.Contains(got, "has never completed") {
		t.Errorf("got %q", got)
	}

	lapsed := StaleCheck{Anvil: "explorer", Checker: CheckerDepcheck, EverSucceeded: true, Age: 3 * 24 * time.Hour}
	if got := lapsed.Title(); !strings.Contains(got, "has not completed in 3 days") {
		t.Errorf("got %q", got)
	}
}

// The detail must not claim a cause. It is raised precisely because no
// classification can be trusted to have fired, so naming one would be the kind
// of wrong that sends an operator to the wrong place.
func TestStaleCheckDetailClaimsNoCause(t *testing.T) {
	d := StaleCheck{
		Anvil: "explorer", Checker: CheckerPRReconcile,
		EverSucceeded: true, Age: 5 * time.Hour, Threshold: 150 * time.Minute,
	}.Detail()

	if !strings.Contains(d, "not saying why") {
		t.Errorf("detail should disclaim a cause, got %q", d)
	}
	if !strings.Contains(d, "withdraws itself") {
		t.Errorf("detail should say the entry clears itself, got %q", d)
	}
	if !strings.Contains(d, "PR reconcile") {
		t.Errorf("detail should name the checker in words, got %q", d)
	}
}

// A caller with no anvils configured supplies an EMPTY map, not a nil one, and
// that must filter everything out rather than judge everything. Read the other
// way it reports every leftover row in the table, none of which names an anvil
// the reader still has — the false positive that would train an operator to
// ignore the panel.
func TestStaleChecks_EmptyKnownAnvilsFiltersEverything(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{
		Thresholds:  thresholds(CheckerDepcheck, time.Hour),
		KnownAnvils: map[string]bool{},
		Now:         time.Now().Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Errorf("no anvils are registered, so nothing can be judged; got %+v", stale)
	}
}

// Nil is the "do not filter" case, which is what a caller that does not know the
// anvil set passes.
func TestStaleChecks_NilKnownAnvilsDoesNotFilter(t *testing.T) {
	db := openTestDB(t)
	if err := db.BeginCheck("explorer", CheckerDepcheck); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleChecks(StalenessParams{
		Thresholds: thresholds(CheckerDepcheck, time.Hour),
		Now:        time.Now().Add(4 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Errorf("got %d, want 1", len(stale))
	}
}
