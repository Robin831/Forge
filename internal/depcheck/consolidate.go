package depcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// consolidatedBeadTitlePrefix is the common prefix shared by all consolidated
// dependency update bead titles. Used for prefix-based lookup so that beads
// created on a previous day are reused rather than duplicated.
const consolidatedBeadTitlePrefix = "Package updates"

// consolidatedBeadTitle returns the standardized title for a per-anvil consolidated
// dependency update bead. Format: "Package updates starting DD.MM.YYYY"
func consolidatedBeadTitle(t time.Time) string {
	return fmt.Sprintf("%s starting %s", consolidatedBeadTitlePrefix, t.Format("02.01.2006"))
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
	sb.WriteString(fmt.Sprintf("Automated dependency updates for %s:\n\n", anvil))

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
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "sql", "--json", query))
	cmd.Dir = anvilPath
	return cmd.Output()
}

// findConsolidatedBead searches for any open bead whose title starts with
// consolidatedBeadTitlePrefix ("Package updates"). This prefix-based match
// ensures that a bead created on a previous day is reused instead of a new
// duplicate being created when the date changes.
//
// Returns (nil, nil) if no such bead exists, or (bead, nil) if found.
// The returned bead's Description field is populated (via bd show if needed).
func findConsolidatedBead(ctx context.Context, anvilPath string) (*bdBead, error) {
	// Try bd sql first: fast and returns all columns including description.
	escaped := strings.ReplaceAll(consolidatedBeadTitlePrefix, "'", "''")
	query := fmt.Sprintf(`SELECT * FROM issues WHERE status = 'open' AND title LIKE '%s%%' ORDER BY updated_at DESC`, escaped)
	out, err := fetchSQL(ctx, anvilPath, query)
	if err == nil {
		var beads []bdBead
		if json.Unmarshal(out, &beads) == nil {
			var matches []bdBead
			for i := range beads {
				if strings.HasPrefix(beads[i].Title, consolidatedBeadTitlePrefix) {
					matches = append(matches, beads[i])
				}
			}
			if len(matches) == 0 {
				// Query succeeded with no match.
				return nil, nil
			}
			if len(matches) > 1 {
				log.Printf("[depcheck] %s: found %d open consolidated beads — picking most recently updated (%s)", anvilPath, len(matches), matches[0].ID)
			}
			// matches[0] is the most recently updated due to ORDER BY updated_at DESC.
			return &matches[0], nil
		}
	}

	// Fall back to bd list + bd show.
	listOut, listErr := fetchBeadList(ctx, anvilPath, "open")
	if listErr != nil {
		return nil, fmt.Errorf("bd list --status=open: %w", listErr)
	}
	var beads []bdBead
	if err := json.Unmarshal(listOut, &beads); err != nil {
		return nil, fmt.Errorf("parse bd list output: %w", err)
	}
	var matches []bdBead
	for i := range beads {
		if strings.HasPrefix(beads[i].Title, consolidatedBeadTitlePrefix) {
			matches = append(matches, beads[i])
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > 1 {
		log.Printf("[depcheck] %s: found %d open consolidated beads — picking most recently updated by updated_at", anvilPath, len(matches))
		// Sort by updated_at descending to pick the most recently updated bead.
		sort.Slice(matches, func(i, j int) bool {
			return matches[i].UpdatedAt > matches[j].UpdatedAt
		})
	}
	// Fetch full details (including description) via bd show for the chosen bead.
	if fullOut := fetchBeadShow(ctx, anvilPath, matches[0].ID); len(fullOut) > 0 {
		var full bdBead
		if json.Unmarshal(fullOut, &full) == nil {
			return &full, nil
		}
	}
	return &matches[0], nil
}
