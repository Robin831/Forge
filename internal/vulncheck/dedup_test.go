package vulncheck

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every resolution test below runs against a pool holding BOTH anvils' beads,
// because that is the only shape in which the bug exists: Munin and Explorer
// share one beads database on purpose, two repositories depending on the same
// library hit the same CVE, and with one anvil's bead in the pool a substring
// search returns the right answer by accident.

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

// vulnBead builds a bead in exactly the shape vulncheck writes: the description
// comes from the real writer, so a change to its format breaks these tests
// rather than silently detaching them from production.
func vulnBead(id, anvil, vulnID, status string) bdBead {
	v := ParsedVuln{
		ID:          vulnID,
		Summary:     "denial of service in the parser",
		Severity:    "HIGH",
		AffectedPkg: "github.com/example/parser",
	}
	return bdBead{
		ID:          id,
		Title:       vulnBeadTitlePrefix + vulnID + " — denial of service in the parser",
		Status:      status,
		Description: buildBeadDescription(v, anvil),
	}
}

// lookupOver builds a lookup reading one shared pool, the way
// Scanner.vulnBeadLookupFor does against a real beads workspace.
func lookupOver(anvil, vulnID string, pins beadPinStore, pool []bdBead) vulnBeadLookup {
	return vulnBeadLookup{
		anvil:  anvil,
		vulnID: vulnID,
		pins:   pins,
		showBead: func(id string) (*bdBead, error) {
			for i := range pool {
				if pool[i].ID == id {
					return &pool[i], nil
				}
			}
			return nil, nil // bd's "no such bead" answer
		},
		openBeads: func() ([]bdBead, error) {
			var open []bdBead
			for _, b := range pool {
				if b.Status == "open" {
					open = append(open, b)
				}
			}
			return open, nil
		},
	}
}

// TestResolve_SameCVEInTwoAnvils is the bug: two repositories depend on the same
// library, both are exposed to one CVE, and the second anvil to scan found the
// first's bead in the raw JSON and created nothing — so one exposure went
// unreported.
func TestResolve_SameCVEInTwoAnvils(t *testing.T) {
	pool := []bdBead{
		vulnBead("bd-munin", "munin", "GO-2026-1234", "open"),
		vulnBead("bd-explorer", "explorer", "GO-2026-1234", "open"),
	}
	pins := newFakePins()

	munin, err := lookupOver("munin", "GO-2026-1234", pins, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, munin)
	assert.Equal(t, "bd-munin", munin.ID)

	explorer, err := lookupOver("explorer", "GO-2026-1234", pins, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, explorer)
	assert.Equal(t, "bd-explorer", explorer.ID)
}

// TestResolve_OtherAnvilsBeadIsNotAdopted: the pool holds only the OTHER anvil's
// bead for this CVE. Explorer must report "no bead" so its own exposure gets
// one; under the substring search it silently got nothing.
func TestResolve_OtherAnvilsBeadIsNotAdopted(t *testing.T) {
	pool := []bdBead{vulnBead("bd-munin", "munin", "GO-2026-1234", "open")}
	pins := newFakePins()

	got, err := lookupOver("explorer", "GO-2026-1234", pins, pool).resolve()
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, pins.pins, "a bead this anvil does not own must not be pinned to it")
}

// TestResolve_ShorterIDIsNotASubstringMatch: GO-2026-1234 is a substring of
// GO-2026-12345, so the raw search reported the wrong CVE's bead as this one's.
// One anvil is enough to show it; the pool still holds the other's.
func TestResolve_ShorterIDIsNotASubstringMatch(t *testing.T) {
	pool := []bdBead{
		vulnBead("bd-munin-long", "munin", "GO-2026-12345", "open"),
		vulnBead("bd-explorer", "explorer", "GO-2026-1234", "open"),
	}
	pins := newFakePins()

	got, err := lookupOver("munin", "GO-2026-1234", pins, pool).resolve()
	require.NoError(t, err)
	assert.Nil(t, got, "GO-2026-1234 must not match GO-2026-12345's bead")
}

// TestResolve_APassingMentionIsNotABead: an unrelated bead quoting a CVE id in
// prose suppressed the real one under the substring search.
func TestResolve_APassingMentionIsNotABead(t *testing.T) {
	chatter := bdBead{
		ID:          "bd-chatter",
		Title:       "Review the dependency policy",
		Status:      "open",
		Description: "We should also check whether GO-2026-1234 affects us.",
	}
	pool := []bdBead{chatter, vulnBead("bd-munin", "munin", "GO-2026-9999", "open")}

	got, err := lookupOver("explorer", "GO-2026-1234", newFakePins(), pool).resolve()
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestResolve_AdoptsThisAnvilsPreFixBead: vulncheck already wrote the anvil into
// every description, so a bead created before the pin shipped is adopted rather
// than duplicated.
func TestResolve_AdoptsThisAnvilsPreFixBead(t *testing.T) {
	pool := []bdBead{
		vulnBead("bd-munin", "munin", "GO-2026-1234", "open"),
		vulnBead("bd-explorer", "explorer", "GO-2026-1234", "open"),
	}
	pins := newFakePins() // nothing recorded before the fix shipped

	got, err := lookupOver("explorer", "GO-2026-1234", pins, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "bd-explorer", got.ID)
	assert.Equal(t, "bd-explorer", pins.pins["explorer/vulncheck/GO-2026-1234"])
}

// TestResolve_PinnedBeadSurvivesARetitle: the pin is the bead's identity, so a
// retitled bead is still this anvil's bead for this CVE.
func TestResolve_PinnedBeadSurvivesARetitle(t *testing.T) {
	retitled := vulnBead("bd-explorer", "explorer", "GO-2026-1234", "open")
	retitled.Title = "Parser DoS — waiting on upstream"
	pool := []bdBead{vulnBead("bd-munin", "munin", "GO-2026-1234", "open"), retitled}
	pins := newFakePins()
	require.NoError(t, pins.SetAnvilBead("explorer", vulnBeadKind, "GO-2026-1234", "bd-explorer"))

	got, err := lookupOver("explorer", "GO-2026-1234", pins, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "bd-explorer", got.ID)
}

// TestResolve_ForgetsAClosedBead: a vulnerability that reappears after its bead
// was closed must be reported again — the property the old open-only listing
// had, kept.
func TestResolve_ForgetsAClosedBead(t *testing.T) {
	pool := []bdBead{
		vulnBead("bd-munin", "munin", "GO-2026-1234", "open"),
		vulnBead("bd-explorer", "explorer", "GO-2026-1234", "closed"),
	}
	pins := newFakePins()
	require.NoError(t, pins.SetAnvilBead("explorer", vulnBeadKind, "GO-2026-1234", "bd-explorer"))

	got, err := lookupOver("explorer", "GO-2026-1234", pins, pool).resolve()
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, pins.pins)
}

// TestResolve_KeepsThePinWhenTheBeadCannotBeRead: an unreadable PINNED bead is
// not an absent one — Forge recorded creating it, so a timeout is no reason to
// file a second.
func TestResolve_KeepsThePinWhenTheBeadCannotBeRead(t *testing.T) {
	pins := newFakePins()
	require.NoError(t, pins.SetAnvilBead("explorer", vulnBeadKind, "GO-2026-1234", "bd-explorer"))

	l := lookupOver("explorer", "GO-2026-1234", pins,
		[]bdBead{vulnBead("bd-munin", "munin", "GO-2026-1234", "open")})
	l.showBead = func(string) (*bdBead, error) { return nil, errors.New("bd timed out") }

	got, err := l.resolve()
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Equal(t, "bd-explorer", pins.pins["explorer/vulncheck/GO-2026-1234"])
}

// TestResolve_UnreadableListingStillCreates: with no pin there is no evidence a
// bead exists, and a security finding is reported rather than sat on. This is
// the old fail-open behaviour, kept for exactly the case it was written for.
func TestResolve_UnreadableListingStillCreates(t *testing.T) {
	l := lookupOver("explorer", "GO-2026-1234", newFakePins(), nil)
	l.openBeads = func() ([]bdBead, error) { return nil, errors.New("bd list failed") }

	got, err := l.resolve()
	require.NoError(t, err)
	assert.Nil(t, got, "an unknown listing must not suppress a security bead")
}

// TestVulnBeadOwnerRoundTripsTheWriter pins the reader to the writer: adoption
// only works while buildBeadDescription keeps emitting the two lines
// vulnBeadOwner reads.
func TestVulnBeadOwnerRoundTripsTheWriter(t *testing.T) {
	desc := buildBeadDescription(ParsedVuln{
		ID:          "GHSA-abcd-1234-wxyz",
		Summary:     "path traversal",
		Severity:    "CRITICAL",
		AffectedPkg: "github.com/example/fs",
	}, "fhi.munin.explorer")

	anvil, vulnID := vulnBeadOwner(desc)
	assert.Equal(t, "fhi.munin.explorer", anvil)
	assert.Equal(t, "GHSA-abcd-1234-wxyz", vulnID)
}

func TestVulnBeadOwner_MissingLines(t *testing.T) {
	anvil, vulnID := vulnBeadOwner("Just some prose about GO-2026-1234.")
	assert.Empty(t, anvil)
	assert.Empty(t, vulnID)
}
