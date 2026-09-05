package depcheck

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsolidatedBeadTitle(t *testing.T) {
	d := time.Date(2026, 3, 27, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, "Package updates starting 27.03.2026", consolidatedBeadTitle(d))
}

func TestConsolidatedBeadTitle_LeadingZero(t *testing.T) {
	d := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, "Package updates starting 05.01.2026", consolidatedBeadTitle(d))
}

func TestEcoKey(t *testing.T) {
	assert.Equal(t, "go", ecoKey("Go"))
	assert.Equal(t, "npm", ecoKey("npm"))
	assert.Equal(t, "nuget", ecoKey("NuGet"))
	assert.Equal(t, "nuget", ecoKey(".NET"))
	assert.Equal(t, "gradle", ecoKey("Gradle"))
}

func TestFormatPkgEntries(t *testing.T) {
	updates := []ModuleUpdate{
		{Path: "react", Current: "18.2.0", Latest: "19.0.0"},
		{Path: "express", Current: "4.0.0", Latest: "5.0.0"},
	}
	got := formatPkgEntries(updates)
	assert.Equal(t, "react 18.2.0→19.0.0, express 4.0.0→5.0.0", got)
}

func TestBuildConsolidatedDescription_NpmAndNuget(t *testing.T) {
	results := []*CheckResult{
		{
			Ecosystem: "npm",
			Anvil:     "myanvil",
			Patch:     []ModuleUpdate{{Path: "express", Current: "4.18.0", Latest: "4.19.0", Kind: "patch"}},
			Minor:     []ModuleUpdate{{Path: "react", Current: "18.2.0", Latest: "18.3.0", Kind: "minor"}},
		},
		{
			Ecosystem: "NuGet",
			Anvil:     "myavil",
			Patch:     []ModuleUpdate{{Path: "Newtonsoft.Json", Current: "12.0.3", Latest: "12.0.4", Kind: "patch"}},
		},
	}
	desc := buildConsolidatedDescription("myanvil", results)

	assert.Contains(t, desc, "Automated dependency updates for myanvil:")
	assert.Contains(t, desc, "npm: ")
	assert.Contains(t, desc, "nuget: Newtonsoft.Json 12.0.3→12.0.4")
	assert.Contains(t, desc, "react 18.2.0→18.3.0")
	assert.Contains(t, desc, "express 4.18.0→4.19.0")
	assert.NotContains(t, desc, "Major updates")
}

func TestBuildConsolidatedDescription_MixedWithMajor(t *testing.T) {
	results := []*CheckResult{
		{
			Ecosystem: "npm",
			Minor:     []ModuleUpdate{{Path: "lodash", Current: "3.10.1", Latest: "3.10.2", Kind: "minor"}},
			Major:     []ModuleUpdate{{Path: "webpack", Current: "4.0.0", Latest: "5.0.0", Kind: "major"}},
		},
	}
	desc := buildConsolidatedDescription("testrepo", results)

	assert.Contains(t, desc, "npm: lodash 3.10.1→3.10.2")
	assert.Contains(t, desc, "Major updates (require manual review):")
	assert.Contains(t, desc, "npm: webpack 4.0.0→5.0.0")
}

func TestBuildConsolidatedDescription_GoNpmNuget(t *testing.T) {
	// All three ecosystems land in the same bead, in go/npm/nuget order.
	results := []*CheckResult{
		{Ecosystem: "NuGet", Patch: []ModuleUpdate{{Path: "Foo", Current: "1.0", Latest: "1.1", Kind: "patch"}}},
		{Ecosystem: "npm", Patch: []ModuleUpdate{{Path: "bar", Current: "2.0.0", Latest: "2.0.1", Kind: "patch"}}},
		{Ecosystem: "Go", Patch: []ModuleUpdate{{Path: "golang.org/x/net", Current: "v0.1.0", Latest: "v0.2.0", Kind: "patch"}}},
	}
	desc := buildConsolidatedDescription("myrepo", results)

	goIdx := strings.Index(desc, "go:")
	npmIdx := strings.Index(desc, "npm:")
	nugetIdx := strings.Index(desc, "nuget:")

	require.True(t, goIdx >= 0, "go section missing")
	require.True(t, npmIdx >= 0, "npm section missing")
	require.True(t, nugetIdx >= 0, "nuget section missing")

	// go before npm before nuget.
	assert.Less(t, goIdx, npmIdx)
	assert.Less(t, npmIdx, nugetIdx)
}

func TestParsePackageEntry(t *testing.T) {
	tests := []struct {
		input   string
		wantPkg string
		wantCur string
		wantLat string
	}{
		{"react 18.2.0→19.0.0", "react", "18.2.0", "19.0.0"},
		{"golang.org/x/net v0.1.0→v0.2.0", "golang.org/x/net", "v0.1.0", "v0.2.0"},
		{"Newtonsoft.Json 12.0.3→12.0.4", "Newtonsoft.Json", "12.0.3", "12.0.4"},
		{"  lodash 4.17.20→4.17.21  ", "lodash", "4.17.20", "4.17.21"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parsePackageEntry(tt.input)
			require.NotNil(t, got)
			assert.Equal(t, tt.wantPkg, got.Path)
			assert.Equal(t, tt.wantCur, got.Current)
			assert.Equal(t, tt.wantLat, got.Latest)
		})
	}
}

func TestParsePackageEntry_Invalid(t *testing.T) {
	assert.Nil(t, parsePackageEntry(""))
	assert.Nil(t, parsePackageEntry("no-arrow-here"))
	assert.Nil(t, parsePackageEntry("→only-latest"))
}

func TestParseConsolidatedDescription_RoundTrip(t *testing.T) {
	results := []*CheckResult{
		{
			Ecosystem: "npm",
			Patch:     []ModuleUpdate{{Path: "react", Current: "18.0.0", Latest: "18.1.0", Kind: "patch"}},
			Major:     []ModuleUpdate{{Path: "webpack", Current: "4.0.0", Latest: "5.0.0", Kind: "major"}},
		},
		{
			Ecosystem: "NuGet",
			Patch:     []ModuleUpdate{{Path: "Foo.Bar", Current: "2.0.0", Latest: "2.1.0", Kind: "minor"}},
		},
	}
	desc := buildConsolidatedDescription("myrepo", results)

	auto, major := parseConsolidatedDescription(desc)

	require.Contains(t, auto, "npm")
	require.Contains(t, auto["npm"], "react")
	assert.Equal(t, "18.0.0", auto["npm"]["react"].Current)
	assert.Equal(t, "18.1.0", auto["npm"]["react"].Latest)

	require.Contains(t, auto, "nuget")
	require.Contains(t, auto["nuget"], "Foo.Bar")

	require.Contains(t, major, "npm")
	require.Contains(t, major["npm"], "webpack")
	assert.Equal(t, "4.0.0", major["npm"]["webpack"].Current)
}

func TestMergeConsolidatedPackages_NewPackageAdded(t *testing.T) {
	existingAuto := map[string]map[string]ModuleUpdate{
		"npm": {"react": {Path: "react", Current: "18.0.0", Latest: "18.1.0", Kind: "patch"}},
	}
	existingMajor := map[string]map[string]ModuleUpdate{}

	newResults := []*CheckResult{
		{
			Ecosystem: "npm",
			Patch:     []ModuleUpdate{{Path: "express", Current: "4.18.0", Latest: "4.19.0", Kind: "patch"}},
		},
	}
	mergedAuto, mergedMajor := mergeConsolidatedPackages(existingAuto, existingMajor, newResults)

	assert.Contains(t, mergedAuto["npm"], "react")
	assert.Contains(t, mergedAuto["npm"], "express")
	assert.Empty(t, mergedMajor)
}

func TestMergeConsolidatedPackages_NewVersionWins(t *testing.T) {
	existingAuto := map[string]map[string]ModuleUpdate{
		"npm": {"react": {Path: "react", Current: "18.0.0", Latest: "18.1.0", Kind: "patch"}},
	}
	existingMajor := map[string]map[string]ModuleUpdate{}

	// Same package but with a newer target version.
	newResults := []*CheckResult{
		{
			Ecosystem: "npm",
			Minor:     []ModuleUpdate{{Path: "react", Current: "18.0.0", Latest: "18.3.0", Kind: "minor"}},
		},
	}
	mergedAuto, _ := mergeConsolidatedPackages(existingAuto, existingMajor, newResults)

	assert.Equal(t, "18.3.0", mergedAuto["npm"]["react"].Latest)
}

func TestMergeConsolidatedPackages_NpmAndNugetSameBead(t *testing.T) {
	existingAuto := map[string]map[string]ModuleUpdate{
		"npm": {"react": {Path: "react", Current: "18.0.0", Latest: "18.1.0"}},
	}
	existingMajor := map[string]map[string]ModuleUpdate{}

	newResults := []*CheckResult{
		{
			Ecosystem: "NuGet",
			Patch:     []ModuleUpdate{{Path: "Newtonsoft.Json", Current: "12.0.3", Latest: "12.0.4", Kind: "patch"}},
		},
	}
	mergedAuto, _ := mergeConsolidatedPackages(existingAuto, existingMajor, newResults)

	assert.Contains(t, mergedAuto, "npm")
	assert.Contains(t, mergedAuto, "nuget")
	assert.Contains(t, mergedAuto["nuget"], "Newtonsoft.Json")
}

func TestBuildDescriptionFromMaps_EmptyAutoKeepsMajor(t *testing.T) {
	autoByEco := map[string]map[string]ModuleUpdate{}
	majorByEco := map[string]map[string]ModuleUpdate{
		"npm": {"webpack": {Path: "webpack", Current: "4.0.0", Latest: "5.0.0", Kind: "major"}},
	}
	desc := buildDescriptionFromMaps("myrepo", autoByEco, majorByEco)

	assert.Contains(t, desc, "Major updates (require manual review):")
	assert.Contains(t, desc, "npm: webpack 4.0.0→5.0.0")
}

// TestConsolidatedBeadTitleHasPrefix verifies that generated titles always start with
// the prefix used by findConsolidatedBead to locate existing beads. If this test
// breaks, prefix-based reuse will silently stop working.
func TestConsolidatedBeadTitleHasPrefix(t *testing.T) {
	dates := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
	}
	for _, d := range dates {
		title := consolidatedBeadTitle(d)
		assert.True(t, strings.HasPrefix(title, consolidatedBeadTitlePrefix),
			"title %q does not start with prefix %q", title, consolidatedBeadTitlePrefix)
	}
}

// --- anvil scoping -------------------------------------------------------
//
// Every test below runs against a pool holding MORE THAN ONE anvil's beads,
// because that is the only shape in which the bug exists: Munin and Explorer
// share one beads database on purpose, and with a single anvil's bead in the
// pool a title-prefix lookup returns the right answer by accident.

// fakeOwners is an in-memory beadOwnerStore.
type fakeOwners struct {
	pins    map[string]string
	readErr error
}

func newFakeOwners() *fakeOwners { return &fakeOwners{pins: map[string]string{}} }

func (f *fakeOwners) ConsolidatedBead(anvil string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.pins[anvil], nil
}

func (f *fakeOwners) SetConsolidatedBead(anvil, beadID string) error {
	f.pins[anvil] = beadID
	return nil
}

func (f *fakeOwners) ClearConsolidatedBead(anvil string) error {
	delete(f.pins, anvil)
	return nil
}

// openConsolidatedBead builds a bead in exactly the shape depcheck writes: the
// description comes from the real writer, so a change to the header format
// breaks these tests rather than silently detaching them from production.
func openConsolidatedBead(id, anvil, updatedAt, pkg string) bdBead {
	return bdBead{
		ID:     id,
		Title:  consolidatedBeadTitle(time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)),
		Status: "open",
		Labels: []string{DepsUpdateLabel},
		Description: buildConsolidatedDescription(anvil, []*CheckResult{{
			Ecosystem: "npm",
			Anvil:     anvil,
			Minor:     []ModuleUpdate{{Path: pkg, Current: "1.0.0", Latest: "1.1.0", Kind: "minor"}},
		}}),
		UpdatedAt: updatedAt,
	}
}

// lookupOver builds a lookup reading one shared pool, the way
// findConsolidatedBead does against a real beads workspace.
func lookupOver(anvil string, owners beadOwnerStore, pool []bdBead) consolidatedBeadLookup {
	return consolidatedBeadLookup{
		anvil:  anvil,
		owners: owners,
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

// TestResolve_TwoAnvilsInOneDatabaseKeepSeparateBeads is the case the bug
// produced: both anvils have outdated packages, both scan the same pool, and
// neither may be handed the other's bead.
func TestResolve_TwoAnvilsInOneDatabaseKeepSeparateBeads(t *testing.T) {
	pool := []bdBead{
		openConsolidatedBead("bd-munin", "munin", "2026-09-05T03:17:18Z", "react"),
		openConsolidatedBead("bd-explorer", "explorer", "2026-09-05T04:02:00Z", "astro"),
	}
	owners := newFakeOwners()

	munin, err := lookupOver("munin", owners, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, munin)
	assert.Equal(t, "bd-munin", munin.ID)

	explorer, err := lookupOver("explorer", owners, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, explorer)
	assert.Equal(t, "bd-explorer", explorer.ID,
		"explorer must not be handed munin's bead, whichever was updated most recently")

	assert.Equal(t, map[string]string{"munin": "bd-munin", "explorer": "bd-explorer"}, owners.pins)
}

// TestResolve_OtherAnvilsBeadIsNotAdopted: the pool holds only the OTHER anvil's
// open bead — the state on the first Explorer scan after the fix, with Munin's
// bead already open. Explorer must report "no bead" so it creates its own.
func TestResolve_OtherAnvilsBeadIsNotAdopted(t *testing.T) {
	pool := []bdBead{openConsolidatedBead("bd-munin", "munin", "2026-09-05T03:17:18Z", "react")}
	owners := newFakeOwners()

	got, err := lookupOver("explorer", owners, pool).resolve()
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, owners.pins, "a bead this anvil does not own must not be pinned to it")
}

// TestResolve_AdoptsTheAnvilsPreFixBead: a bead created before ownership was
// recorded is found by the anvil whose packages it holds, not orphaned and
// duplicated on the first run.
func TestResolve_AdoptsTheAnvilsPreFixBead(t *testing.T) {
	pool := []bdBead{openConsolidatedBead("bd-munin", "munin", "2026-09-05T03:17:18Z", "react")}
	owners := newFakeOwners() // nothing was recorded before the fix shipped

	got, err := lookupOver("munin", owners, pool).resolve()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "bd-munin", got.ID)
	assert.Equal(t, "bd-munin", owners.pins["munin"], "adoption must pin the bead so the title stops mattering")
}

// TestResolve_PinnedBeadSurvivesARetitle is the trap in the acceptance criteria:
// a title someone tidies must not fork the bead into a second one. Once pinned,
// the title is not part of the lookup at all.
func TestResolve_PinnedBeadSurvivesARetitle(t *testing.T) {
	bead := openConsolidatedBead("bd-munin", "munin", "2026-09-05T03:17:18Z", "react")
	bead.Title = "Munin: dependency bumps for week 36"

	owners := newFakeOwners()
	owners.pins["munin"] = "bd-munin"

	got, err := lookupOver("munin", owners, []bdBead{bead}).resolve()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "bd-munin", got.ID)
}

// TestResolve_ForgetsAClosedBead: the pin is dropped once the bead is closed, so
// the next scan opens a fresh one instead of updating a closed bead forever.
func TestResolve_ForgetsAClosedBead(t *testing.T) {
	bead := openConsolidatedBead("bd-munin", "munin", "2026-09-05T03:17:18Z", "react")
	bead.Status = "closed"

	owners := newFakeOwners()
	owners.pins["munin"] = "bd-munin"

	got, err := lookupOver("munin", owners, []bdBead{bead}).resolve()
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, owners.pins)
}

// TestResolve_DoesNotAdoptAnUnattributedBead: a description with no header names
// no anvil, so no anvil claims it. A duplicate bead is a nuisance; a merged
// package list dispatched into one checkout is a broken run.
func TestResolve_DoesNotAdoptAnUnattributedBead(t *testing.T) {
	bead := openConsolidatedBead("bd-orphan", "munin", "2026-09-05T03:17:18Z", "react")
	bead.Description = "npm: react 1.0.0→1.1.0\n"

	owners := newFakeOwners()
	for _, anvil := range []string{"munin", "explorer"} {
		got, err := lookupOver(anvil, owners, []bdBead{bead}).resolve()
		require.NoError(t, err)
		assert.Nil(t, got, "%s must not adopt a bead that names no anvil", anvil)
	}
	assert.Empty(t, owners.pins)
}

// TestResolve_FillsAThinListingFromShowBead: a listing that omits descriptions
// would make every candidate look unattributed, which would duplicate the bead
// rather than adopt it.
func TestResolve_FillsAThinListingFromShowBead(t *testing.T) {
	full := openConsolidatedBead("bd-munin", "munin", "2026-09-05T03:17:18Z", "react")
	thin := full
	thin.Description = ""

	owners := newFakeOwners()
	lookup := consolidatedBeadLookup{
		anvil:     "munin",
		owners:    owners,
		showBead:  func(string) (*bdBead, error) { return &full, nil },
		openBeads: func() ([]bdBead, error) { return []bdBead{thin}, nil },
	}

	got, err := lookup.resolve()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "bd-munin", got.ID)
}

// TestResolve_KeepsThePinWhenTheBeadCannotBeRead: an unreadable pin is not an
// absent bead. Forgetting it on a timeout would let the same run go on to create
// a second bead for the anvil — the outcome the pin exists to prevent — so the
// error propagates and the caller skips the cycle instead.
func TestResolve_KeepsThePinWhenTheBeadCannotBeRead(t *testing.T) {
	owners := newFakeOwners()
	owners.pins["munin"] = "bd-munin"

	adopted := false
	lookup := consolidatedBeadLookup{
		anvil:    "munin",
		owners:   owners,
		showBead: func(string) (*bdBead, error) { return nil, errors.New("bd show: context deadline exceeded") },
		openBeads: func() ([]bdBead, error) {
			adopted = true
			return nil, nil
		},
	}

	got, err := lookup.resolve()
	require.Error(t, err)
	assert.Nil(t, got)
	assert.False(t, adopted, "an unreadable pin must not fall through to adoption or creation")
	assert.Equal(t, "bd-munin", owners.pins["munin"], "the pin must survive a failed read")
}

// TestResolve_ForgetsABeadBdReportsAsAbsent: a deleted bead is a definite
// answer, unlike an unreadable one, so the pin is dropped and the anvil opens a
// fresh bead.
func TestResolve_ForgetsABeadBdReportsAsAbsent(t *testing.T) {
	owners := newFakeOwners()
	owners.pins["munin"] = "bd-gone"

	got, err := lookupOver("munin", owners, nil).resolve()
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, owners.pins)
}

// TestBeadNotFound separates bd's "no such bead" answer from every other way a
// bd show can come back empty, which is what makes the pin safe to drop.
func TestBeadNotFound(t *testing.T) {
	assert.True(t, beadNotFound([]byte(`{"error":"no issues found matching the provided IDs","schema_version":1}`)))
	assert.True(t, beadNotFound([]byte(`Error fetching x: no issue found matching "x"`+"\n"+`{"error":"no issue found matching \"x\""}`)))
	assert.False(t, beadNotFound([]byte(`{"error":"dial tcp 127.0.0.1:3306: connection refused"}`)))
	assert.False(t, beadNotFound(nil))
	assert.False(t, beadNotFound([]byte("bd: command not found")))
}

// TestSelectConsolidatedBead_MostRecentOfThisAnvilsWins: duplicates within one
// anvil still collapse to the most recently updated, and the other anvil's newer
// bead does not win the tie-break.
func TestSelectConsolidatedBead_MostRecentOfThisAnvilsWins(t *testing.T) {
	pool := []bdBead{
		openConsolidatedBead("bd-munin-old", "munin", "2026-08-26T03:04:49Z", "react"),
		openConsolidatedBead("bd-munin-new", "munin", "2026-09-05T03:17:18Z", "react"),
		openConsolidatedBead("bd-explorer", "explorer", "2026-09-05T23:59:00Z", "astro"),
	}
	got := selectConsolidatedBead(pool, "munin")
	require.NotNil(t, got)
	assert.Equal(t, "bd-munin-new", got.ID)
}

func TestDescriptionOwner(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want string
	}{
		{"written by depcheck", buildConsolidatedDescription("munin", nil), "munin"},
		{"leading blank lines", "\n\nAutomated dependency updates for munin:\n\nnpm: x 1→2\n", "munin"},
		{"no header", "npm: react 1.0.0→1.1.0\n", ""},
		{"header without colon", "Automated dependency updates for munin\n", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, descriptionOwner(tt.desc))
		})
	}
}

// TestConsolidatedCandidatesQueryIsAnvilScoped: the SQL itself carries the
// anvil, so a shared pool does not even send the other anvil's bead back.
func TestConsolidatedCandidatesQueryIsAnvilScoped(t *testing.T) {
	assert.Contains(t, consolidatedCandidatesQuery("explorer"),
		"description LIKE 'Automated dependency updates for explorer:%'")
	assert.NotContains(t, consolidatedCandidatesQuery("munin"), "explorer")
	assert.Contains(t, consolidatedCandidatesQuery("o'brien"), "for o''brien:", "single quotes are escaped")
	assert.Contains(t, consolidatedCandidatesQuery("web_client"), `for web\_client:`,
		"a LIKE metacharacter in the anvil name must not stay a wildcard")
}

// TestDescriptionOwnerRoundTripsTheWriter: the adoption path reads the header
// buildDescriptionFromMaps writes. If those two ever disagree, every anvil's
// bead is unattributed and gets duplicated instead of updated.
func TestDescriptionOwnerRoundTripsTheWriter(t *testing.T) {
	for _, anvil := range []string{"munin", "explorer", "Forge", "some-anvil_2"} {
		assert.Equal(t, anvil, descriptionOwner(buildDescriptionFromMaps(anvil, nil, nil)))
	}
}
