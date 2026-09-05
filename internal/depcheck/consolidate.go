package depcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// consolidatedBeadTitlePrefix is the common prefix shared by all consolidated
// dependency update bead titles. It narrows a listing to plausible candidates;
// it never decides which anvil a candidate belongs to. See findConsolidatedBead.
const consolidatedBeadTitlePrefix = "Package updates"

// consolidatedBeadDescriptionPrefix opens the description of every consolidated
// bead depcheck writes, and is followed by the anvil name and a colon. It is the
// only anvil marker legacy beads carry, so it is what the adoption path in
// consolidatedBeadLookup.resolve matches on.
const consolidatedBeadDescriptionPrefix = "Automated dependency updates for "

// consolidatedBeadTitle returns the standardized title for a per-anvil consolidated
// dependency update bead. Format: "Package updates starting DD.MM.YYYY"
func consolidatedBeadTitle(t time.Time) string {
	return fmt.Sprintf("%s starting %s", consolidatedBeadTitlePrefix, t.Format("02.01.2006"))
}

// consolidatedBeadHeader renders the first line of an anvil's consolidated bead
// description.
func consolidatedBeadHeader(anvil string) string {
	return consolidatedBeadDescriptionPrefix + anvil + ":"
}

// descriptionOwner reads the anvil name out of a consolidated bead description,
// or returns "" when the description does not open with the header depcheck
// writes — in which case no anvil claims the bead, which is the safe answer: a
// duplicate bead is a nuisance, adopting another repository's package list is a
// dispatch into a checkout where half the manifests do not exist.
func descriptionOwner(desc string) string {
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rest, ok := strings.CutPrefix(line, consolidatedBeadDescriptionPrefix)
		if !ok {
			return ""
		}
		name, ok := strings.CutSuffix(rest, ":")
		if !ok {
			return ""
		}
		return strings.TrimSpace(name)
	}
	return ""
}

// ecoKey normalises an ecosystem name to a lowercase short identifier used
// in the bead description format.
func ecoKey(ecosystem string) string {
	switch strings.ToLower(ecosystem) {
	case "nuget", ".net":
		return "nuget"
	default:
		return strings.ToLower(ecosystem)
	}
}

// formatPkgEntries formats a slice of updates as "pkg1 cur→lat, pkg2 cur→lat, ...".
func formatPkgEntries(updates []ModuleUpdate) string {
	parts := make([]string, len(updates))
	for i, u := range updates {
		parts[i] = fmt.Sprintf("%s %s→%s", u.Path, u.Current, u.Latest)
	}
	return strings.Join(parts, ", ")
}

// buildConsolidatedDescription builds a bead description from all ecosystem results.
// Patch/minor updates appear first (one line per ecosystem), followed by an optional
// "Major updates" section.
func buildConsolidatedDescription(anvil string, allResults []*CheckResult) string {
	autoByEco, majorByEco := collectUpdates(allResults)
	return buildDescriptionFromMaps(anvil, autoByEco, majorByEco)
}

// collectUpdates groups all updates from results into ecosystem-keyed maps of
// package-path → ModuleUpdate. Patch/minor updates go into the first return
// value; major updates go into the second.
func collectUpdates(allResults []*CheckResult) (auto, major map[string]map[string]ModuleUpdate) {
	auto = make(map[string]map[string]ModuleUpdate)
	major = make(map[string]map[string]ModuleUpdate)
	for _, r := range allResults {
		if r == nil || r.Error != nil {
			continue
		}
		eco := ecoKey(r.Ecosystem)
		for _, u := range append(r.Patch, r.Minor...) {
			if auto[eco] == nil {
				auto[eco] = make(map[string]ModuleUpdate)
			}
			auto[eco][u.Path] = u
		}
		for _, u := range r.Major {
			if major[eco] == nil {
				major[eco] = make(map[string]ModuleUpdate)
			}
			major[eco][u.Path] = u
		}
	}
	return
}

// buildDescriptionFromMaps renders ecosystem-keyed package maps into a bead description.
func buildDescriptionFromMaps(anvil string, autoByEco, majorByEco map[string]map[string]ModuleUpdate) string {
	ecos := orderedEcoKeys(autoByEco, majorByEco)

	var sb strings.Builder
	sb.WriteString(consolidatedBeadHeader(anvil) + "\n\n")

	for _, eco := range ecos {
		if len(autoByEco[eco]) == 0 {
			continue
		}
		pkgs := mapToSlice(autoByEco[eco])
		sortUpdates(pkgs)
		sb.WriteString(eco)
		sb.WriteString(": ")
		sb.WriteString(formatPkgEntries(pkgs))
		sb.WriteString("\n")
	}

	var hasMajor bool
	for _, eco := range ecos {
		if len(majorByEco[eco]) > 0 {
			hasMajor = true
			break
		}
	}
	if hasMajor {
		sb.WriteString("\nMajor updates (require manual review):\n")
		for _, eco := range ecos {
			if len(majorByEco[eco]) == 0 {
				continue
			}
			pkgs := mapToSlice(majorByEco[eco])
			sortUpdates(pkgs)
			sb.WriteString(eco)
			sb.WriteString(": ")
			sb.WriteString(formatPkgEntries(pkgs))
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// orderedEcoKeys returns all ecosystem keys present in either map, in deterministic
// order: go, npm, nuget first, then any others alphabetically.
func orderedEcoKeys(maps ...map[string]map[string]ModuleUpdate) []string {
	seen := make(map[string]bool)
	for _, m := range maps {
		for eco := range m {
			seen[eco] = true
		}
	}

	priority := []string{"go", "npm", "nuget"}
	var ecos []string
	inPriority := map[string]bool{}
	for _, e := range priority {
		if seen[e] {
			ecos = append(ecos, e)
			inPriority[e] = true
		}
	}
	var extras []string
	for e := range seen {
		if !inPriority[e] {
			extras = append(extras, e)
		}
	}
	sort.Strings(extras)
	return append(ecos, extras...)
}

// parsePackageEntry parses a single "pkg cur→lat" entry as produced by formatPkgEntries.
// Returns nil if the entry cannot be parsed.
func parsePackageEntry(entry string) *ModuleUpdate {
	entry = strings.TrimSpace(entry)
	arrowIdx := strings.Index(entry, "→")
	if arrowIdx < 0 {
		return nil
	}
	prefix := strings.TrimSpace(entry[:arrowIdx])
	latest := strings.TrimSpace(entry[arrowIdx+len("→"):])
	spaceIdx := strings.LastIndex(prefix, " ")
	if spaceIdx < 0 {
		return nil
	}
	pkgPath := prefix[:spaceIdx]
	current := prefix[spaceIdx+1:]
	if pkgPath == "" || current == "" || latest == "" {
		return nil
	}
	return &ModuleUpdate{
		Path:    pkgPath,
		Current: current,
		Latest:  latest,
		Kind:    classifyUpdate(current, latest),
	}
}

// parseConsolidatedDescription extracts the package maps from an existing
// consolidated bead description. Returns auto (patch/minor) and major maps,
// each keyed by ecosystem → package-path → ModuleUpdate.
func parseConsolidatedDescription(desc string) (auto, major map[string]map[string]ModuleUpdate) {
	auto = make(map[string]map[string]ModuleUpdate)
	major = make(map[string]map[string]ModuleUpdate)
	inMajor := false
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Major updates") {
			inMajor = true
			continue
		}
		colonIdx := strings.Index(line, ": ")
		if colonIdx <= 0 {
			continue
		}
		eco := line[:colonIdx]
		rest := line[colonIdx+2:]

		target := auto
		if inMajor {
			target = major
		}
		if target[eco] == nil {
			target[eco] = make(map[string]ModuleUpdate)
		}
		for _, rawEntry := range strings.Split(rest, ", ") {
			u := parsePackageEntry(rawEntry)
			if u != nil {
				target[eco][u.Path] = *u
			}
		}
	}
	return
}

// mergeConsolidatedPackages merges new scan results into existing package maps.
// New packages take precedence over existing ones with the same package path.
func mergeConsolidatedPackages(
	existingAuto, existingMajor map[string]map[string]ModuleUpdate,
	allResults []*CheckResult,
) (auto, major map[string]map[string]ModuleUpdate) {
	// Start with deep copies of existing data.
	auto = make(map[string]map[string]ModuleUpdate)
	for eco, pkgs := range existingAuto {
		auto[eco] = make(map[string]ModuleUpdate)
		for p, u := range pkgs {
			auto[eco][p] = u
		}
	}
	major = make(map[string]map[string]ModuleUpdate)
	for eco, pkgs := range existingMajor {
		major[eco] = make(map[string]ModuleUpdate)
		for p, u := range pkgs {
			major[eco][p] = u
		}
	}

	// Merge in new results (new wins on conflict).
	for _, r := range allResults {
		if r == nil || r.Error != nil {
			continue
		}
		eco := ecoKey(r.Ecosystem)
		for _, u := range append(r.Patch, r.Minor...) {
			if auto[eco] == nil {
				auto[eco] = make(map[string]ModuleUpdate)
			}
			auto[eco][u.Path] = u
		}
		for _, u := range r.Major {
			if major[eco] == nil {
				major[eco] = make(map[string]ModuleUpdate)
			}
			major[eco][u.Path] = u
		}
	}
	return
}

// mapToSlice converts a map[string]ModuleUpdate to a []ModuleUpdate.
func mapToSlice(m map[string]ModuleUpdate) []ModuleUpdate {
	result := make([]ModuleUpdate, 0, len(m))
	for _, u := range m {
		result = append(result, u)
	}
	return result
}

// fetchSQL runs a bd sql query and returns the raw JSON output.
func fetchSQL(ctx context.Context, anvilPath, query string) ([]byte, error) {
	cmd, cancel := executil.BdCommand(ctx, "sql", "--json", query)
	defer cancel()
	cmd.Dir = anvilPath
	return cmd.Output()
}

// beadOwnerStore records which bead holds an anvil's consolidated dependency
// updates. It is the subset of state.DB the lookup needs, so the resolution can
// be driven in a test without a SQLite file. See state.DB.ConsolidatedBead for
// why the answer is stored rather than re-derived from the pool every scan.
type beadOwnerStore interface {
	ConsolidatedBead(anvil string) (string, error)
	SetConsolidatedBead(anvil, beadID string) error
	ClearConsolidatedBead(anvil string) error
}

// consolidatedBeadLookup resolves one anvil's consolidated bead. It is a struct
// rather than a function because the two ways it reaches beads — one bead by
// id, and the open listing — are what a test replaces.
type consolidatedBeadLookup struct {
	anvil  string
	owners beadOwnerStore
	// showBead returns one bead by id. (nil, nil) means bd answered that the
	// bead does not exist; an error means no answer was obtained at all. The
	// two must stay distinct — see pinned.
	showBead func(id string) (*bdBead, error)
	// openBeads returns the open beads that could be this anvil's.
	openBeads func() ([]bdBead, error)
}

// resolve returns the anvil's consolidated bead, or (nil, nil) when it has none.
//
// Ownership is pinned by bead id, not matched by title: a beads pool can hold
// two anvils (Munin and Explorer share one deliberately), and every anvil's
// title starts with the same prefix, so a title match hands each anvil whichever
// bead was touched last and the two overwrite each other every cycle. A title is
// also the field a human tidies, and a lookup keyed on it forks the bead the
// first time someone does.
//
// The listing scan is the adoption path, and it runs only until the anvil has a
// pin: for a bead created before ownership was recorded, or after the state
// database is lost. It reads the anvil out of the description header, which is
// the marker those beads already carry.
func (l consolidatedBeadLookup) resolve() (*bdBead, error) {
	pin, err := l.pinned()
	if err != nil {
		return nil, err
	}
	if pin != nil {
		return pin, nil
	}

	beads, err := l.openBeads()
	if err != nil {
		return nil, err
	}
	candidates := make([]bdBead, 0, len(beads))
	for i := range beads {
		b := beads[i]
		if !strings.HasPrefix(b.Title, consolidatedBeadTitlePrefix) {
			continue
		}
		// A listing that omits descriptions would leave every candidate
		// ownerless, and an ownerless candidate is adopted by nobody — so fill
		// them in rather than let a thin listing duplicate the bead.
		if b.Description == "" && l.showBead != nil {
			full, err := l.showBead(b.ID)
			if err != nil {
				return nil, fmt.Errorf("reading candidate bead %s: %w", b.ID, err)
			}
			if full != nil && full.ID != "" {
				b = *full
			}
		}
		candidates = append(candidates, b)
	}

	found := selectConsolidatedBead(candidates, l.anvil)
	if found == nil {
		return nil, nil
	}
	log.Printf("[depcheck] %s: adopting existing consolidated bead %s", l.anvil, found.ID)
	l.remember(found.ID)
	return found, nil
}

// pinned returns the recorded bead when it still exists and is still open,
// clearing the record when bd answers that it does not. The record is
// deliberately not checked against the title: the pin is the bead's identity,
// so a retitled bead stays this anvil's bead.
//
// A failure to READ the pinned bead is an error, not a missing bead. Dropping
// the pin on a timeout would send the same run on to create a second bead for
// the anvil, which is the outcome the pin exists to prevent; the caller skips
// the cycle instead and the pin survives to be resolved next time.
func (l consolidatedBeadLookup) pinned() (*bdBead, error) {
	if l.owners == nil || l.showBead == nil {
		return nil, nil
	}
	id, err := l.owners.ConsolidatedBead(l.anvil)
	if err != nil {
		// A Forge-local read failure: fall through to the header scan rather
		// than hold up the anvil's dependency updates on it.
		log.Printf("[depcheck] %s: could not read the recorded consolidated bead: %v", l.anvil, err)
		return nil, nil
	}
	if id == "" {
		return nil, nil
	}
	b, err := l.showBead(id)
	if err != nil {
		return nil, fmt.Errorf("reading recorded consolidated bead %s: %w", id, err)
	}
	if b == nil || b.ID == "" || b.Status != "open" {
		l.forget()
		return nil, nil
	}
	return b, nil
}

func (l consolidatedBeadLookup) remember(beadID string) {
	if l.owners == nil || beadID == "" {
		return
	}
	if err := l.owners.SetConsolidatedBead(l.anvil, beadID); err != nil {
		log.Printf("[depcheck] %s: could not record consolidated bead %s: %v", l.anvil, beadID, err)
	}
}

func (l consolidatedBeadLookup) forget() {
	if l.owners == nil {
		return
	}
	if err := l.owners.ClearConsolidatedBead(l.anvil); err != nil {
		log.Printf("[depcheck] %s: could not clear the recorded consolidated bead: %v", l.anvil, err)
	}
}

// selectConsolidatedBead picks the anvil's consolidated bead out of a set of
// open beads, most recently updated first. A bead the header does not attribute
// to this anvil is never returned, whoever else it belongs to.
func selectConsolidatedBead(beads []bdBead, anvil string) *bdBead {
	if anvil == "" {
		return nil
	}
	var matches []bdBead
	for i := range beads {
		if !strings.HasPrefix(beads[i].Title, consolidatedBeadTitlePrefix) {
			continue
		}
		if !strings.EqualFold(descriptionOwner(beads[i].Description), anvil) {
			continue
		}
		matches = append(matches, beads[i])
	}
	if len(matches) == 0 {
		return nil
	}
	if len(matches) > 1 {
		// Unparseable and empty timestamps sort as oldest.
		sort.SliceStable(matches, func(i, j int) bool {
			ti, _ := parseBeadTime(matches[i].UpdatedAt)
			tj, _ := parseBeadTime(matches[j].UpdatedAt)
			return ti.After(tj)
		})
		log.Printf("[depcheck] %s: found %d open consolidated beads — picking most recently updated (%s)",
			anvil, len(matches), matches[0].ID)
	}
	return &matches[0]
}

// findConsolidatedBead returns the consolidated dependency-update bead belonging
// to anvilName, or (nil, nil) when it has none. The returned bead's Description
// is populated. See consolidatedBeadLookup.resolve for how ownership is decided.
func findConsolidatedBead(ctx context.Context, owners beadOwnerStore, anvilPath, anvilName string) (*bdBead, error) {
	return consolidatedBeadLookup{
		anvil:  anvilName,
		owners: owners,
		showBead: func(id string) (*bdBead, error) {
			return showBead(ctx, anvilPath, id)
		},
		openBeads: func() ([]bdBead, error) {
			return fetchConsolidatedCandidates(ctx, anvilPath, anvilName)
		},
	}.resolve()
}

// showBead reads one bead by id, separating "bd says it does not exist" from
// "bd did not answer". fetchBeadShow discards its exec error and its callers
// read an unreadable bead as an absent one; the pinned lookup cannot, because
// forgetting a pin on a timeout is how an anvil ends up with a second bead.
func showBead(ctx context.Context, anvilPath, beadID string) (*bdBead, error) {
	out := fetchBeadShow(ctx, anvilPath, beadID)
	if b := executil.DecodeOneBead(out, func(b bdBead) string { return b.ID }); b != nil {
		return b, nil
	}
	if executil.BdReportsNoSuchBead(out) {
		return nil, nil
	}
	return nil, fmt.Errorf("bd show %s in %s returned no bead", beadID, anvilPath)
}

// fetchConsolidatedCandidates lists the open beads that could be this anvil's
// consolidated bead. The SQL narrows on the anvil's own description header as
// well as the shared title prefix, so a pool holding two anvils normally does
// not send the other anvil's bead back over the wire. It is only a filter:
// selectConsolidatedBead decides.
func fetchConsolidatedCandidates(ctx context.Context, anvilPath, anvilName string) ([]bdBead, error) {
	if out, err := fetchSQL(ctx, anvilPath, consolidatedCandidatesQuery(anvilName)); err == nil {
		var beads []bdBead
		if json.Unmarshal(out, &beads) == nil {
			return beads, nil
		}
	}

	// Fall back to bd list, which carries no WHERE clause of its own; the
	// filtering all happens in selectConsolidatedBead either way.
	listOut, listErr := fetchBeadList(ctx, anvilPath, "open")
	if listErr != nil {
		return nil, fmt.Errorf("bd list --status=open: %w", listErr)
	}
	var beads []bdBead
	if err := json.Unmarshal(listOut, &beads); err != nil {
		return nil, fmt.Errorf("parse bd list output: %w", err)
	}
	return beads, nil
}

// consolidatedCandidatesQuery narrows the open issues to the ones that could be
// this anvil's consolidated bead. The anvil name goes into a LIKE pattern, so
// its own % and _ are escaped as literals — an anvil named "web_client" would
// otherwise match "webXclient" and pull back a bead it does not own.
func consolidatedCandidatesQuery(anvilName string) string {
	return fmt.Sprintf(
		`SELECT * FROM issues WHERE status = 'open' AND title LIKE '%s%%' ESCAPE '\' AND description LIKE '%s%%' ESCAPE '\' ORDER BY updated_at DESC`,
		likePattern(consolidatedBeadTitlePrefix), likePattern(consolidatedBeadHeader(anvilName)))
}

// likePattern escapes a value for use as a literal inside a single-quoted LIKE
// pattern: SQL string quoting plus the LIKE metacharacters. Pair it with an
// ESCAPE '\' clause, since not every dialect assumes that escape character.
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `'`, `''`)
	return r.Replace(s)
}
