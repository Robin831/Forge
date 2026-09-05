package questgiver

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every resolution test below runs against a pool holding BOTH anvils' beads,
// because that is the only shape in which the bug exists: Munin and Explorer
// share one beads database on purpose, a quest name is a filename in each
// repository, and with a single anvil's bead in the pool a title-prefix lookup
// returns the right answer by accident.

// fakePins is an in-memory beadPinStore.
type fakePins struct {
	pins    map[string]string
	readErr error
}

func newFakePins() *fakePins { return &fakePins{pins: map[string]string{}} }

func (f *fakePins) key(anvil, kind, key string) string { return anvil + "/" + kind + "/" + key }

func (f *fakePins) AnvilBead(anvil, kind, key string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.pins[f.key(anvil, kind, key)], nil
}

func (f *fakePins) SetAnvilBead(anvil, kind, key, beadID string) error {
	f.pins[f.key(anvil, kind, key)] = beadID
	return nil
}

func (f *fakePins) ClearAnvilBead(anvil, kind, key string) error {
	delete(f.pins, f.key(anvil, kind, key))
	return nil
}

// questBead builds a bead in exactly the shape questgiver writes: the
// description comes from the real writer, so a change to its format breaks these
// tests rather than silently detaching them from production.
func questBead(id, anvil, quest, status string) bdBead {
	q := &Quest{Name: quest, FilePath: ".quests/" + quest + ".yaml"}
	r := &QuestResult{FailedStep: 2, ErrorMessage: "timeout waiting for #submit"}
	return bdBead{
		ID:          id,
		Title:       questBeadTitlePrefix + quest + " — step 2 (click)",
		Status:      status,
		Description: questBeadDescription(anvil, q, r, "click"),
	}
}

// lookupOver builds a lookup reading one shared pool, the way
// Monitor.questBeadLookupFor does against a real beads workspace.
func lookupOver(anvil, quest string, pins beadPinStore, pool []bdBead) questBeadLookup {
	return questBeadLookup{
		anvil: anvil,
		quest: quest,
		pins:  pins,
		showBead: func(id string) (*bdBead, error) {
			for i := range pool {
				if pool[i].ID == id {
					return &pool[i], nil
				}
			}
			return nil, nil // bd's "no such bead" answer
		},
		activeBeads: func() ([]bdBead, error) {
			var active []bdBead
			for _, b := range pool {
				if isActiveBeadStatus(b.Status) {
					active = append(active, b)
				}
			}
			return active, nil
		},
	}
}

// TestResolve_SameQuestNameInTwoAnvils is the bug: both repositories have a
// quest called "login", both fail, and the second anvil to scan must not be
// handed the first's bead — under the title prefix it was, and the second
// anvil's failure was never reported at all.
func TestResolve_SameQuestNameInTwoAnvils(t *testing.T) {
	pool := []bdBead{
		questBead("bd-munin", "munin", "login", "open"),
		questBead("bd-explorer", "explorer", "login", "open"),
	}
	pins := newFakePins()

	munin, err := lookupOver("munin", "login", pins, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, munin)
	assert.Equal(t, "bd-munin", munin.ID)

	explorer, err := lookupOver("explorer", "login", pins, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, explorer)
	assert.Equal(t, "bd-explorer", explorer.ID,
		"explorer must not be handed munin's bead for a quest of the same name")
}

// TestResolve_OtherAnvilsBeadIsNotAdopted: the pool holds only the OTHER
// anvil's bead for this quest name. Explorer must report "no bead" so that its
// own failure gets one — reporting a duplicate-looking bead is a nuisance,
// reporting nothing loses the failure.
func TestResolve_OtherAnvilsBeadIsNotAdopted(t *testing.T) {
	pool := []bdBead{questBead("bd-munin", "munin", "login", "open")}
	pins := newFakePins()

	got, err := lookupOver("explorer", "login", pins, pool).resolve()
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, pins.pins, "a bead this anvil does not own must not be pinned to it")
}

// TestResolve_QuestNameIsNotAPrefixMatch: "login" is a prefix of "login-admin",
// so the old title-prefix check reported the wrong quest's bead as this one's —
// inside a single anvil, no second repository needed.
func TestResolve_QuestNameIsNotAPrefixMatch(t *testing.T) {
	pool := []bdBead{
		questBead("bd-munin-admin", "munin", "login-admin", "open"),
		questBead("bd-explorer", "explorer", "login", "open"),
	}
	pins := newFakePins()

	got, err := lookupOver("munin", "login", pins, pool).resolve()
	require.NoError(t, err)
	assert.Nil(t, got, "login must not match login-admin's bead")
}

// TestResolve_AdoptsThisAnvilsPreFixBead: a bead created before ownership was
// recorded is found by the anvil whose failure it reports, rather than
// duplicated on the first run after the pin ships.
func TestResolve_AdoptsThisAnvilsPreFixBead(t *testing.T) {
	pool := []bdBead{
		questBead("bd-munin", "munin", "login", "open"),
		questBead("bd-explorer", "explorer", "login", "open"),
	}
	pins := newFakePins() // nothing recorded before the fix shipped

	got, err := lookupOver("explorer", "login", pins, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "bd-explorer", got.ID)
	assert.Equal(t, "bd-explorer", pins.pins["explorer/questgiver/login"],
		"an adopted bead is pinned, so the next run does not re-derive it from text")
}

// TestResolve_PinnedBeadSurvivesARetitle: the pin is the bead's identity, so a
// bead a human has retitled is still this anvil's bead. The pool holds the other
// anvil's untouched bead, which the retitled one must not fall through to.
func TestResolve_PinnedBeadSurvivesARetitle(t *testing.T) {
	retitled := questBead("bd-explorer", "explorer", "login", "open")
	retitled.Title = "Login quest is flaky again"
	pool := []bdBead{
		questBead("bd-munin", "munin", "login", "open"),
		retitled,
	}
	pins := newFakePins()
	require.NoError(t, pins.SetAnvilBead("explorer", questBeadKind, "login", "bd-explorer"))

	got, err := lookupOver("explorer", "login", pins, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "bd-explorer", got.ID)
}

// TestResolve_InProgressBeadStillSuppresses: a bead someone has claimed is still
// an open report of the failure.
func TestResolve_InProgressBeadStillSuppresses(t *testing.T) {
	pool := []bdBead{
		questBead("bd-munin", "munin", "login", "open"),
		questBead("bd-explorer", "explorer", "login", "in_progress"),
	}
	pins := newFakePins()

	got, err := lookupOver("explorer", "login", pins, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "bd-explorer", got.ID)
}

// TestResolve_ForgetsAClosedBead: a quest that starts failing again after its
// bead was closed must be reported again, and the stale pin must go.
func TestResolve_ForgetsAClosedBead(t *testing.T) {
	pool := []bdBead{
		questBead("bd-munin", "munin", "login", "open"),
		questBead("bd-explorer", "explorer", "login", "closed"),
	}
	pins := newFakePins()
	require.NoError(t, pins.SetAnvilBead("explorer", questBeadKind, "login", "bd-explorer"))

	got, err := lookupOver("explorer", "login", pins, pool).resolve()
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, pins.pins)
}

// TestResolve_ForgetsABeadBdReportsAsAbsent: a deleted bead drops the pin, which
// is what separates it from an unreadable one.
func TestResolve_ForgetsABeadBdReportsAsAbsent(t *testing.T) {
	pool := []bdBead{questBead("bd-munin", "munin", "login", "open")}
	pins := newFakePins()
	require.NoError(t, pins.SetAnvilBead("explorer", questBeadKind, "login", "bd-deleted"))

	got, err := lookupOver("explorer", "login", pins, pool).resolve()
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, pins.pins)
}

// TestResolve_KeepsThePinWhenTheBeadCannotBeRead: bd exits non-zero both for a
// deleted bead and for a timeout. Dropping the pin on a timeout would file the
// second bead the pin exists to prevent, so the run reports an error instead and
// the caller skips creation.
func TestResolve_KeepsThePinWhenTheBeadCannotBeRead(t *testing.T) {
	pins := newFakePins()
	require.NoError(t, pins.SetAnvilBead("explorer", questBeadKind, "login", "bd-explorer"))

	l := lookupOver("explorer", "login", pins,
		[]bdBead{questBead("bd-munin", "munin", "login", "open")})
	l.showBead = func(string) (*bdBead, error) { return nil, errors.New("bd timed out") }

	got, err := l.resolve()
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Equal(t, "bd-explorer", pins.pins["explorer/questgiver/login"],
		"an unreadable bead is not an absent one")
}

// TestResolve_ListingFailureIsAnError: an unknown answer must not read as "no
// bead", because the caller would then file one every cycle.
func TestResolve_ListingFailureIsAnError(t *testing.T) {
	l := lookupOver("explorer", "login", newFakePins(), nil)
	l.activeBeads = func() ([]bdBead, error) { return nil, errors.New("bd list failed") }

	got, err := l.resolve()
	require.Error(t, err)
	assert.Nil(t, got)
}

// TestResolve_DoesNotAdoptAnUnattributedBead: a bead whose description names no
// anvil belongs to nobody. Both anvils creating their own is the nuisance;
// either one adopting it silences the other's failure.
func TestResolve_DoesNotAdoptAnUnattributedBead(t *testing.T) {
	orphan := questBead("bd-orphan", "munin", "login", "open")
	orphan.Description = "Quest: login\nFailed step: 2\nError: timeout"
	pool := []bdBead{orphan}

	for _, anvil := range []string{"munin", "explorer"} {
		pins := newFakePins()
		got, err := lookupOver(anvil, "login", pins, pool).resolve()
		require.NoError(t, err)
		assert.Nil(t, got, "%s must not adopt an unattributed bead", anvil)
		assert.Empty(t, pins.pins)
	}
}

// TestQuestBeadOwnerRoundTripsTheWriter pins the reader to the writer: the
// adoption path only works while questBeadDescription keeps emitting the two
// lines questBeadOwner reads.
func TestQuestBeadOwnerRoundTripsTheWriter(t *testing.T) {
	desc := questBeadDescription("fhi.munin.explorer",
		&Quest{Name: "kilde-search", FilePath: ".quests/kilde-search.yaml"},
		&QuestResult{FailedStep: 0, ErrorMessage: "no results"}, "click")

	anvil, quest := questBeadOwner(desc)
	assert.Equal(t, "fhi.munin.explorer", anvil)
	assert.Equal(t, "kilde-search", quest)
}

func TestQuestBeadOwner_MissingLines(t *testing.T) {
	anvil, quest := questBeadOwner("Failed step: 1\nError: boom")
	assert.Empty(t, anvil)
	assert.Empty(t, quest)
}

// TestQuestBeadOwner_TakesTheFirstOfEachLine: the error text a quest failure
// carries is arbitrary, and a step message reading "Anvil: something" must not
// re-attribute the bead.
func TestQuestBeadOwner_TakesTheFirstOfEachLine(t *testing.T) {
	desc := questBeadDescription("explorer",
		&Quest{Name: "login", FilePath: ".quests/login.yaml"},
		&QuestResult{FailedStep: 1, ErrorMessage: "Anvil: munin"}, "click")

	anvil, quest := questBeadOwner(desc)
	assert.Equal(t, "explorer", anvil)
	assert.Equal(t, "login", quest)
}
